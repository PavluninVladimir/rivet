package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/blob"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func (c *capture) lastDeploy(t *testing.T) *pb.DeployJob {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.sent) - 1; i >= 0; i-- {
		if d := c.sent[i].GetDeploy(); d != nil {
			return d
		}
	}
	t.Fatal("DeployJob не отправлен")
	return nil
}

// seedDeploy — проект с окружением и deploy-runner'ом.
func seedDeploy(t *testing.T, st *store.Store, e *Engine, trigger string) (domain.User, domain.Environment) {
	t.Helper()
	ctx := context.Background()
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "staging", ExecType: "ssh", Trigger: trigger,
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Agent: "wrap", Capabilities: []string{"deploy"}}); err != nil {
		t.Fatal(err)
	}
	return owner, env
}

func deploymentStatus(t *testing.T, st *store.Store, depID string) string {
	t.Helper()
	d, err := st.GetDeployment(context.Background(), depID)
	if err != nil {
		t.Fatal(err)
	}
	return d.Status
}

// Happy path: queued → deploying (джоба runner'у) → verifying → done,
// runner освобождён; stale-replay результата после финала отбрасывается.
func TestDeployPipelineAndStaleReplay(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_, env := seedDeploy(t, st, e, "manual")

	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	job := out.lastDeploy(t)
	if job.DeploymentId != dep.ID || job.Version != "sha-1" || job.DeployCmd != "true" {
		t.Fatalf("джоба: %+v", job)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "deploying" {
		t.Fatalf("want deploying, got %s", got)
	}

	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: dep.ID, Stage: pb.DeployResult_DEPLOY, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "verifying" {
		t.Fatalf("want verifying, got %s", got)
	}
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: dep.ID, Stage: pb.DeployResult_VERIFY, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "done" {
		t.Fatalf("want done, got %s", got)
	}
	runners, _ := st.ListRunners(ctx)
	if runners[0].Status != domain.RunnerIdle {
		t.Fatalf("runner должен быть idle: %+v", runners[0])
	}

	// Stale-replay провала после финала не меняет публикацию и не паузит env.
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: dep.ID, Stage: pb.DeployResult_VERIFY, Ok: false, Detail: "stale"}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "done" {
		t.Fatalf("stale-результат изменил публикацию: %s", got)
	}
	got, _ := st.GetEnvironment(ctx, env.ID)
	if got.Paused {
		t.Fatal("stale-провал не должен паузить окружение")
	}
	// Чужой runner тоже не проходит.
	if err := e.OnDeployResult(ctx, "intruder", &pb.DeployResult{
		DeploymentId: dep.ID, Stage: pb.DeployResult_VERIFY, Ok: false}); err != nil {
		t.Fatal(err)
	}
}

// Провал Verify без предыдущей успешной версии: сразу failed, окружение на
// паузе, эскалация DEPLOY_FAILED без задачи, runner свободен.
func TestDeployFailNoRollbackTarget(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	owner, env := seedDeploy(t, st, e, "manual")

	dep, _ := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: dep.ID, Stage: pb.DeployResult_VERIFY, Ok: false, Detail: "health-check упал"}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "failed" {
		t.Fatalf("want failed, got %s", got)
	}
	envAfter, _ := st.GetEnvironment(ctx, env.ID)
	if !envAfter.Paused {
		t.Fatal("окружение должно встать на паузу")
	}
	atts, _ := st.ListAttention(ctx, owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttDeployFailed || atts[0].DeploymentID != dep.ID || atts[0].TaskID != "" {
		t.Fatalf("ожидалась эскалация DEPLOY_FAILED по публикации: %+v", atts)
	}
	if !strings.Contains(atts[0].Message, "health-check упал") {
		t.Fatalf("в эскалации нет причины: %q", atts[0].Message)
	}
	runners, _ := st.ListRunners(ctx)
	if runners[0].Status != domain.RunnerIdle {
		t.Fatalf("runner должен быть idle: %+v", runners[0])
	}
}

