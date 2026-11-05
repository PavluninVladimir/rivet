package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Находки ревью add-agent-roles в оркестраторе: недоставленное назначение,
// сбой инфраструктуры тестировщика, подписи исхода, переиспользование
// освобождённого runner'а, перезапуск runner'а, подключение агента.

// downSender — runner недоступен: Assignment не доставляется.
type downSender struct{ capture }

func (d *downSender) Send(runnerID string, msg *pb.PlaneMsg) bool {
	d.capture.Send(runnerID, msg)
	return false
}

// Недоставленное назначение: запуск отвязан от runner'а и агента, runner
// свободен, сессия закрыта (P2-6).
func TestUndeliveredAssignmentReleasesAgent(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""})
	down := &downSender{}
	f.e.Out = down
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(down.sent) == 0 {
		t.Fatal("назначение должно было уйти")
	}
	agent, _ := f.st.GetTeamAgent(ctx, "maks")
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if agent.TaskID != "" || task.AgentID != "" {
		t.Fatalf("агент свободен после недоставки: %+v %+v", agent.TaskID, task.AgentID)
	}
	runners, _ := f.st.ListRunners(ctx)
	for _, r := range runners {
		if r.TaskID != "" {
			t.Fatalf("runner свободен после недоставки: %+v", r)
		}
	}
}

// Сбой инфраструктуры тестировщика: один повтор без расхода попытки,
// повторный сбой эскалирует (P3-4 оркестратора).
func TestTesterInfraErrorRetriesThenEscalates(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, f)
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: store.AgentRivet, Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("тестировщик: %+v", as)
	}
	before, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: "error", Detail: "git: push отклонён"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskTesting || task.AttemptUsed != before.AttemptUsed || task.StepRejections["test"] != before.StepRejections["test"] {
		t.Fatalf("повтор без расхода попытки: %s %d→%d", task.Status, before.AttemptUsed, task.AttemptUsed)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.Stage != pb.StageResult_TESTING {
		t.Fatalf("шаг test назначен заново: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: "error", Detail: "git: push отклонён"}); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFailed {
		t.Fatalf("повторный сбой эскалирует: %s", got)
	}
}

// driveToTest — задача прошла код и стоит на назначенном шаге test.
func driveToTest(t *testing.T, f policyFixture) *pb.Assignment {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: store.AgentRivet, Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	return f.out.lastAssign(t)
}

func lastStatusText(t *testing.T, f policyFixture) string {
	t.Helper()
	evs := eventsOfType(t, f.st, f.task.ID, "task.status")
	return evs[len(evs)-1].Text
}

// Подписи исхода test: тестировщик одобрил, а проверки красные — «проверки
// упали»; runner без ролей — без отчёта тестировщика (P3-2).
func TestTesterLabelsFollowAgentVerdict(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, f)
	as := driveToTest(t, f)
	if err := f.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Verdict: "ok", Detail: "## Тестировщик\nкритерии проверены\n\n## Проверки\nFAIL"}); err != nil {
		t.Fatal(err)
	}
	if text := lastStatusText(t, f); !strings.Contains(text, "проверки упали") {
		t.Fatalf("одобрение тестировщика при красных проверках: %q", text)
	}
	if ctxText := f.e.peekStageContext(f.task.ID); !strings.HasPrefix(ctxText, "Замечания тестировщика и вывод проверок:") {
		t.Fatalf("контекст исправления с отчётом тестировщика: %q", ctxText)
	}

	g := newPolicyFixture(t, "diff --git a/x b/x\n")
	withTester(t, g)
	as = driveToTest(t, g)
	// Старый runner: роль не поддерживает, вердикта агента нет.
	if err := g.e.OnStageResult(ctx, "worker", &pb.StageResult{TaskId: g.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_TESTING, Ok: false, Detail: "== tests ==\nFAIL"}); err != nil {
		t.Fatal(err)
	}
	if text := lastStatusText(t, g); !strings.Contains(text, "проверки упали") {
		t.Fatalf("старый runner: %q", text)
	}
	if ctxText := g.e.peekStageContext(g.task.ID); !strings.HasPrefix(ctxText, "Вывод проверок:") {
		t.Fatalf("старый runner — только вывод проверок: %q", ctxText)
	}
}

