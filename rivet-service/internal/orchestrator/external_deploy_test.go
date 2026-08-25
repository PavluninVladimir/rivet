package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Публикация через внешний пайплайн (change add-external-delivery, спека
// deployment «Дирижирование внешними системами доставки»).

// seedExternal — проект с окружением внешней доставки; deploy-runner'ов в
// установке нет намеренно: внешняя публикация их не ждёт.
func seedExternal(t *testing.T, st *store.Store, verifyURL string) (domain.Project, domain.Environment) {
	t.Helper()
	ctx := context.Background()
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "prod", ExecType: domain.ExecPipeline, Trigger: "manual",
		Config: domain.EnvConfig{Pipeline: "deploy.yml", Ref: "main",
			Vars: map[string]string{"STACK": "prod"}, VerifyURL: verifyURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, env
}

// advance — время тика вперёд: опрос прогона не чаще раза в интервал.
func advance(e *Engine, d time.Duration) {
	now := e.Now()
	e.Now = func() time.Time { return now.Add(d) }
}

// Успешный прогон: триггер с версией и окружением в переменных, ссылка на
// прогон в событии, Verify по URL, публикация done.
func TestExternalDeploySuccess(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	verified := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { verified++ }))
	defer srv.Close()
	sc := &fakeSCM{}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedExternal(t, st, srv.URL)

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	triggers := append([]pipelineCall(nil), sc.triggers...)
	sc.mu.Unlock()
	if len(triggers) != 1 {
		t.Fatalf("ожидали один запуск пайплайна: %+v", triggers)
	}
	if triggers[0].Pipeline != "deploy.yml" || triggers[0].Ref != "main" {
		t.Fatalf("параметры запуска: %+v", triggers[0])
	}
	if triggers[0].Vars["RIVET_VERSION"] != "sha-1" || triggers[0].Vars["RIVET_ENV"] != "prod" ||
		triggers[0].Vars["STACK"] != "prod" {
		t.Fatalf("переменные прогона: %+v", triggers[0].Vars)
	}
	got, err := st.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "deploying" || got.ExternalRunID != "run-1" || got.ExternalURL != "https://ci/run-1" {
		t.Fatalf("публикация после запуска: %+v", got)
	}

	// Прогон ещё идёт: следующий опрос ничего не меняет и второй запуск
	// не делает.
	advance(e, externalPollInterval+time.Second)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	n := len(sc.triggers)
	sc.runState = scm.PipelineSuccess
	sc.mu.Unlock()
	if n != 1 {
		t.Fatalf("повторный тик не должен запускать второй прогон: %d", n)
	}
	if deploymentStatus(t, st, dep.ID) != "deploying" {
		t.Fatalf("статус во время прогона: %s", deploymentStatus(t, st, dep.ID))
	}

	advance(e, 2*externalPollInterval)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "done" {
		t.Fatalf("публикация должна завершиться: %s", got)
	}
	if verified == 0 {
		t.Fatal("Verify по URL не выполнен")
	}
	// Ссылка на прогон видна в событиях публикации.
	evs, err := st.Events(ctx, store.EventFilter{Type: "deploy.status", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var withURL bool
	for _, ev := range evs {
		if ev.Payload["external_url"] == "https://ci/run-1" {
			withURL = true
		}
	}
	if !withURL {
		t.Fatalf("ссылка на прогон должна попасть в событие: %+v", evs)
	}
}

// Провал прогона без предыдущей успешной версии: публикация failed,
// окружение на паузе, человеку эскалация.
func TestExternalDeployFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	sc := &fakeSCM{}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedExternal(t, st, srv.URL)

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	sc.runState = scm.PipelineFailed
	sc.mu.Unlock()
	advance(e, externalPollInterval+time.Second)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "failed" {
		t.Fatalf("публикация должна провалиться: %s", got)
	}
	gotEnv, err := st.GetEnvironment(ctx, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotEnv.Paused {
		t.Fatal("окружение должно встать на паузу")
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM attention WHERE reason='DEPLOY_FAILED' AND status <> 'resolved'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("эскалация публикации: %d", n)
	}
}

// Verify не прошёл: публикация не считается успешной.
func TestExternalDeployVerifyFails(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "не готово", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	sc := &fakeSCM{runState: scm.PipelineSuccess}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedExternal(t, st, srv.URL)

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	advance(e, externalPollInterval+time.Second)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "failed" {
		t.Fatalf("провал Verify: %s", got)
	}
}

// Ошибка запуска пайплайна проваливает публикацию с понятной причиной.
func TestExternalDeployTriggerError(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	sc := &fakeSCM{triggerErr: errTrigger}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedExternal(t, st, srv.URL)

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("публикация должна провалиться: %s", got.Status)
	}
	if got.Detail == "" {
		t.Fatal("причина провала должна сохраниться")
	}
}

// Запущенный, но ещё не найденный прогон (GitHub не возвращает id на
// workflow_dispatch) не приводит ко второму запуску пайплайна.
func TestExternalDeployPendingRunNotRetriggered(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	sc := &fakeSCM{noRunID: true}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedExternal(t, st, srv.URL)

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalRunID != store.ExternalRunPending {
		t.Fatalf("прогон без идентификатора должен помечаться pending: %q", got.ExternalRunID)
	}
	advance(e, externalPollInterval+time.Second)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	n := len(sc.triggers)
	sc.mu.Unlock()
	if n != 1 {
		t.Fatalf("пайплайн не должен запускаться второй раз: %d", n)
	}
}

// Конфигурацию окружения нельзя менять под идущей публикацией: проверку
// делает сам UPDATE, окна между проверкой и записью нет.
func TestEnvConfigLockedDuringDeployment(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	e := New(st, &fakeSCM{}, nil, &capture{}, 90*time.Second)
	_, env := seedExternal(t, st, srv.URL)
	if _, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova"); err != nil {
		t.Fatal(err)
	}
	active, err := st.HasActiveDeployment(ctx, env.ID)
	if err != nil || !active {
		t.Fatalf("публикация в очереди считается активной: %v %v", err, active)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if active, err = st.HasActiveDeployment(ctx, env.ID); err != nil || !active {
		t.Fatalf("идущая публикация считается активной: %v %v", err, active)
	}
	// Правка конфигурации под публикацией отклоняется.
	env.Config.VerifyURL = srv.URL + "/other"
	if _, err := st.UpdateEnvironment(ctx, env); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("правка под публикацией должна быть конфликтом: %v", err)
	}
	// После финала публикации правка проходит.
	if _, err := st.FinishDeployment(ctx, dep(t, st, env.ID), "", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateEnvironment(ctx, env); err != nil {
		t.Fatalf("после публикации правка должна проходить: %v", err)
	}
}

// dep — идентификатор последней публикации окружения.
func dep(t *testing.T, st *store.Store, envID string) string {
	t.Helper()
	d, err := st.LastDeployment(context.Background(), envID)
	if err != nil {
		t.Fatal(err)
	}
	return d.ID
}