// Провал после успешной публикации: одна попытка отката к предыдущей версии,
// её успех — статус rolled_back, окружение на паузе, эскалация; resume
// возобновляет и планировщик подхватывает queued.
func TestDeployFailRollsBackToPrevious(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_, env := seedDeploy(t, st, e, "manual")

	// Успешная публикация sha-1.
	first, _ := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_ = e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: first.ID, Stage: pb.DeployResult_DEPLOY, Ok: true})
	_ = e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: first.ID, Stage: pb.DeployResult_VERIFY, Ok: true})

	// Вторая публикация проваливает deploy → откат к sha-1.
	second, _ := st.EnqueueDeployment(ctx, env.ID, "sha-2", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: second.ID, Stage: pb.DeployResult_DEPLOY, Ok: false, Detail: "docker упал"}); err != nil {
		t.Fatal(err)
	}
	rb := out.lastDeploy(t)
	if !rb.Rollback || rb.Version != "sha-1" || rb.PrevVersion != "sha-2" || rb.DeploymentId != second.ID {
		t.Fatalf("ожидалась откат-джоба к sha-1: %+v", rb)
	}
	// Откат доставлен и проверен (результаты несут эхо rollback-фазы);
	// повтор исходного провала при этом отбрасывается guard'ом фазы.
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: second.ID, Stage: pb.DeployResult_DEPLOY, Ok: false, Detail: "поздний повтор"}); err != nil {
		t.Fatal(err)
	}
	_ = e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: second.ID, Stage: pb.DeployResult_DEPLOY, Ok: true, Rollback: true})
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: second.ID, Stage: pb.DeployResult_VERIFY, Ok: true, Rollback: true}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, second.ID); got != "rolled_back" {
		t.Fatalf("want rolled_back, got %s", got)
	}
	dep, _ := st.GetDeployment(ctx, second.ID)
	if !strings.Contains(dep.Detail, "docker упал") || !strings.Contains(dep.Detail, "откат") {
		t.Fatalf("detail должен нести провал и итог отката: %q", dep.Detail)
	}
	envAfter, _ := st.GetEnvironment(ctx, env.ID)
	if !envAfter.Paused {
		t.Fatal("после провала окружение на паузе")
	}

	// Пауза не даёт запускать queued; resume — планировщик подхватывает.
	third, _ := st.EnqueueDeployment(ctx, env.ID, "sha-3", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, third.ID); got != "queued" {
		t.Fatalf("на паузе публикация не должна стартовать: %s", got)
	}
	if err := st.SetEnvPaused(ctx, env.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, third.ID); got != "deploying" {
		t.Fatalf("после resume публикация должна стартовать: %s", got)
	}
}

// Потеря deploy-runner'а (heartbeat) и watchdog по времени проваливают
// активную публикацию полной цепочкой.
func TestDeployRunnerLostAndWatchdog(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	owner, env := seedDeploy(t, st, e, "manual")

	dep, _ := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Runner замолкает.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE runners SET last_seen = now() - interval '10 minutes' WHERE id='deployer'`); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "failed" {
		t.Fatalf("want failed после потери runner'а, got %s", got)
	}
	atts, _ := st.ListAttention(ctx, owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttDeployFailed {
		t.Fatalf("ожидалась эскалация DEPLOY_FAILED: %+v", atts)
	}

	// Watchdog: живой runner, но джоба зависла дольше дедлайна.
	if err := st.SetEnvPaused(ctx, env.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Agent: "wrap", Capabilities: []string{"deploy"}}); err != nil {
		t.Fatal(err)
	}
	dep2, _ := st.EnqueueDeployment(ctx, env.ID, "sha-2", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET started_at = now() - interval '2 hours' WHERE id=$1`, dep2.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep2.ID); got != "failed" {
		t.Fatalf("watchdog должен провалить зависшую публикацию: %s", got)
	}
}

// Merge задачи ставит автопубликации проекта; повторный merge коалесцируется
// в одну queued с новой версией.
func TestMergeEnqueuesAutoDeploysCoalesced(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)

	owner := mustOwner(t, st)
	p, _ := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "staging", ExecType: "ssh", Trigger: "auto",
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Две зависимые задачи доходят до merge по очереди (без deploy-runner'а —
	// публикации копятся в очереди).
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: "wrap", Capabilities: []string{"coding"}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: "wrap", Capabilities: []string{"coding", "review"}})
	taskA, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "B", Deps: []string{taskA.ID}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.TransitionEpic(ctx, epic.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, Type: "epic.status"})

	mergeOne := func(task domain.Task) {
		t.Helper()
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		workerID := mustTaskRunner(t, st, task.ID)
		_ = e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true})
		_ = e.OnStageResult(ctx, workerID, &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true})
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		_ = e.OnStageResult(ctx, "reviewer", &pb.StageResult{TaskId: task.ID, SessionId: out.lastAssign(t).SessionId, Stage: pb.StageResult_REVIEW, Ok: true})
		if err := e.MergeTask(ctx, task.ID, owner.Login); err != nil {
			t.Fatal(err)
		}
	}
	mergeOne(taskA)
	mergeOne(taskB)

	deps, err := st.ListDeployments(ctx, env.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].Status != "queued" || deps[0].Initiator != "auto" {
		t.Fatalf("ожидалась одна queued-автопубликация: %+v", deps)
	}
	if !strings.HasPrefix(deps[0].Version, "sha-merge-") {
		t.Fatalf("версия должна быть sha merge-коммита: %q", deps[0].Version)
	}
	// Версии двух merge'ей различаются — в очереди последняя.
	if deps[0].Version == "sha-merge-0" {
		t.Fatalf("неожиданная версия: %q", deps[0].Version)
	}
}

