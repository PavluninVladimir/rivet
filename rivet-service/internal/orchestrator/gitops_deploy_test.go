package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Публикация через GitOps (change add-gitops-delivery): коммит версии как
// этап Deploy, ожидание синхронизации как Verify.

// Подстановка версии: файл целиком и значение ключа YAML.
func TestApplyVersion(t *testing.T) {
	if got, err := applyVersion("old\n", "", "sha-1"); err != nil || got != "sha-1\n" {
		t.Fatalf("файл целиком: %q %v", got, err)
	}
	src := "# конфигурация\nimage:\n  repo: registry/app  # реестр\n  tag: v1\nreplicas: 2\n"
	got, err := applyVersion(src, "image.tag", "v2")
	if err != nil {
		t.Fatal(err)
	}
	want := "# конфигурация\nimage:\n  repo: registry/app  # реестр\n  tag: v2\nreplicas: 2\n"
	if got != want {
		t.Fatalf("значение ключа:\n%q\nожидали\n%q", got, want)
	}
	// Ключ верхнего уровня.
	if got, err := applyVersion(src, "replicas", "3"); err != nil || !strings.Contains(got, "replicas: 3") {
		t.Fatalf("ключ верхнего уровня: %q %v", got, err)
	}
	// Ключа нет — это ошибка конфигурации, а не тихая дописка.
	if _, err := applyVersion(src, "image.digest", "x"); err == nil {
		t.Fatal("отсутствующий ключ должен давать ошибку")
	}
}

