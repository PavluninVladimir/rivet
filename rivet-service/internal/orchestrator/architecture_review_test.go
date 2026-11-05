package orchestrator

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
)

// Тесты находок ревью Opus, круг 1 (add-architecture-c4).

// fixtureFiles — файлы фикстуры testdata/<name> для фиктивного хостинга.
func fixtureFiles(t *testing.T, name, repo, ref string) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "architecture", "testdata", name)
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		files[repo+"@"+ref+":"+architecture.DefaultDir+"/"+filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Ветка ушла вперёд, а дерево docs/architecture то же: снимок не
// перечитывается, события нет, запоминается новый SHA (ревью Opus, круг 2).
func TestRefreshSameTreeSkipsRead(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{files: describedFiles(t, "o/r", "sha-1"), headSHA: "sha-1", treeSHA: "tree-1"}
	p, e := archProject(t, st, sc)
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	r, _, _ := st.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "ok" || r.TreeSHA != "tree-1" {
		t.Fatalf("первое чтение: %s %q", r.Status, r.TreeSHA)
	}
	// Merge кода: новый SHA ветки, файлов описания по нему у fake нет, то
	// есть полное чтение дало бы absent.
	sc.mu.Lock()
	sc.headSHA = "sha-2"
	sc.mu.Unlock()
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	r, _, _ = st.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "ok" || r.SHA != "sha-2" || r.ValidSHA != "sha-2" {
		t.Fatalf("снимок после merge без правок описания: %s %s %s", r.Status, r.SHA, r.ValidSHA)
	}
	if n := len(archEvents(t, st, p.ID, "project.architecture_updated")); n != 1 {
		t.Fatalf("дерево то же, а снимок перечитан: событий %d", n)
	}
	// Дерево изменилось: полное чтение.
	sc.mu.Lock()
	sc.headSHA, sc.treeSHA = "sha-3", "tree-2"
	sc.mu.Unlock()
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := len(archEvents(t, st, p.ID, "project.architecture_updated")); n != 2 {
		t.Fatalf("новое дерево не перечитано: событий %d", n)
	}
}

// Подключение репозитория с готовым описанием: повод не пропадает молча,
// пишется пропуск has_description (спека «Автоописание архитектуры»).
func TestConnectWithDescriptionLogsSkip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	p, e := archProject(t, st, &fakeSCM{files: describedFiles(t, "o/r", "sha-head")})
	if err := e.ConnectArchitectureNow(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	skips := archEvents(t, st, p.ID, "architecture.describe_skipped")
	if len(skips) != 1 || skips[0].Payload["reason"] != "has_description" || skips[0].Payload["trigger"] != "connect" {
		t.Fatalf("пропуск при готовом описании: %+v", skips)
	}
}

// Символическая ссылка в каталоге описания: содержимое цели в снимок не
// попадает, файл пропускается с дефектом file.symlink, снимок invalid
// (ревью Opus, круг 3).
func TestRefreshSymlinkDefect(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	files := describedFiles(t, "o/r", "sha-head")
	link := ""
	for k := range files {
		if strings.HasSuffix(k, "/model/application.yaml") {
			link = strings.TrimPrefix(k, "o/r@sha-head:")
		}
	}
	if link == "" {
		t.Fatal("в скелете описания нет model/application.yaml")
	}
	p, e := archProject(t, st, &fakeSCM{files: files, symlinks: map[string]bool{link: true}})
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	r, _, _ := st.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "invalid" || r.Data != nil || !strings.Contains(string(r.Defects), "file.symlink") {
		t.Fatalf("снимок с символической ссылкой: %s %s", r.Status, r.Defects)
	}
}

// Ни один runner не объявляет architecture: причина ожидания задачи
// описания подсказывает обновить rivet-runner (ревью Opus, круг 2).
func TestArchitectureWaitReasonHint(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	p, e := archProject(t, st, &fakeSCM{})
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "old", Agent: "wrap", Capabilities: []string{"coding", "review"}}); err != nil {
		t.Fatal(err)
	}
	_, taskID, err := e.DescribeArchitecture(ctx, p.ID, "manual", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, taskID)
	if !strings.Contains(task.WaitReason, "обновите rivet-runner") {
		t.Fatalf("причина ожидания без runner'ов с architecture: %q", task.WaitReason)
	}
}

// Пока снимка нет, обычной задаче проверка «Архитектура» добавляется:
// описание может быть, гейт сам пропустит ветку без его изменений.
func TestArchitectureChecksWithoutSnapshot(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	p, e := archProject(t, st, &fakeSCM{})
	checks := e.architectureChecks(ctx, domain.Task{Kind: domain.KindWork}, p, nil)
	if len(checks) != 1 || checks[0].Name != "Архитектура" {
		t.Fatalf("проверки без снимка: %+v", checks)
	}
}

// Превышение лимита на том же SHA не перечитывается: повтор дал бы то же.
func TestRefreshLimitNotReread(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{files: describedFiles(t, "o/r", "sha-head")}
	p, e := archProject(t, st, sc)
	old := ArchitectureFSLimits
	ArchitectureFSLimits = scm.FSLimits{MaxFiles: 1, MaxBytes: 8 << 20}
	defer func() { ArchitectureFSLimits = old }()
	for i := 0; i < 3; i++ {
		if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(archEvents(t, st, p.ID, "project.architecture_updated")); n != 1 {
		t.Fatalf("тот же SHA с превышением лимита перечитан: событий %d", n)
	}
	// Сбой хостинга, наоборот, перечитывается.
	sc.mu.Lock()
	sc.headErr = errHost
	sc.mu.Unlock()
	_ = e.RefreshArchitectureNow(ctx, p.ID, false)
	sc.mu.Lock()
	sc.headErr = nil
	sc.mu.Unlock()
	_ = e.RefreshArchitectureNow(ctx, p.ID, false)
	if n := len(archEvents(t, st, p.ID, "project.architecture_updated")); n != 3 {
		t.Fatalf("после сбоя хостинга тот же SHA не перечитан: событий %d", n)
	}
}

var errHost = &hostErr{}

type hostErr struct{}

func (*hostErr) Error() string { return "503" }

// Нулевой символ в тексте ADR jsonb не принимает: снимок не замораживается,
// а получает статус error с причиной.
func TestRefreshNulInADR(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	files := fixtureFiles(t, "crm", "o/r", "sha-head")
	nul := false
	for k, v := range files {
		if strings.Contains(k, "/adr/") {
			files[k] = v + "\nстрока с нулевым символом \x00 в теле\n"
			nul = true
		}
	}
	if !nul {
		t.Fatal("в фикстуре crm нет ADR")
	}
	p, e := archProject(t, st, &fakeSCM{files: files})
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatalf("обновление с нулевым символом: %v", err)
	}
	r, ok, err := st.GetProjectArchitecture(ctx, p.ID)
	if err != nil || !ok {
		t.Fatalf("снимок не записан: %v %v", ok, err)
	}
	if r.Status != "error" || !strings.Contains(r.Error, "снимок не записан") {
		t.Fatalf("снимок с нулевым символом: %s %q", r.Status, r.Error)
	}
	if evs := archEvents(t, st, p.ID, "project.architecture_updated"); len(evs) != 1 {
		t.Fatalf("событие снимка: %d", len(evs))
	}
}