// Сохранённый лог публикации замаскирован (redact при flush в blob);
// без MinIO тест пропускается.
func TestDeployLogMaskedInBlob(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	endpoint := os.Getenv("RIVET_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	bl, err := blob.New(endpoint, "rivet", "rivetsecret", "rivet-test", false)
	if err != nil {
		t.Skipf("minio недоступен: %v", err)
	}
	if err := bl.EnsureBucket(ctx); err != nil {
		t.Skipf("minio недоступен: %v", err)
	}
	out := &capture{}
	e := New(st, &fakeSCM{}, bl, out, 90*time.Second)
	_, env := seedDeploy(t, st, e, "manual")

	dep, _ := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	e.OnDeployTranscript(dep.ID, []byte("вывод деплоя: export TOKEN=ghp_deploySecret0123456789\n"))
	_ = e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: dep.ID, Stage: pb.DeployResult_DEPLOY, Ok: true})
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: dep.ID, Stage: pb.DeployResult_VERIFY, Ok: true}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDeployment(ctx, dep.ID)
	if err != nil || got.LogRef == "" {
		t.Fatalf("лог не сохранён: %+v %v", got, err)
	}
	data, err := bl.Get(ctx, got.LogRef)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghp_deploySecret0123456789") {
		t.Fatalf("секрет утёк в сохранённый лог: %q", data)
	}
	if !strings.Contains(string(data), "***") || !strings.Contains(string(data), "вывод деплоя") {
		t.Fatalf("лог без маски или без содержимого: %q", data)
	}
}

// Фаза отката durable: рестарт rivetd между провалом и результатом отката
// не превращает откат в done проваленной версии.
func TestRollbackSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_, env := seedDeploy(t, st, e, "manual")

	first, _ := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_ = e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: first.ID, Stage: pb.DeployResult_DEPLOY, Ok: true})
	_ = e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: first.ID, Stage: pb.DeployResult_VERIFY, Ok: true})

	second, _ := st.EnqueueDeployment(ctx, env.ID, "sha-2", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: second.ID, Stage: pb.DeployResult_DEPLOY, Ok: false, Detail: "упало"}); err != nil {
		t.Fatal(err)
	}

	// «Рестарт»: новый Engine, память отката пуста — фаза читается из БД.
	e2 := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_ = e2.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: second.ID, Stage: pb.DeployResult_DEPLOY, Ok: true, Rollback: true})
	if got := deploymentStatus(t, st, second.ID); got == "verifying" {
		t.Fatal("deploy ok отката не должен переводить в verifying")
	}
	if err := e2.OnDeployResult(ctx, "deployer", &pb.DeployResult{
		DeploymentId: second.ID, Stage: pb.DeployResult_VERIFY, Ok: true, Rollback: true}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, second.ID); got != "rolled_back" {
		t.Fatalf("после рестарта откат должен закончиться rolled_back, got %s", got)
	}
}

// Reconnect deploy-runner'а проваливает его активную публикацию сразу
// (деплой-goroutine прежней сессии мертва, результата не будет).
func TestDeployRunnerReconnectFailsActive(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	_, env := seedDeploy(t, st, e, "manual")

	dep, _ := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Reconnect: путь Register — активная публикация проваливается, runner чист.
	depID, err := st.RunnerActiveDeployment(ctx, "deployer")
	if err != nil || depID != dep.ID {
		t.Fatalf("активная публикация runner'а: %q, %v", depID, err)
	}
	if err := e.FailDeploymentNow(ctx, depID, "runner переподключился"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Agent: "wrap", Capabilities: []string{"deploy"}}); err != nil {
		t.Fatal(err)
	}
	if got := deploymentStatus(t, st, dep.ID); got != "failed" {
		t.Fatalf("want failed после reconnect, got %s", got)
	}
	if depID, _ := st.RunnerActiveDeployment(ctx, "deployer"); depID != "" {
		t.Fatalf("deployment_id должен очиститься: %q", depID)
	}
}
