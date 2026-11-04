package ops

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MergedTask — смерженная задача проекта: кандидат в состав релиза.
type MergedTask struct {
	TaskID      string
	Num         int
	Title       string
	Kind        string // work | bug | architecture
	EpicID      string
	EpicTitle   string
	PRURL       string
	PRTitle     string
	IncidentID  string
	Description string
	Criteria    []string
	MergedAt    time.Time
}

// Composition — состав релиза: задачи, смерженные в полуинтервале
// (создание прошлой успешной публикации окружения, создание этой]. Без
// прошлой публикации — все смерженные не позже создания этой. Порядок: по
// времени merge, затем по номеру.
func Composition(tasks []MergedTask, prevCreated *time.Time, created time.Time) []MergedTask {
	out := []MergedTask{}
	for _, t := range tasks {
		if t.MergedAt.After(created) {
			continue
		}
		if prevCreated != nil && !t.MergedAt.After(*prevCreated) {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].MergedAt.Equal(out[j].MergedAt) {
			return out[i].MergedAt.Before(out[j].MergedAt)
		}
		return out[i].Num < out[j].Num
	})
	return out
}

// ReleaseItem — задача состава в API (api-contract ReleaseItem).
type ReleaseItem struct {
	TaskID     string  `json:"task_id"`
	Num        int     `json:"num"`
	Title      string  `json:"title"`
	Kind       string  `json:"kind"`
	EpicID     string  `json:"epic_id"`
	EpicTitle  string  `json:"epic_title"`
	PRURL      string  `json:"pr_url"`
	IncidentID *string `json:"incident_id"`
}

// Items — состав для хранения в releases.composition и для API.
func Items(comp []MergedTask) []ReleaseItem {
	out := make([]ReleaseItem, 0, len(comp))
	for _, t := range comp {
		kind := t.Kind
		if kind == "" {
			kind = "work"
		}
		it := ReleaseItem{TaskID: t.TaskID, Num: t.Num, Title: t.Title, Kind: kind, EpicID: t.EpicID, EpicTitle: t.EpicTitle, PRURL: t.PRURL}
		if t.IncidentID != "" {
			id := t.IncidentID
			it.IncidentID = &id
		}
		out = append(out, it)
	}
	return out
}

// Статусы release notes.
const (
	NotesPending  = "pending"
	NotesReady    = "ready"
	NotesTemplate = "template"
)

// NoteItem — пункт release notes со ссылкой на задачу состава.
type NoteItem struct {
	TaskNum int
	Text    string
}

// Notes — release notes по разделам.
type Notes struct {
	New, Fixes, Other []NoteItem
}

const maxNoteText = 300

var reTaskNum = regexp.MustCompile(`\d+`)

// taskNum — номер задачи из поля ответа: число, «7», «task-7», «#7».
func taskNum(v json.RawMessage) (int, bool) {
	var n int
	if err := json.Unmarshal(v, &n); err == nil {
		return n, n > 0
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return 0, false
	}
	m := reTaskNum.FindString(s)
	if m == "" {
		return 0, false
	}
	n, err := strconv.Atoi(m)
	return n, err == nil && n > 0
}

func noteText(s string) string {
	s = oneLine(s)
	s = strings.TrimLeft(s, "-*• ")
	if utf8.RuneCountInString(s) > maxNoteText {
		s = string([]rune(s)[:maxNoteText-1]) + "…"
	}
	return s
}

// ParseNotes разбирает ответ задания release_notes
// `{new:[{task, text}], fixes:[...], other:[...]}` и проверяет его составом:
// пункты без задачи состава, без текста и повторы задачи отбрасываются
// (dropped), задачи состава, которые агент не упомянул, дописываются в
// «Прочее» по названию.
func ParseNotes(raw string, comp []MergedTask) (notes Notes, dropped int, err error) {
	obj, err := jsonObject(raw)
	if err != nil {
		return Notes{}, 0, err
	}
	type item struct {
		Task json.RawMessage `json:"task"`
		Text string          `json:"text"`
	}
	var resp struct {
		New   []item `json:"new"`
		Fixes []item `json:"fixes"`
		Other []item `json:"other"`
	}
	if err := json.Unmarshal([]byte(obj), &resp); err != nil {
		return Notes{}, 0, badAnswer("JSON: %v", err)
	}
	inComp := map[int]bool{}
	for _, t := range comp {
		inComp[t.Num] = true
	}
	used := map[int]bool{}
	take := func(src []item) []NoteItem {
		out := []NoteItem{}
		for _, it := range src {
			n, ok := taskNum(it.Task)
			text := noteText(it.Text)
			if !ok || !inComp[n] || used[n] || text == "" {
				dropped++
				continue
			}
			used[n] = true
			out = append(out, NoteItem{TaskNum: n, Text: text})
		}
		return out
	}
	notes.New, notes.Fixes, notes.Other = take(resp.New), take(resp.Fixes), take(resp.Other)
	if len(comp) > 0 && len(used) == 0 {
		// Ни одного валидного пункта: это не release notes, а мусор. Вызывающий
		// соберёт заметки по шаблону.
		return Notes{}, dropped, badAnswer("нет ни одного пункта по задачам состава")
	}
	for _, t := range comp {
		if !used[t.Num] {
			notes.Other = append(notes.Other, NoteItem{TaskNum: t.Num, Text: noteText(t.Title)})
		}
	}
	return notes, dropped, nil
}

