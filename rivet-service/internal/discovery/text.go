// Package discovery — чистое ядро исследования продукта (change
// add-product-discovery, спека backend/product-discovery): нормализация
// интервью и поиск цитат, нарезка длинных интервью, задания Аналитику для
// конспекта и синтеза, разбор и детерминированная проверка его ответов,
// проверка бэклога и топологический порядок Epic'ов.
//
// Пакет не знает ни базы, ни сети, ни модели: store хранит, оркестратор
// зовёт модель, а здесь решается, что из ответа модели принять.
package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// NormalizeText приводит текст интервью к виду хранения: BOM в начале
// убирается, переводы строк приводятся к `\n`, Unicode к форме NFC.
// Больше ничего не меняется: цитаты должны совпадать с исходным текстом
// дословно.
func NormalizeText(raw string) string {
	s := strings.TrimPrefix(raw, string(rune(0xFEFF)))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return norm.NFC.String(s)
}

// Lines — строки нормализованного текста; номер строки равен индексу + 1.
// Завершающий перевод строки не даёт пустой последней строки.
func Lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// CountLines — число строк текста в нумерации Lines.
func CountLines(text string) int { return len(Lines(text)) }

// TextSHA256 — хэш нормализованного текста (ключ кэша конспекта, набор
// интервью версии исследования).
func TextSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// MinQuoteRunes — минимальная длина цитаты после схлопывания пробелов.
// Цитата «да» нашлась бы почти в любом интервью и ничего не подтверждает.
const MinQuoteRunes = 10

// Located — найденная цитата: точный фрагмент исходного текста, номер
// строки его начала (с единицы) и байтовые границы в тексте.
type Located struct {
	Line  int
	Text  string
	Start int
	End   int
}

// collapsed — текст со схлопнутыми пробельными символами и картой
// обратного отображения: для каждого байта схлопнутого текста — начало и
// конец исходного фрагмента, из которого он получен.
type collapsed struct {
	s     string
	start []int
	end   []int
}

// invisible — символ форматирования (категория Cf: мягкий перенос,
// пробелы нулевой ширины, BOM). Редакторы вставляют их незаметно, а модель
// их не воспроизводит, поэтому при сравнении они пропускаются.
func invisible(r rune) bool { return unicode.Is(unicode.Cf, r) }

// collapse схлопывает каждую серию пробельных символов (включая перевод
// строки и неразрывный пробел) в один пробел ' ' и пропускает невидимые
// символы. Карта отображения указывает на исходный текст, так что
// найденный фрагмент включает пропущенные символы как есть. Края серии
// не обрезаются: этим занимается вызывающий для цитаты.
func collapse(text string) collapsed {
	var b strings.Builder
	b.Grow(len(text))
	c := collapsed{start: make([]int, 0, len(text)), end: make([]int, 0, len(text))}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if invisible(r) {
			i += size
			continue
		}
		if unicode.IsSpace(r) {
			j := i + size
			for j < len(text) {
				r2, s2 := utf8.DecodeRuneInString(text[j:])
				if !unicode.IsSpace(r2) && !invisible(r2) {
					break
				}
				j += s2
			}
			b.WriteByte(' ')
			c.start = append(c.start, i)
			c.end = append(c.end, j)
			i = j
			continue
		}
		b.WriteString(text[i : i+size])
		for k := 0; k < size; k++ {
			c.start = append(c.start, i)
			c.end = append(c.end, i+size)
		}
		i += size
	}
	c.s = b.String()
	return c
}

// normalizeQuote — цитата в форме сравнения: NFC, пробелы схлопнуты,
// края обрезаны.
func normalizeQuote(q string) string {
	return strings.TrimSpace(collapse(norm.NFC.String(q)).s)
}