// versionServer — окружение, которое начинает отвечать версией после
// нескольких опросов (контроллер синхронизируется не мгновенно).
func versionServer(t *testing.T, version string, after int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) > after {
			_, _ = w.Write([]byte(`{"version":"` + version + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"version":"старая"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func seedGitOps(t *testing.T, st *store.Store, verifyURL, key string) (domain.Project, domain.Environment) {
	t.Helper()
	ctx := context.Background()
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "prod", ExecType: domain.ExecGitOps, Trigger: "manual",
		Config: domain.EnvConfig{File: "envs/prod/values.yaml", Key: key,
			Ref: "main", VerifyURL: verifyURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, env
}

// Публикация: коммит версии, ожидание синхронизации, done. Повторный тик
// второго коммита не делает.
func TestGitOpsDeploySuccess(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	// Первые два опроса окружение ещё на старой версии: первый делает тот
	// же тик, что и коммит.
	srv, _ := versionServer(t, "sha-1", 2)
	sc := &fakeSCM{}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedGitOps(t, st, srv.URL, "")

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	commits := len(sc.commits)
	content := sc.files["o/r@main:envs/prod/values.yaml"]
	sc.mu.Unlock()
	if commits != 1 || strings.TrimSpace(content) != "sha-1" {
		t.Fatalf("коммит версии: %d %q", commits, content)
	}
	got, err := st.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "deploying" || got.ExternalURL == "" {
		t.Fatalf("после коммита: %+v", got)
	}

	// Окружение ещё на старой версии: публикация ждёт.
	advance(e, externalPollInterval+time.Second)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if s := deploymentStatus(t, st, dep.ID); s != "deploying" {
		t.Fatalf("до синхронизации: %s", s)
	}
	sc.mu.Lock()
	commits = len(sc.commits)
	sc.mu.Unlock()
	if commits != 1 {
		t.Fatalf("второй коммит не нужен: %d", commits)
	}

	// Синхронизировалось — публикация завершается.
	advance(e, 2*externalPollInterval)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if s := deploymentStatus(t, st, dep.ID); s != "done" {
		t.Fatalf("после синхронизации: %s", s)
	}
}

// Версия уже в конфигурации: коммита нет, публикация сразу ждёт факта.
func TestGitOpsIdempotentCommit(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv, _ := versionServer(t, "sha-1", 0)
	sc := &fakeSCM{files: map[string]string{"o/r@main:envs/prod/values.yaml": "sha-1\n"}}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedGitOps(t, st, srv.URL, "")

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	commits := len(sc.commits)
	sc.mu.Unlock()
	if commits != 0 {
		t.Fatalf("повторный коммит той же версии не нужен: %d", commits)
	}
	advance(e, externalPollInterval+time.Second)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if s := deploymentStatus(t, st, dep.ID); s != "done" {
		t.Fatalf("публикация должна завершиться: %s", s)
	}
}

// Ключ в конфигурации не найден: публикация проваливается с понятной
// причиной, а не коммитит мусор.
func TestGitOpsMissingKey(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv, _ := versionServer(t, "sha-1", 0)
	sc := &fakeSCM{files: map[string]string{"o/r@main:envs/prod/values.yaml": "replicas: 2\n"}}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedGitOps(t, st, srv.URL, "image.tag")

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
	if got.Status != "failed" || !strings.Contains(got.Detail, "image.tag") {
		t.Fatalf("публикация должна провалиться с причиной: %+v", got)
	}
	sc.mu.Lock()
	commits := len(sc.commits)
	sc.mu.Unlock()
	if commits != 0 {
		t.Fatalf("коммита быть не должно: %d", commits)
	}
}

// Конфигурация GitOps проверяется: без адреса окружения ждать нечего.
func TestGitOpsConfigValidation(t *testing.T) {
	bad := []domain.EnvConfig{
		{File: "values.yaml"},      // нет verify_url
		{VerifyURL: "https://x/v"}, // нет файла
		{File: "values.yaml", VerifyURL: "https://x/v", VerifyCmd: "true"}, // команду выполнять негде
		{File: "../secrets", VerifyURL: "https://x/v"},                     // выход за корень
		{File: "values.yaml", VerifyURL: "https://x/v", Key: "image tag"},  // пробел в ключе
		{File: "values.yaml", VerifyURL: "https://x/v", Repo: "не/репо/;"}, // мусор в репозитории
		{File: "values.yaml", VerifyURL: "https://x/v", DeployCmd: "make"}, // команды нет
	}
	for i, cfg := range bad {
		if err := cfg.Validate(domain.ExecGitOps); err == nil {
			t.Fatalf("конфигурация #%d должна отклоняться: %+v", i, cfg)
		}
	}
	ok := domain.EnvConfig{File: "envs/prod/values.yaml", Key: "image.tag",
		Repo: "org/config", Ref: "main", VerifyURL: "https://prod/version"}
	if err := ok.Validate(domain.ExecGitOps); err != nil {
		t.Fatalf("нормальная конфигурация: %v", err)
	}
}

// Отступы файла берутся из него самого, инлайн-комментарий сохраняется.
func TestApplyVersionIndentAndComments(t *testing.T) {
	four := "image:\n    repo: registry/app\n    tag: v1  # текущая\nreplicas: 2\n"
	got, err := applyVersion(four, "image.tag", "v2")
	if err != nil {
		t.Fatal(err)
	}
	want := "image:\n    repo: registry/app\n    tag: v2  # текущая\nreplicas: 2\n"
	if got != want {
		t.Fatalf("отступ 4 пробела:\n%q\nожидали\n%q", got, want)
	}
	// Ключ с таким же именем в другой ветке не путается.
	multi := "app:\n  image:\n    tag: v1\nsidecar:\n  image:\n    tag: v9\n"
	got, err = applyVersion(multi, "sidecar.image.tag", "v10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "    tag: v1\n") || !strings.Contains(got, "    tag: v10\n") {
		t.Fatalf("вложенные ключи:\n%s", got)
	}
}

// Публикация повторяет коммит, если он не был записан (падение между
// захватом и записью), а не ждёт таймаута впустую.
func TestGitOpsRecommitsAfterLostWrite(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv, _ := versionServer(t, "sha-1", 1)
	sc := &fakeSCM{}
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	_, env := seedGitOps(t, st, srv.URL, "")

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	// Публикация захвачена, но коммита нет: имитируем падение rivetd между
	// захватом и записью файла.
	if _, _, err := st.StartNextExternalDeployment(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimExternalTrigger(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	commits := len(sc.commits)
	sc.mu.Unlock()
	if commits != 1 {
		t.Fatalf("потерянный коммит должен повториться: %d", commits)
	}
	got, err := st.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalURL == "" {
		t.Fatalf("ссылка на коммит должна появиться: %+v", got)
	}
}
