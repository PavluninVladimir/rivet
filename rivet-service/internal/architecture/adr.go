package architecture

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Журнал архитектурных решений (спеки «Журнал архитектурных решений» и
// «Реестр решений и ссылки в HLD»): файлы adr/NNNN-<slug>.md по индексу
// манифеста. Структурные поля в YAML front matter, тело — MADR.

// Статусы решения.
const (
	ADRProposed   = "proposed"
	ADRAccepted   = "accepted"
	ADRSuperseded = "superseded"
	ADRDeprecated = "deprecated"
	ADRRejected   = "rejected"
)

var adrStatuses = []string{ADRProposed, ADRAccepted, ADRSuperseded, ADRDeprecated, ADRRejected}

var adrStatusTitles = map[string]string{
	ADRProposed:   "предложено",
	ADRAccepted:   "принято",
	ADRSuperseded: "заменено",
	ADRDeprecated: "устарело",
	ADRRejected:   "отклонено",
}

// adrNamePattern — имя файла решения без расширения: NNNN-<slug>.
var adrNamePattern = regexp.MustCompile(`^([0-9]{4})-([a-z0-9][a-z0-9-]*)$`)

var adrDatePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// adrSections — обязательные разделы тела: русский и английский заголовок.
var adrSections = []struct{ ru, en, title string }{
	{"контекст", "context", "Контекст"},
	{"решение", "decision", "Решение"},
	{"последствия", "consequences", "Последствия"},
}

// Decision — архитектурное решение (api-contract Decision).
type Decision struct {
	Number       int      `json:"number"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Status       string   `json:"status"`
	Date         string   `json:"date"`
	Elements     []string `json:"elements"`
	Supersedes   []int    `json:"supersedes"`
	SupersededBy *int     `json:"superseded_by"`
	Body         string   `json:"body"`
	// Path — путь файла: в снимке от корня репозитория, иначе от корня описания.
	Path string `json:"path"`

	// Name — имя файла без расширения (NNNN-<slug>).
	Name string `json:"-"`
	// File — путь файла от корня описания.
	File string `json:"-"`
	// Raw — исходный текст файла.
	Raw      []byte   `json:"-"`
	sections []string // заголовки второго уровня тела, строчными
}

// adrFrontMatter — поля front matter до проверки.
type adrFrontMatter struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	Status       string   `json:"status"`
	Date         string   `json:"date"`
	Elements     []string `json:"elements"`
	Supersedes   []int    `json:"supersedes"`
	SupersededBy *int     `json:"superseded_by"`
}

func adrFile(dir, name string) string { return dir + ADRDir + "/" + name + ".md" }

// splitFrontMatter делит файл на front matter и тело. Обе части — срезы
// исходных байтов, тело с прежними переводами строк. eol — перевод строки
// открывающего ограждения: "\n" или "\r\n". ok=false — файл не начинается
// с ограждения «---» или оно не закрыто.
func splitFrontMatter(raw []byte) (fm, body []byte, eol string, ok bool) {
	switch {
	case bytes.HasPrefix(raw, []byte("---\r\n")):
		eol = "\r\n"
	case bytes.HasPrefix(raw, []byte("---\n")):
		eol = "\n"
	default:
		return nil, nil, "", false
	}
	start := 3 + len(eol)
	for pos := start; pos <= len(raw); {
		line, next := raw[pos:], len(raw)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], pos+i+1
		}
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte("---")) {
			return raw[start:pos], raw[next:], eol, true
		}
		if next == len(raw) {
			break
		}
		pos = next
	}
	return nil, nil, "", false
}

// bodySections — заголовки второго уровня тела вне блоков кода.
func bodySections(body string) []string {
	var out []string
	fence := false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			fence = !fence
			continue
		}
		if !fence && strings.HasPrefix(l, "## ") {
			out = append(out, strings.ToLower(strings.TrimSpace(strings.TrimPrefix(l, "## "))))
		}
	}
	return out
}

// parseDecision разбирает файл решения. Дефекты разбора — в reporter.
func parseDecision(name, file string, raw []byte, r *reporter) *Decision {
	d := &Decision{Name: name, File: file, Raw: raw}
	if m := adrNamePattern.FindStringSubmatch(name); m != nil {
		d.Number, _ = strconv.Atoi(m[1])
		d.Slug = m[2]
	}
	fm, body, _, ok := splitFrontMatter(raw)
	if !ok {
		r.errorf("adr.front-matter", file, "", "файл решения должен начинаться с YAML front matter между строками ---")
		return d
	}
	d.Body = strings.TrimLeft(string(body), "\r\n")
	d.sections = bodySections(d.Body)
	_, v, err := parseYAML(fm)
	if err != nil {
		r.errorf("yaml.parse", file, "", "front matter не разбирается: %v", err)
		return d
	}
	if v == nil {
		v = map[string]any{}
	}
	reportSchema(r, adrSchema, v, func(p []string) (string, string) { return file, formatPointer(p) })
	var f adrFrontMatter
	decodeValue(v, &f)
	if f.Number != 0 && d.Number != 0 && f.Number != d.Number {
		r.errorf("adr.number-mismatch", file, "/number", "номер %d не совпадает с номером в имени файла %s", f.Number, name)
	}
	if f.Number != 0 {
		d.Number = f.Number
	}
	d.Title, d.Status, d.Date = f.Title, f.Status, f.Date
	d.Elements = append([]string{}, f.Elements...)
	d.Supersedes = append([]int{}, f.Supersedes...)
	d.SupersededBy = f.SupersededBy
	return d
}

// loadDecisions читает решения из индекса. dir — префикс каталога.
func loadDecisions(fsys fs.FS, names []string, dir, indexFile, indexPtr string, r *reporter) ([]*Decision, error) {
	var out []*Decision
	seen := map[string]bool{}
	for i, name := range names {
		loc := fmt.Sprintf("%s/%d", indexPtr, i)
		switch {
		case !adrNamePattern.MatchString(name):
			r.errorf("adr.name", indexFile, loc, "имя решения %q должно иметь вид NNNN-<slug>: четыре цифры номера, дефис, строчные латинские буквы, цифры и дефис", name)
			continue
		case seen[name]:
			r.errorf("adr.name", indexFile, loc, "решение %s указано в индексе повторно", name)
			continue
		}
		seen[name] = true
		file := adrFile(dir, name)
		raw, err := fs.ReadFile(fsys, file)
		if errors.Is(err, fs.ErrNotExist) {
			r.errorf("adr.missing", indexFile, loc, "нет файла решения %s", file)
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, parseDecision(name, file, raw, r))
	}
	sortDecisions(out)
	return out, nil
}

func sortDecisions(list []*Decision) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Number != list[j].Number {
			return list[i].Number < list[j].Number
		}
		return list[i].Name < list[j].Name
	})
}

// checkDecisions — правила adr.* по журналу решений модели.
func (m *Model) checkDecisions(r *reporter, opt checkOptions) {
	byNum := map[int][]*Decision{}
	for _, d := range m.decisions {
		if d.Number > 0 {
			byNum[d.Number] = append(byNum[d.Number], d)
		}
	}
	for _, d := range m.decisions {
		if d.Status != "" && !slices.Contains(adrStatuses, d.Status) {
			r.errorf("adr.status", d.File, "/status", "неизвестный статус %q: proposed, accepted, superseded, deprecated или rejected", d.Status)
		}
		if d.Date != "" {
			if _, err := time.Parse("2006-01-02", d.Date); err != nil || !adrDatePattern.MatchString(d.Date) {
				r.errorf("adr.date", d.File, "/date", "дата %q должна быть в формате ГГГГ-ММ-ДД", d.Date)
			}
		}
		for _, s := range adrSections {
			if !slices.Contains(d.sections, s.ru) && !slices.Contains(d.sections, s.en) {
				r.errorf("adr.section", d.File, "", "в теле решения нет раздела «%s» (## %s или ## %s)", s.title, s.title, strings.ToUpper(s.en[:1])+s.en[1:])
			}
		}
		// Заменённое, устаревшее или отклонённое решение остаётся в журнале
		// историей: его ссылка на элемент не мешает этот элемент удалить.
		historic := d.Status == ADRSuperseded || d.Status == ADRDeprecated || d.Status == ADRRejected
		for i, id := range d.Elements {
			if !opt.removed[id] && m.element(id) != nil {
				continue
			}
			loc := fmt.Sprintf("/elements/%d", i)
			if historic {
				r.warnf("adr.element", d.File, loc, "элемента %s нет в модели, решение со статусом %s остаётся в журнале как история", id, d.Status)
			} else {
				r.errorf("adr.element", d.File, loc, "элемента %s нет в модели", id)
			}
		}
		switch {
		case d.Status == ADRSuperseded && d.SupersededBy == nil:
			r.errorf("adr.superseded-by", d.File, "/superseded_by", "у решения со статусом superseded должен быть superseded_by")
		case d.Status != ADRSuperseded && d.SupersededBy != nil:
			r.errorf("adr.superseded-by", d.File, "/superseded_by", "superseded_by задаётся только у решения со статусом superseded, а статус %s", d.Status)
		}
		// Решение заменяет только более ранние решения и заменяется только
		// более поздним. Так в журнале не бывает циклов замены.
		for i, n := range d.Supersedes {
			loc := fmt.Sprintf("/supersedes/%d", i)
			targets := byNum[n]
			switch {
			case d.Number > 0 && n >= d.Number:
				r.errorf("adr.supersede-target", d.File, loc, "решение %d может заменять только решения с меньшим номером, а не %d", d.Number, n)
				continue
			case len(targets) == 0:
				r.errorf("adr.supersede-target", d.File, loc, "решение %d заменяет несуществующее решение %d", d.Number, n)
				continue
			}
			t := targets[0]
			if t.Status != ADRSuperseded || t.SupersededBy == nil || *t.SupersededBy != d.Number {
				r.errorf("adr.supersede-mutual", d.File, loc,
					"решение %d заменяет %d, но у %d должны быть статус superseded и superseded_by: %d", d.Number, n, n, d.Number)
			}
		}
		if d.SupersededBy != nil {
			by := *d.SupersededBy
			targets := byNum[by]
			switch {
			case d.Number > 0 && by <= d.Number:
				r.errorf("adr.supersede-target", d.File, "/superseded_by", "решение %d может быть заменено только решением с большим номером, а не %d", d.Number, by)
			case len(targets) == 0:
				r.errorf("adr.supersede-target", d.File, "/superseded_by", "решение %d заменено несуществующим решением %d", d.Number, by)
			case !slices.Contains(targets[0].Supersedes, d.Number):
				r.errorf("adr.supersede-mutual", d.File, "/superseded_by",
					"решение %d заменено решением %d, но у %d нет supersedes: [%d]", d.Number, by, by, d.Number)
			}
		}
	}
	// Нумерация подряд с 1 без повторов: удалять решения нельзя, только
	// переводить в deprecated или rejected.
	var numbers []int
	for n, list := range byNum {
		numbers = append(numbers, n)
		if len(list) > 1 {
			var files []string
			for _, d := range list {
				files = append(files, d.File)
			}
			sort.Strings(files)
			r.errorf("adr.numbering", ManifestFile, "/decisions", "номер решения %d повторяется: %s", n, strings.Join(files, ", "))
		}
	}
	sort.Ints(numbers)
	if len(numbers) > 0 {
		var missing []string
		for n := 1; n < numbers[len(numbers)-1]; n++ {
			if len(byNum[n]) == 0 {
				missing = append(missing, fmt.Sprintf("%04d", n))
			}
		}
		if len(missing) > 0 {
			r.errorf("adr.numbering", ManifestFile, "/decisions",
				"нумерация решений должна идти подряд с 1, нет решений: %s", strings.Join(missing, ", "))
		}
	}
}

// Decisions возвращает решения описания по номеру.
func (m *Model) Decisions() []*Decision { return m.decisions }

// decisionView — решение для снимка: путь от корня репозитория.
func decisionView(d *Decision, prefix string) Decision {
	out := *d
	out.Path = prefix + d.File
	if out.Elements == nil {
		out.Elements = []string{}
	}
	if out.Supersedes == nil {
		out.Supersedes = []int{}
	}
	return out
}

func adrStatusTitle(s string) string {
	if t, ok := adrStatusTitles[s]; ok {
		return t
	}
	return s
}

// renderADRIndex — реестр решений views/adr.md.
func renderADRIndex(m *Model) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# Архитектурные решения: %s\n\n", m.participantName(DescribedSystemID))
	if len(m.decisions) == 0 {
		b.WriteString("Решений в журнале нет.\n")
		return b.String()
	}
	b.WriteString("| № | Решение | Статус | Дата | Элементы | Замена |\n|---|---|---|---|---|---|\n")
	for _, d := range m.decisions {
		var repl []string
		for _, n := range d.Supersedes {
			repl = append(repl, fmt.Sprintf("заменяет %04d", n))
		}
		if d.SupersededBy != nil {
			repl = append(repl, fmt.Sprintf("заменено %04d", *d.SupersededBy))
		}
		fmt.Fprintf(&b, "| %04d | [%s](../%s) | %s | %s | %s | %s |\n",
			d.Number, cell(linkText(d.Title)), d.File, adrStatusTitle(d.Status), d.Date, code(d.Elements), listOr(repl, ""))
	}
	return b.String()
}