// FindQuote ищет цитату в нормализованном тексте интервью. Сравнение идёт
// после приведения цитаты к NFC и схлопывания пробельных символов в обоих
// текстах; буквы, регистр и знаки препинания должны совпасть. Если
// вхождений несколько, берётся ближайшее к hintLine (номер строки от
// модели — только подсказка), при равенстве — первое. Возвращает точный
// фрагмент исходного текста и строку его начала.
func FindQuote(text, quote string, hintLine int) (Located, bool) {
	q := normalizeQuote(quote)
	if utf8.RuneCountInString(q) < MinQuoteRunes {
		return Located{}, false
	}
	c := collapse(text)
	var best Located
	found := false
	bestDist := 0
	// Номер строки считается нарастающим итогом: вхождения идут по
	// возрастанию позиции, пересчёт от начала текста дал бы квадрат.
	line, counted := 1, 0
	for from := 0; from <= len(c.s)-len(q); {
		idx := strings.Index(c.s[from:], q)
		if idx < 0 {
			break
		}
		pos := from + idx
		start, end := c.start[pos], c.end[pos+len(q)-1]
		line += strings.Count(text[counted:start], "\n")
		counted = start
		loc := Located{Line: line, Text: text[start:end], Start: start, End: end}
		dist := 0
		if hintLine > 0 {
			dist = abs(loc.Line - hintLine)
		}
		if found && dist >= bestDist && loc.Line > hintLine {
			// Дальше вхождения только удаляются от подсказки.
			break
		}
		if !found || dist < bestDist {
			best, bestDist, found = loc, dist, true
		}
		if hintLine <= 0 {
			break
		}
		_, size := utf8.DecodeRuneInString(c.s[pos:])
		from = pos + size
	}
	return best, found
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// NormalizeTitle — название в форме сравнения на уникальность и
// сопоставления с существующими Epic'ами: NFC, нижний регистр, пробелы
// схлопнуты, «ё» как «е».
func NormalizeTitle(title string) string {
	s := strings.ToLower(normalizeQuote(title))
	return strings.ReplaceAll(s, "ё", "е")
}

// LeakWords — с какой длины дословный фрагмент цитаты в тексте считается
// перенесённой цитатой. Требование, пересказанное своими словами, часто
// совпадает с речью респондента на 6–7 слов («по статусу и по дате
// создания»), поэтому порог 8.
const LeakWords = 8

// leakMaxStopShare — фрагмент, где служебных слов больше этой доли, не
// считается переносом: он не несёт содержания респондента.
const leakMaxStopShare = 0.6

// stopWords — служебные слова (предлоги, союзы, местоимения, частицы) в
// форме сравнения: нижний регистр, «ё» как «е».
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`в во на по и а но или либо о об обо от до за из изо к ко с со у для при без через над под про
		перед между же ли бы не ни что чтобы как это этот эта эти то тот та те так такой я мы ты вы он она оно они его ее их
		мне меня нам нас вам вас им ими ему ей себя себе свой своя свои наш наша наши ваш ваша мой моя мои все весь вся всё
		вот там тут где когда если уже еще очень только даже есть был была были быть будет чем кто какой который которая
		которые да нет же ну раз ведь или тоже также
		the a an and or of to in on for with by at from is are be it this that as`) {
		m[w] = true
	}
	return m
}()

// leakWords — слова текста в форме сравнения: регистр, «ё», знаки
// препинания не важны.
func leakWords(s string) []string {
	return strings.FieldsFunc(NormalizeTitle(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// QuoteIndex — окна цитат по LeakWords слов, посчитанные один раз для
// проверки многих текстов (все задачи плана).
type QuoteIndex struct {
	windows map[string]bool
}

// NewQuoteIndex строит индекс окон цитат. Окна, где служебных слов больше
// leakMaxStopShare, в индекс не входят.
func NewQuoteIndex(quotes []string) *QuoteIndex {
	ix := &QuoteIndex{windows: map[string]bool{}}
	for _, q := range quotes {
		qw := leakWords(q)
		for i := 0; i+LeakWords <= len(qw); i++ {
			win := qw[i : i+LeakWords]
			stop := 0
			for _, w := range win {
				if stopWords[w] {
					stop++
				}
			}
			if float64(stop) > leakMaxStopShare*float64(LeakWords) {
				continue
			}
			ix.windows[strings.Join(win, " ")] = true
		}
	}
	return ix
}

// Leak ищет в text содержательный фрагмент цитаты от LeakWords слов подряд.
func (ix *QuoteIndex) Leak(text string) (string, bool) {
	if ix == nil || len(ix.windows) == 0 {
		return "", false
	}
	tw := leakWords(text)
	for i := 0; i+LeakWords <= len(tw); i++ {
		if w := strings.Join(tw[i:i+LeakWords], " "); ix.windows[w] {
			return w, true
		}
	}
	return "", false
}

// QuoteLeak ищет в text содержательный фрагмент любой из цитат от
// LeakWords слов подряд и возвращает его. Это детерминированная проверка
// переноса речи респондентов в Epic'и и задачи.
func QuoteLeak(text string, quotes []string) (string, bool) {
	return NewQuoteIndex(quotes).Leak(text)
}

// truncateRunes обрезает строку до n рун и отмечает обрезку многоточием.
func truncateRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return string(r[:n]) + "…", true
}
