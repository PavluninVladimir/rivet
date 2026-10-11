package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Разбор архива OpenSpec и привязка PR (спека domain-model «Импорт
// истории проекта»).

func writeChange(t *testing.T, root, dir, proposal, tasks string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "proposal.md"), []byte(proposal), 0o600); err != nil {
		t.Fatal(err)
	}
	if tasks != "" {
		if err := os.WriteFile(filepath.Join(d, "tasks.md"), []byte(tasks), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadArchive(t *testing.T) {
	root := t.TempDir()
	writeChange(t, root, "2026-08-12-harden-core",
		"# harden-core\n\n## Зачем\n\nЯдро ненадёжно.\nВторая строка.\n\n## Что меняется\n\n- x\n",
		"# Задачи\n\n## 1. rivet (бэкенд)\n\n- [x] 1.1 Первая\n- [ ] 1.2 Не сделана\n\n## 2. rivet-web\n\n- [x] 2.1 Консоль\n\n## 3. Проверка\n\n- [x] 3.1 Тесты\n")
	writeChange(t, root, "2026-08-09-define-requirements", "## Why\n\nEnglish goal.\n", "")
	if err := os.MkdirAll(filepath.Join(root, "не-архив"), 0o755); err != nil {
		t.Fatal(err)
	}
	changes, err := ReadArchive(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("change'ей: %d", len(changes))
	}
	// По возрастанию даты; без заголовка «# имя» название — имя каталога.
	if changes[0].Name != "define-requirements" || changes[0].Title != "define-requirements" || changes[0].Goal != "English goal." {
		t.Fatalf("первый change: %+v", changes[0])
	}
	c := changes[1]
	if c.Title != "harden-core" || c.Goal != "Ядро ненадёжно.\nВторая строка." || !c.Date.Equal(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("второй change: %+v", c)
	}
	want := []ArchiveTask{
		{"1.1 Первая", true, "rivet"}, {"1.2 Не сделана", false, "rivet"},
		{"2.1 Консоль", true, "rivet-web"}, {"3.1 Тесты", true, ""},
	}
	if len(c.Tasks) != len(want) {
		t.Fatalf("задачи: %+v", c.Tasks)
	}
	for i := range want {
		if c.Tasks[i] != want[i] {
			t.Fatalf("задача %d: %+v, ожидали %+v", i, c.Tasks[i], want[i])
		}
	}
}

func TestLinkChangesAndBuildManifest(t *testing.T) {
	changes := []Change{
		{Key: "2026-08-25-add-x", Name: "add-x", Date: time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), Title: "X",
			Tasks: []ArchiveTask{{"1.1 бэкенд", true, "rivet"}, {"2.1 консоль", true, "rivet-web"}, {"3.1 проверка", true, ""}}},
		{Key: "2026-08-12-old", Name: "old", Date: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), Title: "Old",
			Tasks: []ArchiveTask{{"1.1 ядро", true, "rivet"}}},
		{Key: "2026-08-09-none", Name: "none", Date: time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC), Title: "None"},
	}
	merged := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	changes = append(changes, Change{Key: "2026-08-26-fix-x", Name: "fix-x", Date: merged, Title: "Fix X",
		Tasks: []ArchiveTask{{"1 правка", true, "rivet"}}})
	prs := []PullRequest{
		// PR исправления упоминает родителя: родитель не должен его забрать.
		{Repo: "rivet", Number: 31, Body: "Change `fix-x`: по ретро-ревью `add-x`", URL: "https://gh/r/pull/31", MergedAt: merged},
		{Repo: "rivet", Number: 30, Body: "Change `add-x` …", URL: "https://gh/r/pull/30", MergedAt: merged},
		{Repo: "rivet-web", Number: 14, Body: "Клиентская часть change'а `add-x`", URL: "https://gh/w/pull/14", MergedAt: merged},
		{Repo: "rivet", Number: 8, Body: "без метки", URL: "https://gh/r/pull/8", MergedAt: merged.AddDate(0, 0, -13)},
		{Repo: "rivet", Number: 3, Title: "Сирота", Body: "", URL: "https://gh/r/pull/3", MergedAt: merged.AddDate(0, 0, -20)},
	}
	links, rep := LinkChanges(changes, prs, LinkMap{"old": {"rivet": 8}})
	if len(links) != 4 {
		t.Fatalf("привязок: %d", len(links))
	}
	if links[3].PRs["rivet"].Number != 31 {
		t.Fatalf("PR исправления должен остаться у исправления: %+v", links[3].PRs)
	}
	if links[0].PRs["rivet"].Number != 30 || links[0].PRs["rivet-web"].Number != 14 {
		t.Fatalf("PR по метке: %+v", links[0].PRs)
	}
	if links[1].PRs["rivet"].Number != 8 {
		t.Fatalf("PR по карте: %+v", links[1].PRs)
	}
	if len(rep.ChangesWithoutPR) != 1 || rep.ChangesWithoutPR[0] != "none" {
		t.Fatalf("без PR: %+v", rep.ChangesWithoutPR)
	}
	if len(rep.OrphanPRs) != 1 || rep.OrphanPRs[0].Number != 3 {
		t.Fatalf("сироты: %+v", rep.OrphanPRs)
	}

	m := BuildManifest(links, rep.OrphanPRs, "rivet")
	if len(m.Epics) != 5 {
		t.Fatalf("Epic'ов: %d", len(m.Epics))
	}
	x := m.Epics[0]
	if x.Key != "2026-08-25-add-x" || !x.DoneAt.Equal(merged) {
		t.Fatalf("Epic X: %+v", x)
	}
	// PR по секции: бэкенд → rivet, консоль → rivet-web, проверка — без PR.
	if x.Tasks[0].PRURL != "https://gh/r/pull/30" || x.Tasks[1].PRURL != "https://gh/w/pull/14" || x.Tasks[2].PRURL != "" {
		t.Fatalf("PR задач: %+v", x.Tasks)
	}
	orphan := m.Epics[4]
	if orphan.Key != "pr-rivet-3" || len(orphan.Tasks) != 1 || orphan.Tasks[0].PRURL != "https://gh/r/pull/3" {
		t.Fatalf("Epic сироты: %+v", orphan)
	}
	if err := m.Normalize().Validate(); err != nil {
		t.Fatalf("манифест: %v", err)
	}
}

func TestManifestValidate(t *testing.T) {
	now := time.Now()
	bad := []Manifest{
		{},
		{Epics: []Epic{{Key: "", Title: "x", CreatedAt: now}}},
		{Epics: []Epic{{Key: "a", Title: "", CreatedAt: now}}},
		{Epics: []Epic{{Key: "a", Title: "x"}}},
		{Epics: []Epic{{Key: "a", Title: "x", CreatedAt: now}, {Key: "a", Title: "y", CreatedAt: now}}},
		{Epics: []Epic{{Key: "a", Title: "x", CreatedAt: now, Tasks: []Task{{Title: ""}}}}},
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			t.Fatalf("манифест #%d должен отклоняться", i)
		}
	}
	ok := Manifest{Epics: []Epic{{Key: "a", Title: "x", CreatedAt: now, DoneAt: now.Add(-time.Hour)}}}
	n := ok.Normalize()
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
	// Завершение раньше создания подтягивается к созданию.
	if !n.Epics[0].DoneAt.Equal(now) {
		t.Fatalf("done_at: %v", n.Epics[0].DoneAt)
	}
}