// Освобождённый runner не переиспользуется: его могли отдать другой задаче
// между решением и входом на шаг (P2-3).
func TestReuseRequiresRunnerStillBound(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	proc, _, err := f.e.processFor(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	test, _ := proc.Step("test")
	if !f.e.reuseTarget(ctx, task, test, "worker") {
		t.Fatal("runner за задачей переиспользуется")
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET task_id=NULL, status='idle' WHERE id='worker'`); err != nil {
		t.Fatal(err)
	}
	if f.e.reuseTarget(ctx, task, test, "worker") {
		t.Fatal("освобождённый runner не переиспользуется")
	}
}

// Перезапуск runner'а: открытые запуски снимаются, агент свободен, запуск
// назначается заново (P2-6).
func TestRunnerRestartReassigns(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	withTeam(t, f, [4]string{"maks", "Макс", "developer", ""})
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	first := f.out.lastAssign(t)
	if err := f.e.ReleaseRunnerRuns(ctx, "worker", nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.st.GetTeamAgent(ctx, "maks"); a.TaskID != "" {
		t.Fatal("агент свободен после перезапуска runner'а")
	}
	if f.e.SessionMatches(ctx, f.task.ID, first.SessionId) {
		t.Fatal("сессия прежнего процесса закрыта")
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET status='idle', task_id=NULL WHERE id='worker'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if again := f.out.lastAssign(t); again.SessionId == first.SessionId || again.GetRole().GetName() != "Макс" {
		t.Fatalf("назначение заново: %+v", again)
	}
}

// Подключение агента команды едет в назначение парой (P3-3, спека agents
// «Модель из двух подключений» и «Доставка модели и учётных данных
// runner'у»): runner агента Rivet (feature native) получает endpoint пары
// агента команды, а не модели по умолчанию профиля или шаблона роли.
// Окружения CLI в назначении нет, ключ и секретные заголовки — только по
// режиму секретов профиля и каналу runner'а, доставка — в event log без
// значений.
func TestAgentConnectionInAssignment(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "")
	owner := f.owner
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	f.e.Box = box
	key, org, token := "b-key-1234567890", "acme", "hdr-secret-123456"
	for _, c := range []store.ConnectionInput{
		{ID: "a", Name: "A", Kind: "local", API: "openai", BaseURL: "http://a/v1"},
		{ID: "b", Name: "B", Kind: "local", API: "openai", BaseURL: "http://b/v1", Key: &key,
			Headers: &[]store.HeaderInput{{Name: "X-Org", Value: &org}, {Name: "X-Token", Value: &token, Secret: true}}},
	} {
		if _, _, err := f.st.UpsertConnection(ctx, c, box, owner.ID, owner.Login); err != nil {
			t.Fatal(err)
		}
	}
	window, in, out, cached := int64(128000), int64(270000), int64(1100000), int64(70000)
	maxOut, modelMax := int64(32768), int64(65536)
	if _, err := f.st.SetConnectionModels(ctx, "a", []domain.ModelEntry{{ID: "m"}}, owner.ID, owner.Login); err != nil {
		t.Fatal(err)
	}
	// Предел ответа на ход, предельный выход и цена кеша из каталога
	// (задачи 1.9.1, 1.12.1): thinking-модели нужен ход больше 8192.
	if _, err := f.st.SetConnectionModels(ctx, "b", []domain.ModelEntry{{ID: "m", ContextWindow: &window, InputPrice: &in, OutputPrice: &out,
		CachedInputPrice: &cached, MaxOutputTokens: &maxOut, ModelMaxOutput: &modelMax}}, owner.ID, owner.Login); err != nil {
		t.Fatal(err)
	}
	// Модель по умолчанию профиля и шаблона — a/m, у агентов команды — b/m.
	limits := domain.AgentLimits{MaxTurns: 120, MaxTokens: 500000, MaxMinutes: 30}
	if _, _, err := f.st.UpsertAgent(ctx, store.AgentInput{ID: store.AgentRivet, Name: "Агент Rivet", Adapter: store.AdapterNative,
		DefaultModel: &domain.AgentModelRef{ConnectionID: "a", Model: "m"}, Secrets: "secure", Limits: &limits}, owner.ID, owner.Login); err != nil {
		t.Fatal(err)
	}
	withTeam(t, f)
	dev, _ := f.st.GetRole(ctx, domain.RoleDeveloper)
	if _, err := f.st.UpdateRole(ctx, store.RoleInput{ID: dev.ID, Title: dev.Title, Instruction: dev.Instruction, Color: dev.Color,
		Capability: dev.Capability, Agent: store.AgentRivet, DefaultModel: &domain.AgentModelRef{ConnectionID: "a", Model: "m"}}, owner.ID, owner.Login); err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.TeamAgentInput{
		{ID: "maks", Name: "Макс", RoleID: domain.RoleDeveloper, Agent: store.AgentRivet, ConnectionID: "b", Model: "m"},
		{ID: "eva", Name: "Ева", RoleID: domain.RoleDeveloper, Agent: store.AgentRivet, ConnectionID: "b", Model: "m"},
	} {
		if _, err := f.st.CreateTeamAgent(ctx, a, owner.ID, owner.Login); err != nil {
			t.Fatal(err)
		}
	}
	// Кодом занимаются только runner'ы агента Rivet: прежние runner'ы
	// фикстуры убираются.
	if _, err := f.st.Pool.Exec(ctx, `DELETE FROM runners`); err != nil {
		t.Fatal(err)
	}
	native := []string{domain.FeatureRoles, "native"}
	checkEndpoint := func(as *pb.Assignment, agent string) *pb.ModelEndpoint {
		t.Helper()
		ep := as.GetEndpoint()
		if ep == nil || len(as.AgentEnv) != 0 || len(as.AgentArgs) != 0 || as.AgentCommand != "" || len(as.AgentSecretNames) != 0 { //nolint:staticcheck // устаревшие поля CLI должны быть пустыми
			t.Fatalf("агенту Rivet — endpoint без окружения CLI: %+v", as)
		}
		if run := assignAgent(t, f, as.SessionId); run.AgentID != agent {
			t.Fatalf("исполнитель: %+v", run)
		}
		// Пара агента команды бьёт модель по умолчанию профиля и шаблона.
		if ep.Api != "openai" || ep.BaseUrl != "http://b/v1" || ep.Model != "m" || ep.ConnectionId != "b" || as.ConnectionId != "b" || as.Model != "m" ||
			ep.ContextWindow != 128000 || !ep.HasPrices || ep.InputPrice != 0.27 || ep.OutputPrice != 1.1 || ep.Headers["X-Org"] != org ||
			ep.MaxOutputTokens != 32768 || ep.ModelMaxOutput != 65536 || ep.CachedInputPrice != 0.07 ||
			ep.GetLimits().GetMaxTurns() != 120 || ep.GetLimits().GetMaxTokens() != 500000 || ep.GetLimits().GetMaxMinutes() != 30 {
			t.Fatalf("endpoint пары агента команды: %+v", ep)
		}
		return ep
	}

	// Runner по loopback: в режиме secure ключ и секретный заголовок
	// уходят, факт доставки — в event log без значений.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "local", Agent: store.AgentRivet, Capabilities: []string{"coding"}, Features: native, Secure: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if ep := checkEndpoint(as, "maks"); ep.Key != key || ep.Headers["X-Token"] != token || !as.SecretsIncluded {
		t.Fatalf("секреты по loopback в режиме secure: %+v", ep)
	}
	evs, err := f.st.Events(ctx, store.EventFilter{TaskID: f.task.ID, Type: "runner.secrets_delivered"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("событие доставки секретов: %+v %v", evs, err)
	}
	raw, _ := json.Marshal(evs[0])
	if evs[0].Payload["connection_id"] != "b" || strings.Contains(string(raw), key) || strings.Contains(string(raw), token) {
		t.Fatalf("событие доставки без значений: %s", raw)
	}

	// Runner по открытому каналу: только несекретные данные, события
	// доставки нет.
	taskB, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "B", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "remote", Agent: store.AgentRivet, Capabilities: []string{"coding"}, Features: native}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as = f.out.lastAssign(t)
	if as.TaskId != taskB.ID {
		t.Fatalf("вторая задача: %+v", as)
	}
	if ep := checkEndpoint(as, "eva"); ep.Key != "" || ep.Headers["X-Token"] != "" || as.SecretsIncluded {
		t.Fatalf("секретов по открытому каналу нет: %+v", ep)
	}
	if evs, _ := f.st.Events(ctx, store.EventFilter{TaskID: taskB.ID, Type: "runner.secrets_delivered"}); len(evs) != 0 {
		t.Fatalf("без секретов события доставки нет: %+v", evs)
	}
	_ = policy.OutcomeOk
}
