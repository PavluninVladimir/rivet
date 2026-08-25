package history

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Разбор архива OpenSpec: каталог changes/archive/<дата>-<имя>/ с
// proposal.md («Зачем») и tasks.md (чекбоксы под заголовками секций,
// называющими репозиторий: «## 1. rivet», «## 2. rivet-web»).

// Change — архивный change как источник Epic'а.
type Change struct {
	Key   string    // имя каталога: <дата>-<имя>
	Name  string    // имя change'а без даты
	Date  time.Time // дата архивации из префикса
	Title string
	Goal  string
	Tasks []ArchiveTask
}

// ArchiveTask — пункт tasks.md с секцией, в которой он стоял.
type ArchiveTask struct {
	Title   string
	Done    bool
	Section string // репозиторий из заголовка секции; пусто — не про репозиторий
}

var (
	archiveDirRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-(.+)$`)
	taskLineRe   = regexp.MustCompile(`^\s*-\s*\[( |x|X)\]\s*(.+?)\s*$`)
	sectionRe    = regexp.MustCompile(`^##\s+(?:\d+\.\s*)?(.+?)\s*$`)
	// taskNumRe — «1.2 » в начале пункта: номер оставляем в заголовке, он
	// помогает читать историю, но не считается частью названия при сверке.
	taskNumRe = regexp.MustCompile(`^\d+(\.\d+)*\s+`)
)

// ReadArchive читает все change'и архива по возрастанию даты.
func ReadArchive(dir string) ([]Change, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := archiveDirRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		date, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		c, err := readChange(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		c.Key, c.Name, c.Date = e.Name(), m[2], date
		if c.Title == "" {
			c.Title = c.Name
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// readChange — название и цель из proposal.md, задачи из tasks.md.
func readChange(dir string) (Change, error) {
	var c Change
	if raw, err := os.ReadFile(filepath.Join(dir, "proposal.md")); err == nil {
		c.Title, c.Goal = parseProposal(string(raw))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tasks.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	c.Tasks = parseTasks(string(raw))
	return c, nil
}

// parseProposal — заголовок «# имя» (если есть) и текст раздела «Зачем»
// (или «Why») до следующего раздела.
func parseProposal(text string) (title, goal string) {
	var goalLines []string
	inGoal := false
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# ") && title == "":
			title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
		case strings.HasPrefix(line, "## "):
			head := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			inGoal = head == "зачем" || head == "why"
		case inGoal:
			goalLines = append(goalLines, line)
		}
	}
	return title, strings.TrimSpace(strings.Join(goalLines, "\n"))
}

// parseTasks — чекбоксы с секцией репозитория из ближайшего заголовка.
func parseTasks(text string) []ArchiveTask {
	var out []ArchiveTask
	section := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			section = repoFromSection(m[1])
			continue
		}
		m := taskLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, ArchiveTask{
			Title:   strings.TrimSpace(m[2]),
			Done:    m[1] != " ",
			Section: section,
		})
	}
	return out
}

// repoFromSection — имя репозитория из заголовка секции: «rivet (бэкенд и
// runner)» → rivet, «rivet-web» → rivet-web; «Проверка» — не репозиторий.
func repoFromSection(head string) string {
	head = strings.ToLower(head)
	fields := strings.FieldsFunc(head, func(r rune) bool {
		return r == ' ' || r == '(' || r == ')' || r == ':' || r == ','
	})
	if len(fields) == 0 {
		return ""
	}
	first := fields[0]
	if strings.HasPrefix(first, "rivet") {
		return first
	}
	return ""
}

// StripTaskNum — название задачи без ведущего номера «1.2 ».
func StripTaskNum(title string) string {
	return taskNumRe.ReplaceAllString(title, "")
}