// TemplateNotes — запасной вариант без модели: названия задач по виду
// (work → «Новое», bug → «Исправления», остальное → «Прочее»).
func TemplateNotes(comp []MergedTask) Notes {
	n := Notes{New: []NoteItem{}, Fixes: []NoteItem{}, Other: []NoteItem{}}
	for _, t := range comp {
		it := NoteItem{TaskNum: t.Num, Text: noteText(t.Title)}
		switch t.Kind {
		case "", "work":
			n.New = append(n.New, it)
		case "bug":
			n.Fixes = append(n.Fixes, it)
		default:
			n.Other = append(n.Other, it)
		}
	}
	return n
}

// RenderNotes — Markdown release notes. Пункт ссылается на задачу состава,
// у бага с инцидентом добавляется короткая ссылка на инцидент. Пустые
// разделы не выводятся.
func RenderNotes(n Notes, comp []MergedTask, env, version string) string {
	byNum := map[int]MergedTask{}
	for _, t := range comp {
		byNum[t.Num] = t
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Релиз %s · %s\n", shortSHA(version), env)
	if len(n.New)+len(n.Fixes)+len(n.Other) == 0 {
		b.WriteString("\nИзменений в составе нет.\n")
		return b.String()
	}
	section := func(title string, items []NoteItem) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, it := range items {
			ref := fmt.Sprintf("task-%d", it.TaskNum)
			if t := byNum[it.TaskNum]; t.IncidentID != "" {
				ref += ", инцидент " + shortID(t.IncidentID)
			}
			fmt.Fprintf(&b, "- %s (%s)\n", it.Text, ref)
		}
	}
	section("Новое", n.New)
	section("Исправления", n.Fixes)
	section("Прочее", n.Other)
	return b.String()
}

var reHexSHA = regexp.MustCompile(`^[0-9a-fA-F]{9,64}$`)

// shortSHA укорачивает до 8 символов только hex-SHA. Теги, ветки, версии
// вида v1.2.3 и текст-заглушка остаются как есть.
func shortSHA(v string) string {
	if reHexSHA.MatchString(v) {
		return v[:8]
	}
	return v
}

// shortID — первые 8 символов идентификатора (UUID инцидента) для ссылки.
func shortID(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// Analysis — разбор провала публикации Релиз-инженером.
type Analysis struct {
	Cause          string `json:"cause"`
	Tasks          []int  `json:"tasks"`
	Recommendation string `json:"recommendation"`
}

// ParseAnalysis разбирает ответ задания deploy_analysis
// `{cause, tasks[], recommendation}`: затронутые задачи только из состава.
func ParseAnalysis(raw string, comp []MergedTask) (Analysis, error) {
	obj, err := jsonObject(raw)
	if err != nil {
		return Analysis{}, err
	}
	var resp struct {
		Cause          string            `json:"cause"`
		Tasks          []json.RawMessage `json:"tasks"`
		Recommendation string            `json:"recommendation"`
	}
	if err := json.Unmarshal([]byte(obj), &resp); err != nil {
		return Analysis{}, badAnswer("JSON: %v", err)
	}
	a := Analysis{Cause: strings.TrimSpace(Clean(resp.Cause)), Recommendation: strings.TrimSpace(Clean(resp.Recommendation)), Tasks: []int{}}
	if err := checkLen("cause", a.Cause, 1, maxCause); err != nil {
		return Analysis{}, err
	}
	if err := checkLen("recommendation", a.Recommendation, 0, maxCause); err != nil {
		return Analysis{}, err
	}
	inComp := map[int]bool{}
	for _, t := range comp {
		inComp[t.Num] = true
	}
	seen := map[int]bool{}
	for _, raw := range resp.Tasks {
		if n, ok := taskNum(raw); ok && inComp[n] && !seen[n] {
			seen[n] = true
			a.Tasks = append(a.Tasks, n)
		}
	}
	return a, nil
}

// ReleaseIncident — описание инцидента провала публикации: из разбора
// Релиз-инженера или, без него, из этапа и хвоста лога. Задача-баг
// предлагает добиться прохождения проверки публикации.
func ReleaseIncident(a *Analysis, env, version, stage, logTail string) Description {
	summary := fmt.Sprintf("Публикация %s в окружение «%s» провалилась на этапе %s", shortSHA(version), env, stage)
	d := Description{Summary: summary, Reproduction: []string{}}
	desc := summary + "."
	if a != nil {
		d.Cause = a.Cause
		if len(a.Tasks) > 0 {
			refs := make([]string, 0, len(a.Tasks))
			for _, n := range a.Tasks {
				refs = append(refs, fmt.Sprintf("task-%d", n))
			}
			desc += "\n\nЗатронутые задачи состава: " + strings.Join(refs, ", ") + "."
		}
		if a.Recommendation != "" {
			desc += "\n\n**Рекомендация:** " + a.Recommendation
		}
	} else {
		desc += "\n\n" + EvidenceNote + "\n\n" + fenced(Evidence(logTail))
		d.EvidenceIncluded = true
	}
	d.Bug = BugProposal{
		Title:       "Публикация в " + env + " падает на этапе " + stage,
		Description: desc,
		Criteria:    []string{"Публикация в окружение «" + env + "» проходит проверку"},
	}
	return d
}
