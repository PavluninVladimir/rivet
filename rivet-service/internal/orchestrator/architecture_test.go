package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// describedFiles — файлы корректного описания (architecture init +
// представления) для фиктивного хостинга: ключи repo@ref:docs/architecture/…
func describedFiles(t *testing.T, repo, ref string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := architecture.Init(dir, "Демо"); err != nil {
		t.Fatal(err)
	}
	if _, err := architecture.WriteViews(dir); err != nil {
		t.Fatal(err)
	}
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

func archProject(t *testing.T, st *store.Store, sc *fakeSCM) (domain.Project, *Engine) {
	t.Helper()
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	// Fake-установка: проекты тестов без собственных учётных данных.
	e.Adapters.Force = true
	owner := mustOwner(t, st)
	p, err := st.CreateProject(context.Background(), "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, e
}

func archEvents(t *testing.T, st *store.Store, projectID, typ string) []domain.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{ProjectID: projectID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// Спека project-architecture «Снимок описания»: снимок строится по SHA
// основной ветки, неизменный SHA не перечитывается, сбой хостинга пишется
// статусом error и не теряет последнюю корректную версию.
func TestRefreshArchitectureSnapshot(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{files: describedFiles(t, "o/r", "sha-head")}
	p, e := archProject(t, st, sc)

	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	r, ok, err := st.GetProjectArchitecture(ctx, p.ID)
	if err != nil || !ok || r.Status != "ok" || r.SHA != "sha-head" || r.ValidSHA != "sha-head" {
		t.Fatalf("снимок: %+v ok=%v err=%v", r, ok, err)
	}
	var data architecture.SnapshotData
	if err := json.Unmarshal(r.Data, &data); err != nil || data.System.Name != "Демо" || data.HLD == "" {
		t.Fatalf("данные снимка: %v %+v", err, data.System)
	}
	if evs := archEvents(t, st, p.ID, "project.architecture_updated"); len(evs) != 1 || evs[0].Payload["status"] != "ok" {
		t.Fatalf("событие снимка: %+v", evs)
	}
	// Описание есть: автоописание при подключении не ставит задачу.
	if _, ok, _ := st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("задача описания поставлена при готовом описании")
	}

	// Тот же SHA без force: перечитывать нечего.
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := len(archEvents(t, st, p.ID, "project.architecture_updated")); n != 1 {
		t.Fatalf("неизменный SHA перечитан: событий %d", n)
	}
	// force перечитывает всегда: паузу между ручными запросами решает API,
	// и после 202 событие приходит даже при гонке с другим чтением.
	if err := e.RefreshArchitectureNow(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := len(archEvents(t, st, p.ID, "project.architecture_updated")); n != 2 {
		t.Fatalf("force не перечитал: событий %d", n)
	}
	// Пауза для API: сразу после чтения повтор ждёт, после паузы нет.
	if wait, err := e.RefreshWait(ctx, p.ID); err != nil || wait <= 0 {
		t.Fatalf("пауза сразу после чтения: %v %v", wait, err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE project_architecture SET read_at = now() - interval '1 minute' WHERE project_id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if wait, err := e.RefreshWait(ctx, p.ID); err != nil || wait != 0 {
		t.Fatalf("пауза прошла: %v %v", wait, err)
	}

	// Хостинг недоступен: error, последняя корректная версия цела.
	sc.mu.Lock()
	sc.headErr = errors.New("503")
	sc.mu.Unlock()
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatalf("сбой хостинга ломает обновление: %v", err)
	}
	r, _, _ = st.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "error" || !strings.Contains(r.Error, "хостинг недоступен") || r.ValidSHA != "sha-head" {
		t.Fatalf("снимок после сбоя: %+v", r)
	}
	// После сбоя тот же SHA перечитывается.
	sc.mu.Lock()
	sc.headErr = nil
	sc.mu.Unlock()
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if r, _, _ = st.GetProjectArchitecture(ctx, p.ID); r.Status != "ok" {
		t.Fatalf("после восстановления хостинга: %+v", r)
	}

	// Одиночное исполнение: поводы, пришедшие во время обновления, дают
	// ровно один повтор.
	block := make(chan struct{})
	sc.mu.Lock()
	sc.headBlock, sc.heads = block, 0
	sc.mu.Unlock()
	e.RefreshArchitecture(p.ID, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sc.mu.Lock()
		n := sc.heads
		sc.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("фоновое обновление не началось")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		e.RefreshArchitecture(p.ID, false)
	}
	close(block)
	e.WaitArchitecture()
	sc.mu.Lock()
	heads := sc.heads
	sc.headBlock = nil
	sc.mu.Unlock()
	if heads != 2 {
		t.Fatalf("одиночное исполнение: обращений к хостингу %d, ожидалось 2", heads)
	}

	// У проекта есть описание: обычной задаче добавляется проверка «Архитектура».
	checks := e.architectureChecks(ctx, domain.Task{Kind: domain.KindWork}, p, nil)
	if len(checks) != 1 || checks[0].Name != "Архитектура" || strings.Contains(checks[0].Cmd, "-force") {
		t.Fatalf("проверки обычной задачи: %+v", checks)
	}
}

// Превышение лимитов чтения: статус error с понятной причиной.
func TestRefreshArchitectureLimit(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{files: describedFiles(t, "o/r", "sha-head")}
	p, e := archProject(t, st, sc)
	old := ArchitectureFSLimits
	ArchitectureFSLimits = scm.FSLimits{MaxFiles: 1, MaxBytes: 8 << 20}
	defer func() { ArchitectureFSLimits = old }()

	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	r, _, _ := st.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "error" || !strings.Contains(r.Error, "слишком большое") {
		t.Fatalf("лимит чтения: %+v", r)
	}
}

// Спека «Автоописание архитектуры»: первый снимок без описания ставит
// задачу Архитектору с внутренним процессом, повторно автоописание не
// срабатывает.
func TestAutoDescribeOnConnect(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	p, e := archProject(t, st, sc)

	// Обычное обновление поводом «подключение» не считается: проект,
	// подключённый до обновления установки, ждёт запуска Epic.
	if err := e.RefreshArchitectureNow(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := st.GetProjectArchitecture(ctx, p.ID); r.Status != "absent" {
		t.Fatalf("снимок без описания: %+v", r)
	}
	if _, ok, _ := st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("обычное обновление поставило задачу описания")
	}

	if err := e.ConnectArchitectureNow(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	info, ok, err := st.ArchitectureTask(ctx, p.ID)
	if err != nil || !ok || info.Trigger != "connect" {
		t.Fatalf("задача описания при подключении: %+v %v %v", info, ok, err)
	}
	task, err := st.GetTask(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var proc policy.Resolved
	if err := json.Unmarshal(task.Process, &proc); err != nil {
		t.Fatal(err)
	}
	if task.Kind != domain.KindArchitecture || len(proc.Steps) != 4 {
		t.Fatalf("задача описания: kind=%s process=%s", task.Kind, task.Process)
	}
	describe := proc.Steps[0]
	if describe.ID != "describe" || describe.Kind != policy.StepCode || describe.Participants[0].Agent.Role != "architect" {
		t.Fatalf("шаг описания: %+v", describe)
	}
	if task.ProcessHash != policy.Hash(proc) {
		t.Fatal("хеш процесса не совпадает со снимком")
	}
	epic, err := st.GetEpic(ctx, info.EpicID)
	if err != nil || epic.Kind != domain.KindArchitecture || epic.Title != "Архитектура проекта" {
		t.Fatalf("системный Epic: %+v %v", epic, err)
	}

	// Задача описания проверяет только описание.
	checks := e.architectureChecks(ctx, task, p, nil)
	if len(checks) != 1 || !strings.Contains(checks[0].Cmd, "-force -only-docs") {
		t.Fatalf("проверки задачи описания: %+v", checks)
	}
	// Описания нет: обычной задаче проверка не добавляется.
	if checks := e.architectureChecks(ctx, domain.Task{Kind: domain.KindWork}, p, nil); len(checks) != 0 {
		t.Fatalf("проверки без описания: %+v", checks)
	}

	// Повторный повод: автоописание уже срабатывало.
	if _, taskID, err := e.MaybeAutoDescribe(ctx, p.ID, "epic_start"); err != nil || taskID != "" {
		t.Fatalf("повторное автоописание: %q %v", taskID, err)
	}
	skips := archEvents(t, st, p.ID, "architecture.describe_skipped")
	if len(skips) != 1 || skips[0].Payload["reason"] != "already_ran" {
		t.Fatalf("пропуск автоописания: %+v", skips)
	}
	// Ручной запуск при идущей задаче отклоняется.
	_, _, err = e.DescribeArchitecture(ctx, p.ID, "manual", "owner")
	var active *store.ErrArchitectureTaskActive
	if !errors.As(err, &active) || active.TaskID != info.TaskID {
		t.Fatalf("вторая задача описания: %v", err)
	}
}

// Пустой репозиторий: снимок absent, автоописание пропускается.
func TestAutoDescribeEmptyRepo(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{headErr: scm.ErrBranchNotFound}
	p, e := archProject(t, st, sc)
	if err := e.ConnectArchitectureNow(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.ArchitectureTask(ctx, p.ID); ok {
		t.Fatal("задача описания для пустого репозитория")
	}
	skips := archEvents(t, st, p.ID, "architecture.describe_skipped")
	if len(skips) != 1 || skips[0].Payload["reason"] != "empty_repo" {
		t.Fatalf("пропуск для пустого репозитория: %+v", skips)
	}
	// Спека «Пустой репозиторий»: задача ставится при запуске первого Epic.
	if _, taskID, err := e.MaybeAutoDescribe(ctx, p.ID, "epic_start"); err != nil || taskID == "" {
		t.Fatalf("автоописание при запуске Epic: %q %v", taskID, err)
	}
}

// Проект, подключённый до обновления установки, снимка не имеет: запуск
// Epic строит снимок в фоне и после него ставит задачу (design, риск
// «Автоописание тратит токены в проектах, подключённых до релиза»).
func TestAutoDescribeEpicStartWithoutSnapshot(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	p, e := archProject(t, st, &fakeSCM{})
	if _, taskID, err := e.MaybeAutoDescribe(ctx, p.ID, "epic_start"); err != nil || taskID != "" {
		t.Fatalf("запуск Epic без снимка не ждёт описания: %q %v", taskID, err)
	}
	e.WaitArchitecture()
	info, ok, err := st.ArchitectureTask(ctx, p.ID)
	if err != nil || !ok || info.Trigger != "epic_start" {
		t.Fatalf("задача после снимка: %+v %v %v", info, ok, err)
	}

	// Чтение не удалось: задачу вслепую не ставим.
	p2, e2 := archProject(t, st, &fakeSCM{})
	if _, _, err := st.SaveProjectArchitecture(ctx, p2.ID, store.ArchitectureSave{Status: "error", Error: "503"}); err != nil {
		t.Fatal(err)
	}
	if _, taskID, err := e2.MaybeAutoDescribe(ctx, p2.ID, "epic_start"); err != nil || taskID != "" {
		t.Fatalf("автоописание при ошибке чтения: %q %v", taskID, err)
	}
	if done, _ := st.ArchitectureAutoDone(ctx, p2.ID); done {
		t.Fatal("отметка поставлена без задачи")
	}
}

// Причины пропуска автоописания: пресет выключен, описание уже есть.
func TestMaybeAutoDescribeReasons(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	p, e := archProject(t, st, sc)

	// Описание есть.
	if _, _, err := st.SaveProjectArchitecture(ctx, p.ID, store.ArchitectureSave{Status: "invalid", SHA: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, taskID, _ := e.MaybeAutoDescribe(ctx, p.ID, "epic_start"); taskID != "" {
		t.Fatal("автоописание при готовом описании")
	}

	// Пресет выключен в политике проекта.
	off := false
	if _, err := st.SaveProjectPolicy(ctx, p.ID, policy.Overrides{AutoDescribeArchitecture: &off}, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SaveProjectArchitecture(ctx, p.ID, store.ArchitectureSave{Status: "absent"}); err != nil {
		t.Fatal(err)
	}
	if _, taskID, _ := e.MaybeAutoDescribe(ctx, p.ID, "connect"); taskID != "" {
		t.Fatal("автоописание при выключенном пресете")
	}

	var reasons []string
	for _, ev := range archEvents(t, st, p.ID, "architecture.describe_skipped") {
		reasons = append(reasons, ev.Payload["reason"].(string))
	}
	if strings.Join(reasons, ",") != "has_description,preset_off" {
		t.Fatalf("причины пропуска: %v", reasons)
	}

	// Ручной запуск пресет не проверяет.
	if _, taskID, err := e.DescribeArchitecture(ctx, p.ID, "manual", "owner"); err != nil || taskID == "" {
		t.Fatalf("ручной запуск: %q %v", taskID, err)
	}
}
