package discovery

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Пороги задания (design, решение 5).
const (
	// SummaryChunkChars — порог одного запроса конспекта: интервью длиннее
	// режется на части по строкам.
	SummaryChunkChars = 60000
	// ChunkOverlapLines — перекрытие соседних частей в строках, чтобы
	// мысль на границе не потерялась.
	ChunkOverlapLines = 20
	// SynthesisInputChars — порог входа синтеза: при превышении находки
	// сокращаются по важности.
	SynthesisInputChars = 120000
)

// Chunk — часть интервью для одного запроса конспекта: строки с FirstLine
// по LastLine включительно в исходной нумерации. Строка длиннее порога
// режется на куски: тогда FirstLine == LastLine, а Lines — один кусок.
type Chunk struct {
	FirstLine int
	LastLine  int
	Lines     []string
}

// Text — текст части без нумерации.
func (c Chunk) Text() string { return strings.Join(c.Lines, "\n") }

// Numbered — строки части с исходными номерами в формате «N| строка».
// По этим номерам модель указывает строку цитаты, а fake-llm читает задание.
func (c Chunk) Numbered() string {
	var b strings.Builder
	for i, l := range c.Lines {
		n := c.FirstLine + i
		if c.FirstLine == c.LastLine {
			n = c.FirstLine
		}
		b.WriteString(strconv.Itoa(n))
		b.WriteString("| ")
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// SplitInterview режет нормализованный текст на части по границам строк,
// каждая часть не длиннее maxChars рун:
//   - соседние части перекрываются не больше чем overlap строками, не
//     больше чем половиной части и не больше чем десятой долей порога в
//     символах: перекрытие страхует границу, а не удваивает объём;
//   - каждая следующая часть заканчивается дальше предыдущей: вложенных
//     частей нет;
//   - строка длиннее порога режется по концам предложений или пробелам
//     на куски с перекрытием в символах (sliceLongLine).
//
// Короткое интервью — одна часть.
func SplitInterview(text string, maxChars, overlap int) []Chunk {
	lines := Lines(text)
	if len(lines) == 0 {
		return nil
	}
	if maxChars <= 0 {
		maxChars = SummaryChunkChars
	}
	if overlap < 0 {
		overlap = 0
	}
	size := func(i int) int { return utf8.RuneCountInString(lines[i]) + 1 }
	var chunks []Chunk
	start, prevEnd := 0, 0
	for start < len(lines) {
		if size(start) > maxChars {
			for _, piece := range sliceLongLine(lines[start], maxChars) {
				chunks = append(chunks, Chunk{FirstLine: start + 1, LastLine: start + 1, Lines: []string{piece}})
			}
			start++
			prevEnd = start
			continue
		}
		end, total := start, 0
		for end < len(lines) && size(end) <= maxChars && total+size(end) <= maxChars {
			total += size(end)
			end++
		}
		if end <= prevEnd && len(chunks) > 0 {
			// Часть из перекрытия не дошла дальше предыдущей: начинаем
			// без перекрытия.
			start = prevEnd
			continue
		}
		chunks = append(chunks, Chunk{FirstLine: start + 1, LastLine: end, Lines: lines[start:end]})
		prevEnd = end
		if end >= len(lines) {
			break
		}
		// Перекрытие: не больше overlap строк, половины части и десятой
		// доли порога в символах.
		k, budget := 0, maxChars/10
		for k < overlap && k < (end-start)/2 && budget-size(end-1-k) >= 0 {
			budget -= size(end - 1 - k)
			k++
		}
		start = end - k
	}
	return chunks
}

// sliceLongLine режет строку длиннее порога на куски не длиннее maxChars
// рун. Граница ищется в последней пятой части окна: сначала конец
// предложения, затем пробел, иначе режется по порогу. Соседние куски
// перекрываются десятой долей порога, начало куска тоже выравнивается на
// пробел, чтобы цитата не начиналась с обрывка слова.
func sliceLongLine(line string, maxChars int) []string {
	r := []rune(line)
	overlap := maxChars / 10
	var out []string
	for start := 0; start < len(r); {
		end := start + maxChars
		if end >= len(r) {
			out = append(out, string(r[start:]))
			break
		}
		cut := boundary(r, start+maxChars*4/5, end)
		out = append(out, string(r[start:cut]))
		next := cut - overlap
		if next <= start {
			next = cut
		}
		// Начало следующего куска — начало предложения в зоне перекрытия,
		// иначе после пробела: цитата не должна начинаться с обрывка слова.
		aligned := -1
		for i := next + 1; i < cut && aligned < 0; i++ {
			if sentenceEnd(r[i-1]) && unicode.IsSpace(r[i]) {
				aligned = i + 1
			}
		}
		for i := next; i < cut && aligned < 0; i++ {
			if unicode.IsSpace(r[i]) {
				aligned = i + 1
			}
		}
		if aligned > start && aligned < cut {
			next = aligned
		}
		start = next
	}
	return out
}

func sentenceEnd(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '…' }

// boundary — позиция разреза в r[from:to]: после последнего конца
// предложения, иначе после последнего пробела, иначе to.
func boundary(r []rune, from, to int) int {
	for i := to - 1; i > from; i-- {
		if sentenceEnd(r[i-1]) && unicode.IsSpace(r[i]) {
			return i + 1
		}
	}
	for i := to - 1; i > from; i-- {
		if unicode.IsSpace(r[i]) {
			return i + 1
		}
	}
	return to
}
