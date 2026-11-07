package orchestrator

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Отказ провайдера во время стадии (ревью fix-agent-without-connection,
// проход 3): подключение переводит в invalid только control plane по своей
// проверке ключа, разбор идёт по тому, с чем стадия ушла runner'у.

const reject401 = "агент завершился с ошибкой: провайдер отклонил ключ: HTTP 401"

// vendorConn — подключение на нелокальном адресе с ключом и одной моделью.
func vendorConn(t *testing.T, f policyFixture, id, model string) {
	t.Helper()
	ctx := context.Background()
	key := fakeConnKey
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: id, Name: id, Kind: "vendor", API: "openai",
		BaseURL: "https://api." + id + ".example", Key: &key}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionModels(ctx, id, []domain.ModelEntry{{ID: model}}, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
}

// codingResult — результат стадии code с вердиктом connection.
func codingResult(t *testing.T, f policyFixture, runner string, as *pb.Assignment, detail string) {
	t.Helper()
	if err := f.e.OnStageResult(context.Background(), runner, &pb.StageResult{TaskId: as.TaskId, SessionId: as.SessionId,
		Stage: as.Stage, Ok: false, Verdict: verdictModelConnection, Detail: detail}); err != nil {
		t.Fatal(err)
	}
}

func connState(t *testing.T, f policyFixture, id string) domain.LLMProviderState {
	t.Helper()
	c, err := f.st.GetConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c.State
}

// Модель проекта сменили, пока стадия шла: invalid получает подключение,
// с которым стадия ушла runner'у, а не текущее.
func TestRejectMarksAssignedConnection(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	vendorConn(t, f, "aaa", "model-a")
	vendorConn(t, f, "bbb", "model-b")
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	used := as.GetEndpoint().GetConnectionId()
	other, otherModel := "bbb", "model-b"
	if used == "bbb" {
		other, otherModel = "aaa", "model-a"
	}
	if used == "" || as.GetEndpoint().GetKey() == "" {
		t.Fatalf("стадия ушла с ключом каталога: %+v", as.GetEndpoint())
	}
	if _, err := f.st.SaveProjectPolicy(ctx, f.p.ID, policy.Overrides{AgentModels: map[string]policy.AgentModel{
		store.AgentRivet: {ConnectionID: other, Model: otherModel}}}, "owner"); err != nil {
		t.Fatal(err)
	}
	codingResult(t, f, "nat", as, reject401)
	if connState(t, f, used) != domain.LLMStateInvalid || connState(t, f, other) == domain.LLMStateInvalid {
		t.Fatalf("invalid у подключения стадии %s, а не у текущего %s: %s / %s", used, other, connState(t, f, used), connState(t, f, other))
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || !strings.Contains(atts[0].Message, "провайдер отклонил ключ подключения "+used) {
		t.Fatalf("эскалация называет подключение стадии: %+v", atts)
	}
}

// Стадия ушла runner'у на открытом канале без ключа каталога, runner шёл
// на своём ключе и переподключился по TLS, пока стадия шла. 401 от ключа
// runner'а подключение каталога не трогает, эскалация про ключ runner'а.
func TestRunnerKeyRejectionKeepsCatalog(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	checked := false
	f.e.CheckKey = func(context.Context, llm.Client) error { checked = true; return nil }
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET secure=false WHERE id='nat'`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.GetEndpoint().GetKey() != "" {
		t.Fatalf("на открытом канале ключ каталога не уходит: %+v", as.GetEndpoint())
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET secure=true WHERE id='nat'`); err != nil {
		t.Fatal(err)
	}
	codingResult(t, f, "nat", as, reject401)
	if s := connState(t, f, "deepseek"); s == domain.LLMStateInvalid || checked {
		t.Fatalf("ключ каталога не проверяется и не помечается: %s, проверка %v", s, checked)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttModelConnection ||
		!strings.Contains(atts[0].Message, "провайдер отклонил ключ runner'а") || !strings.Contains(atts[0].Message, "RIVET_PROVIDER_KEY_DEEPSEEK") {
		t.Fatalf("эскалация про ключ runner'а: %+v", atts)
	}
}

// Отказ 403 (права ключа, регион, блокировка сети) подключение не
// переводит и проверку не запускает.
func TestForbiddenKeepsConnection(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	checked := false
	f.e.CheckKey = func(context.Context, llm.Client) error {
		checked = true
		return &llm.AuthError{Status: http.StatusForbidden}
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	codingResult(t, f, "nat", f.out.lastAssign(t), "агент завершился с ошибкой: провайдер отклонил ключ: HTTP 403")
	if s := connState(t, f, "deepseek"); s == domain.LLMStateInvalid || checked {
		t.Fatalf("403 подключение не трогает: %s, проверка %v", s, checked)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttModelConnection || !strings.Contains(atts[0].Message, "HTTP 403") {
		t.Fatalf("эскалация про отказ в доступе: %+v", atts)
	}
}

// Проверка control plane с тем же ключом прошла: одного отказа на одном
// runner'е мало, подключение остаётся рабочим, задача эскалируется.
func TestOwnCheckPassedKeepsConnection(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	f.e.CheckKey = func(context.Context, llm.Client) error { return nil }
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	codingResult(t, f, "nat", f.out.lastAssign(t), reject401)
	if s := connState(t, f, "deepseek"); s == domain.LLMStateInvalid {
		t.Fatalf("подключение не переведено: %s", s)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || !strings.Contains(atts[0].Message, "отклонил ключ подключения deepseek к модели deepseek-v4-flash во время стадии") ||
		!strings.Contains(atts[0].Message, "ключ заменили") || strings.Contains(atts[0].Message, "прокси") {
		t.Fatalf("эскалация без перевода в invalid, причина про отказ во время стадии: %+v", atts)
	}
}

// Две стадии получили 401 разом: подключение переводит в invalid разбор
// одной из них, и эскалация одна. Вторая задача без эскалации
// возвращается на шаг и ждёт подключения.
func TestConcurrentRejectionsOneEscalation(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	// Пока шла проверка этого разбора, подключение перевёл разбор другой
	// стадии.
	f.e.CheckKey = func(context.Context, llm.Client) error {
		if _, err := f.st.MarkConnectionRejected(ctx, "deepseek", "другая стадия"); err != nil {
			t.Error(err)
		}
		return &llm.AuthError{Status: http.StatusUnauthorized}
	}
	codingResult(t, f, "nat", as, reject401)
	task, _ := f.st.GetTask(ctx, f.task.ID)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status == domain.TaskFailed || len(atts) != 0 || task.AttemptUsed != 0 {
		t.Fatalf("эскалацию пишет тот, кто перевёл: задача %s, эскалаций %d, попыток %d", task.Status, len(atts), task.AttemptUsed)
	}
	ev := stepEventOf(t, f, "code", policy.OutcomeFail)
	if ev.Payload["next"] != "code" {
		t.Fatalf("событие перехода называет возврат на шаг: %+v", ev.Payload)
	}
}

// Исправление после замечаний review: подключение уже invalid, исправляющая
// стадия получила 401 и вернулась на шаг. Замечания review доходят до
// следующего исправляющего запуска, попытка не расходуется.
func TestInvalidReturnKeepsFixContext(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat-review", Agent: store.AgentRivet,
		Capabilities: []string{"review"}, Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rv := f.out.lastAssign(t)
	if rv.Stage != pb.StageResult_REVIEW {
		t.Fatalf("review: %+v", rv)
	}
	if err := f.e.OnStageResult(ctx, mustReviewRunner(t, f), &pb.StageResult{TaskId: f.task.ID, SessionId: rv.SessionId,
		Stage: pb.StageResult_REVIEW, Ok: false, Detail: "ЗАМЕЧАНИЕ-42: поправь обработку ошибок"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fix := f.out.lastAssign(t)
	if fix.Stage != pb.StageResult_FIXING || !strings.Contains(fix.ExtraContext, "ЗАМЕЧАНИЕ-42") {
		t.Fatalf("первое исправление с замечаниями: %s", fix.Stage)
	}
	before, _ := f.st.GetTask(ctx, f.task.ID)
	if _, err := f.st.SetConnectionCheck(ctx, "deepseek", domain.LLMStateInvalid, "401", f.owner.Login); err != nil {
		t.Fatal(err)
	}
	codingResult(t, f, mustTaskRunner(t, f.st, f.task.ID), fix, reject401)
	task, _ := f.st.GetTask(ctx, f.task.ID)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status == domain.TaskFailed || task.StepEntry != policy.OutcomeChanges || task.AttemptUsed != before.AttemptUsed || len(atts) != 0 {
		t.Fatalf("возврат на шаг без эскалации и попытки: %s %s/%s попыток %d (было %d), эскалаций %d",
			task.Status, task.StepID, task.StepEntry, task.AttemptUsed, before.AttemptUsed, len(atts))
	}
	evs := eventsOfType(t, f.st, f.task.ID, "task.step")
	last := evs[len(evs)-1]
	if last.Payload["outcome"] != policy.OutcomeFail || last.Payload["next"] != task.StepID {
		t.Fatalf("событие перехода называет возврат на шаг: %+v", last.Payload)
	}
	// Ключ заменили: исправление идёт снова и с теми же замечаниями.
	key := fakeConnKey
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "deepseek", Name: "DeepSeek", Kind: "vendor", API: "openai",
		BaseURL: "https://api.deepseek.com", Key: &key}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	retryNow(t, f)
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	again := f.out.assignsSince(mark)
	if len(again) == 0 || again[0].Stage != pb.StageResult_FIXING || !strings.Contains(again[0].ExtraContext, "ЗАМЕЧАНИЕ-42") {
		t.Fatalf("повторное исправление с замечаниями review: %+v", again)
	}
}

// Запуск получил отказ по модели агента команды, затем ушёл другому агенту
// и не стартовал по другой причине (назначение не доставлено): после
// прохода причина «нет агента с рабочей моделью» его не перекрывает.
func TestAgentMissKeepsRealReason(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withTester(t, f)
	deepSeekWithoutKey(t, f)
	withLocalModel(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agents SET secrets='secure' WHERE id=$1`, store.AgentRivet); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=false WHERE role_id='developer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		('bad', 'Бэд', 'developer', 'deepseek', 'deepseek-v4-flash', 1),
		('good', 'Гуд', 'developer', 'local', 'm', 2)`); err != nil {
		t.Fatal(err)
	}
	// Свободный runner на открытом канале: проверка до назначения мягкая,
	// агент без ключа её проходит. Назначение достаётся защищённому runner'у
	// (у него меньше capabilities), там ключ нужен.
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "open", Agent: store.AgentRivet, Capabilities: []string{"coding", "review", "testing"},
		Features: []string{"native"}, Secure: false}); err != nil {
		t.Fatal(err)
	}
	down := &downSender{}
	f.e.Out = down
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var good int
	for _, m := range down.sent {
		if a := m.GetAssign(); a != nil && a.GetRole().GetName() == "Гуд" {
			good++
		}
	}
	if good == 0 {
		t.Fatalf("запуск ушёл агенту с рабочей моделью: %d назначений", len(down.sent))
	}
	var reason string
	if err := f.st.Pool.QueryRow(ctx, `SELECT defer_reason FROM task_step_runs WHERE task_id=$1 AND verdict IS NULL`, f.task.ID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if strings.HasPrefix(reason, "нет свободного агента роли") || strings.Contains(task.WaitReason, "нет свободного агента роли") {
		t.Fatalf("причина отказа агента перекрыла настоящую: %q / %q", reason, task.WaitReason)
	}
}

// localModelStage — шаг code на модели m подключения local к локальному
// серверу (с ключом key, если он задан): стадия назначена.
func localModelStage(t *testing.T, f policyFixture, key string) *pb.Assignment {
	t.Helper()
	ctx := context.Background()
	in := store.ConnectionInput{ID: "local", Name: "Local", Kind: "local", API: "openai", BaseURL: "http://localhost:11434/v1"}
	if key != "" {
		in.Key = &key
	}
	if _, _, err := f.st.UpsertConnection(ctx, in, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionModels(ctx, "local", []domain.ModelEntry{{ID: "m"}}, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	setProcess(t, f, policy.Process{Steps: []policy.Step{
		{ID: "code", Kind: policy.StepCode, Participants: []policy.Participant{{Agent: &policy.AgentRef{Kind: "rivet", Model: "m"}}}},
		{ID: "merge", Kind: policy.StepMerge},
	}})
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.GetEndpoint().GetConnectionId() != "local" {
		t.Fatalf("стадия на локальном подключении: %+v", as.GetEndpoint())
	}
	return as
}

// Ревью, проход 4, P3-2: локальный адрес — машина runner'а, control plane
// свой localhost не проверяет и подключение не переводит.
func TestLocalConnectionNoOwnCheck(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	checked := false
	f.e.CheckKey = func(context.Context, llm.Client) error {
		checked = true
		return &llm.AuthError{Status: http.StatusUnauthorized}
	}
	as := localModelStage(t, f, "fake-local-key-0000")
	if as.GetEndpoint().GetKey() == "" {
		t.Fatal("ключ каталога ушёл runner'у")
	}
	codingResult(t, f, "nat", as, reject401)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if s := connState(t, f, "local"); s == domain.LLMStateInvalid || checked {
		t.Fatalf("локальный сервер не проверяется: %s, проверка %v", s, checked)
	}
	if len(atts) != 1 || !strings.Contains(atts[0].Message, "на машине runner'а") {
		t.Fatalf("эскалация про сервер на машине runner'а: %+v", atts)
	}
}

// Ревью, проход 4, P3-5: у локального подключения нет ключа, а сервер
// модели ответил 401: подсказка добавить ключ в каталог.
func TestKeylessConnectionHint(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	as := localModelStage(t, f, "")
	codingResult(t, f, "nat", as, reject401)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || !strings.Contains(atts[0].Message, "у подключения в каталоге нет ключа, добавьте ключ") {
		t.Fatalf("подсказка про ключ в каталоге: %+v", atts)
	}
}

// Ревью, проход 4, P3-3: код отказа читается только из первой строки итога
// (ошибка агента). Тот же текст ниже, в выводе агента, проверку не запускает.
func TestRejectionCodeFromFirstLineOnly(t *testing.T) {
	f := nativeFixture(t)
	addDeepSeek(t, f)
	checked := false
	f.e.CheckKey = func(context.Context, llm.Client) error {
		checked = true
		return &llm.AuthError{Status: http.StatusUnauthorized}
	}
	if err := f.e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	codingResult(t, f, "nat", f.out.lastAssign(t), "агент завершился с ошибкой: нет адреса подключения\nв логе: провайдер отклонил ключ: HTTP 401")
	if checked || connState(t, f, "deepseek") == domain.LLMStateInvalid {
		t.Fatalf("код из вывода агента не читается: проверка %v", checked)
	}
}

// Ревью, проход 4, P3-4: runner отключился, пока шла проверка ключа.
// Исход шага уже закреплён, поэтому применение идёт до конца: подключение
// invalid, задача эскалирована, а не застряла на шаге.
func TestOutcomeSurvivesRunnerDisconnect(t *testing.T) {
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if err := f.e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	stream, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	f.e.CheckKey = func(ctx context.Context, _ llm.Client) error {
		disconnect()
		if err := ctx.Err(); err != nil {
			return err
		}
		return &llm.AuthError{Status: http.StatusUnauthorized}
	}
	if err := f.e.OnStageResult(stream, "nat", &pb.StageResult{TaskId: as.TaskId, SessionId: as.SessionId,
		Stage: as.Stage, Ok: false, Verdict: verdictModelConnection, Detail: reject401}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, _ := f.st.GetTask(ctx, f.task.ID)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if connState(t, f, "deepseek") != domain.LLMStateInvalid || task.Status != domain.TaskFailed || len(atts) != 1 {
		t.Fatalf("исход применён после обрыва канала: подключение %s, задача %s, эскалаций %d", connState(t, f, "deepseek"), task.Status, len(atts))
	}
}
