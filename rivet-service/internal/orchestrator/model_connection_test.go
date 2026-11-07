package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// fix-agent-without-connection: агентская стадия не назначается, пока у
// модели нет рабочего подключения с ключом; задача ждёт с причиной и сама
// продолжает, когда подключение появилось. Стадия, упавшая из-за
// подключения, эскалируется кодом MODEL_CONNECTION, а не TEST_FAILED.

const fakeConnKey = "fake-deepseek-key-0000"

// nativeFixture — фикстура с единственным runner'ом агента Rivet (feature
// native) на защищённом канале; модель runner'а по умолчанию —
// deepseek-v4-flash, каталог подключений пуст. Проверка ключа control
// plane'ом в сеть не ходит: провайдер отвечает 401 (ключ отозван).
func nativeFixture(t *testing.T) policyFixture {
	t.Helper()
	ctx := context.Background()
	f := newPolicyFixture(t, "diff --git a/x b/x\n")
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	f.e.Box = box
	f.e.CheckKey = func(context.Context, llm.Client) error { return &llm.AuthError{Status: http.StatusUnauthorized} }
	if _, err := f.st.Pool.Exec(ctx, `DELETE FROM runners`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat", Agent: store.AgentRivet, Model: "deepseek-v4-flash",
		Capabilities: []string{"coding", "review"}, Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	return f
}

// addDeepSeek — подключение deepseek с ключом и моделью deepseek-v4-flash.
func addDeepSeek(t *testing.T, f policyFixture) {
	t.Helper()
	ctx := context.Background()
	key := fakeConnKey
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "deepseek", Name: "DeepSeek", Kind: "vendor", API: "openai",
		BaseURL: "https://api.deepseek.com", Key: &key}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionModels(ctx, "deepseek", []domain.ModelEntry{{ID: "deepseek-v4-flash"}}, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
}

// retryNow — пауза ожидания подключения прошла.
func retryNow(t *testing.T, f policyFixture) {
	t.Helper()
	if _, err := f.st.Pool.Exec(context.Background(), `UPDATE task_step_runs SET not_before = now() WHERE task_id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
}

// expectConnectionWait — стадия не назначена: Assignment не ушёл, сессии
// нет, runner свободен, задача ждёт с причиной.
func expectConnectionWait(t *testing.T, f policyFixture, mark int, reason string) {
	t.Helper()
	ctx := context.Background()
	if as := f.out.assignsSince(mark); len(as) != 0 {
		t.Fatalf("стадия без подключения не назначается: %+v", as)
	}
	task, err := f.st.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskReady || task.WaitReason != reason {
		t.Fatalf("задача ждёт подключения: %s %q", task.Status, task.WaitReason)
	}
	r, err := f.st.GetRunner(ctx, "nat")
	if err != nil || r.Status != domain.RunnerIdle || r.TaskID != "" {
		t.Fatalf("runner свободен: %+v %v", r, err)
	}
	var sessions int
	if err := f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE task_id=$1`, f.task.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("сессий нет: %d %v", sessions, err)
	}
	if atts, _ := f.st.ListAttention(ctx, f.owner.ID); len(atts) != 0 {
		t.Fatalf("ожидание подключения — не эскалация: %+v", atts)
	}
}

// Подключения к модели нет: стадия ждёт с причиной, а после добавления
// подключения назначается сама, с endpoint и ключом.
func TestStageWaitsForModelConnection(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	want := "нет подключения к модели deepseek-v4-flash: добавьте его в Управление → Подключения"
	expectConnectionWait(t, f, 0, want)
	waits := eventsOfType(t, f.st, f.task.ID, "task.wait")
	if len(waits) != 1 || waits[0].Payload["wait"] != "model_connection" || waits[0].Payload["model"] != "deepseek-v4-flash" ||
		!strings.Contains(waits[0].Text, want) {
		t.Fatalf("событие ожидания подключения: %+v", waits)
	}
	// Следующий тик до паузы ничего не меняет и событий не множит.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(eventsOfType(t, f.st, f.task.ID, "task.wait")); n != 1 {
		t.Fatalf("одно событие ожидания: %d", n)
	}

	addDeepSeek(t, f)
	retryNow(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING || as.Model != "deepseek-v4-flash" || as.GetEndpoint().GetConnectionId() != "deepseek" ||
		as.GetEndpoint().GetKey() != fakeConnKey {
		t.Fatalf("после подключения стадия назначена с endpoint и ключом: %+v", as)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskRunning || task.WaitReason != "" {
		t.Fatalf("задача пошла, причина снята: %s %q", task.Status, task.WaitReason)
	}
}

// Подключение есть, но без ключа, выключено или ключ отклонён: стадия ждёт
// с причиной, которая говорит, что исправить.
func TestStageWaitsForConnectionKey(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	// Ключа нет (перенос старого провайдера без ключа).
	if _, err := f.st.Pool.Exec(ctx, `UPDATE model_connections SET key_enc=NULL, key_prefix='' WHERE id='deepseek'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	expectConnectionWait(t, f, 0, "у подключения deepseek к модели deepseek-v4-flash нет ключа: добавьте ключ в Управление → Подключения")

	// Ключ отклонён провайдером при проверке.
	key := fakeConnKey
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "deepseek", Name: "DeepSeek", Kind: "vendor", API: "openai",
		BaseURL: "https://api.deepseek.com", Key: &key}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionCheck(ctx, "deepseek", domain.LLMStateInvalid, "401", f.owner.Login); err != nil {
		t.Fatal(err)
	}
	retryNow(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	expectConnectionWait(t, f, 0, "провайдер отклонил ключ подключения deepseek к модели deepseek-v4-flash: обновите ключ в Управление → Подключения")

	// Локальное подключение на чужой машине без ключа: runner без ключа
	// туда не пойдёт.
	if _, err := f.st.SetConnectionCheck(ctx, "deepseek", domain.LLMStateOK, "", f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "gpu", Name: "GPU", Kind: "local", API: "openai",
		BaseURL: "http://gpu-box:11434/v1"}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionModels(ctx, "gpu", []domain.ModelEntry{{ID: "qwen"}}, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	gpu := map[string]policy.AgentModel{store.AgentRivet: {ConnectionID: "gpu", Model: "qwen"}}
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AgentModels: gpu}, "owner"); err != nil {
		t.Fatal(err)
	}
	retryNow(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	expectConnectionWait(t, f, 0, "у подключения gpu к модели qwen нет ключа, а без ключа runner обращается только к серверу на своей машине: добавьте ключ или локальный адрес в Управление → Подключения")

	// Исправили: модель проекта снова deepseek, ключ рабочий — стадия идёт.
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{}, "owner"); err != nil {
		t.Fatal(err)
	}
	retryNow(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := f.out.lastAssign(t); as.GetEndpoint().GetConnectionId() != "deepseek" {
		t.Fatalf("после исправления стадия назначена: %+v", as)
	}
}

// Стадия упала из-за подключения (ключ удалили после назначения): задача
// эскалируется кодом MODEL_CONNECTION с причиной, а не TEST_FAILED.
func TestModelConnectionFailureEscalates(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING {
		t.Fatalf("назначение: %+v", as)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE model_connections SET key_enc=NULL, key_prefix='' WHERE id='deepseek'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, "nat", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING,
		Ok: false, Verdict: verdictModelConnection, Detail: "агент завершился с ошибкой: нет ключа подключения"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed {
		t.Fatalf("задача эскалирована: %s", task.Status)
	}
	atts, err := f.st.ListAttention(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].Reason != domain.AttModelConnection ||
		atts[0].Message != "агент не запустился: у подключения deepseek к модели deepseek-v4-flash нет ключа: добавьте ключ в Управление → Подключения" {
		t.Fatalf("эскалация MODEL_CONNECTION с причиной: %+v", atts)
	}
}

// Подключение рабочее, но ключ не дошёл до runner'а (режим секретов never,
// своего ключа у runner'а нет): причина называет переменную runner'а.
func TestModelConnectionFailureSecretsHint(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agents SET secrets='never' WHERE id=$1`, store.AgentRivet); err != nil {
		t.Fatal(err)
	}
	// Ревьюер — другой runner агента Rivet: исполнитель кода review не ведёт.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat-review", Agent: store.AgentRivet,
		Capabilities: []string{"review"}, Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	// Review с ошибкой подключения — тоже fail, а не замечания разработчику.
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW || as.GetEndpoint().GetKey() != "" {
		t.Fatalf("review без ключа в назначении: %+v", as)
	}
	runner := mustReviewRunner(t, f)
	if err := f.e.OnStageResult(ctx, runner, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW,
		Ok: false, Verdict: verdictModelConnection, Detail: "ревьюер завершился с ошибкой: нет ключа подключения"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed || task.AttemptUsed != 0 {
		t.Fatalf("ошибка подключения на review — эскалация без попытки: %s %d", task.Status, task.AttemptUsed)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttModelConnection || !strings.Contains(atts[0].Message, "RIVET_PROVIDER_KEY_DEEPSEEK") ||
		!strings.Contains(atts[0].Message, "never") {
		t.Fatalf("подсказка про ключ runner'а: %+v", atts)
	}
}

// mustReviewRunner — runner запуска review.
func mustReviewRunner(t *testing.T, f policyFixture) string {
	t.Helper()
	var id string
	if err := f.st.Pool.QueryRow(context.Background(), `
		SELECT runner_id FROM task_step_runs WHERE task_id=$1 AND step_kind='review' AND verdict IS NULL`, f.task.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Вердикт connection — fail на любом шаге: не замечания разработчику.
func TestVerdictModelConnectionIsFail(t *testing.T) {
	for _, kind := range []string{policy.StepCode, policy.StepTest, policy.StepReview, policy.StepPrompt} {
		if got := verdictOf(kind, false, verdictModelConnection); got != policy.OutcomeFail {
			t.Fatalf("%s: %s", kind, got)
		}
	}
	if envSuffix("open-router.1") != "OPEN_ROUTER_1" {
		t.Fatalf("суффикс переменной ключа: %s", envSuffix("open-router.1"))
	}
}
