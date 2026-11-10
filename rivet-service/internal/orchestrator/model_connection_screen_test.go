package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Регрессии ревью fix-agent-without-connection (проход 1): проверка
// подключения до назначения, агенты команды, шаг test без роли, причины
// для явной модели, вердикт connection при переходе fail на шаг.

// withLocalModel — рабочее подключение local к локальному серверу с
// моделью m (ключ не нужен).
func withLocalModel(t *testing.T, f policyFixture) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "local", Name: "Local", Kind: "local", API: "openai",
		BaseURL: "http://localhost:11434/v1"}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionModels(ctx, "local", []domain.ModelEntry{{ID: "m"}}, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
}

// deepSeekWithoutKey — подключение deepseek без ключа (перенос старого
// провайдера), модель deepseek-v4-flash.
func deepSeekWithoutKey(t *testing.T, f policyFixture) {
	t.Helper()
	addDeepSeek(t, f)
	if _, err := f.st.Pool.Exec(context.Background(), `UPDATE model_connections SET key_enc=NULL, key_prefix='' WHERE id='deepseek'`); err != nil {
		t.Fatal(err)
	}
}

// Ждущие подключения запуски не занимают runner: задача другого проекта с
// рабочей моделью назначается в том же проходе, runner к ждущим задачам не
// привязывался.
func TestScreenDoesNotStarveHealthyTasks(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	deepSeekWithoutKey(t, f)
	withLocalModel(t, f)
	// Модель по умолчанию — deepseek (первая в каталоге по id подключения).
	for i := 0; i < 8; i++ {
		if _, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: fmt.Sprintf("A%d", i), Criteria: []domain.Criterion{{Text: "ok"}}}); err != nil {
			t.Fatal(err)
		}
	}
	p2, err := f.st.CreateProject(ctx, "demo2", "owner/repo2", nil, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SaveProjectPolicy(ctx, p2.ID, policy.Overrides{AgentModels: map[string]policy.AgentModel{
		store.AgentRivet: {ConnectionID: "local", Model: "m"}}}, "owner"); err != nil {
		t.Fatal(err)
	}
	epic2, _ := f.st.CreateEpic(ctx, p2.ID, "Epic2", "")
	b, err := f.st.CreateTask(ctx, epic2.ID, store.NewTask{Title: "B", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.TransitionEpic(ctx, epic2.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	// Первый проход вводит задачи на шаг, второй назначает.
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if task, _ := f.st.GetTask(ctx, b.ID); task.Status != domain.TaskRunning {
		t.Fatalf("задача с рабочей моделью назначается сразу: %s %q", task.Status, task.WaitReason)
	}
	as := f.out.lastAssign(t)
	if as.TaskId != b.ID || as.GetEndpoint().GetConnectionId() != "local" {
		t.Fatalf("назначение задаче B на local: %+v", as)
	}
	var waiting, sessions int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE wait_reason LIKE 'у подключения deepseek%'`).Scan(&waiting)
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE task_id <> $1`, b.ID).Scan(&sessions)
	if waiting != 9 || sessions != 0 {
		t.Fatalf("ждут подключения 9 задач без сессий: ждут %d, сессий %d", waiting, sessions)
	}
	var assigns int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type IN ('task.assign','task.status') AND task_id <> $1 AND text LIKE 'назнач%'`, b.ID).Scan(&assigns)
	if assigns != 0 {
		t.Fatalf("ждущие задачи runner'у не назначались: %d", assigns)
	}
}

// Агент команды без рабочей модели уступает коллеге той же роли, пары
// событий занят/свободен у него нет.
func TestTeamAgentWithoutConnectionYields(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withTester(t, f)
	deepSeekWithoutKey(t, f)
	withLocalModel(t, f)
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		('bad', 'Бэд', 'developer', 'deepseek', 'deepseek-v4-flash', 1),
		('good', 'Гуд', 'developer', 'local', 'm', 2)`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.assignsSince(0)
	if len(as) != 1 || as[0].GetRole().GetName() != "Гуд" || as[0].GetEndpoint().GetConnectionId() != "local" {
		t.Fatalf("задачу берёт агент с рабочей моделью: %+v", as)
	}
	var busyBad, freeBad, busyGood int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type='team.agent_busy' AND payload->>'id' = 'bad'),
		count(*) FILTER (WHERE type='team.agent_free' AND payload->>'id' = 'bad'),
		count(*) FILTER (WHERE type='team.agent_busy' AND payload->>'id' = 'good') FROM events`).Scan(&busyBad, &freeBad, &busyGood)
	if busyBad != 0 || freeBad != 0 || busyGood != 1 {
		t.Fatalf("агента без модели не занимают: bad busy=%d free=%d, good busy=%d", busyBad, freeBad, busyGood)
	}
}

// Все агенты роли без рабочей модели: задача ждёт с причиной про
// подключение агентов роли, runner'а и агента не занимает.
func TestTeamWithoutConnectionWaitReason(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withTester(t, f)
	deepSeekWithoutKey(t, f)
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		('bad', 'Бэд', 'developer', 'deepseek', 'deepseek-v4-flash', 1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if len(f.out.assignsSince(0)) != 0 || !strings.HasPrefix(task.WaitReason, "нет агента роли «Разработчик» с рабочим подключением к модели: у подключения deepseek") {
		t.Fatalf("причина ожидания агентов роли: %q, назначений %d", task.WaitReason, len(f.out.assignsSince(0)))
	}
	var busy int
	_ = f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type IN ('team.agent_busy','team.agent_free') AND payload->>'id' = 'bad'`).Scan(&busy)
	if busy != 0 {
		t.Fatalf("агента без модели не занимали: %d", busy)
	}
}

// Шаг test без роли только запускает проверки: неработающая модель по
// умолчанию его не держит.
func TestPlainChecksNeedNoModel(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	deepSeekWithoutKey(t, f)
	withLocalModel(t, f)
	setProcess(t, f, policy.Process{Steps: []policy.Step{
		{ID: "code", Kind: policy.StepCode, Participants: []policy.Participant{{Agent: &policy.AgentRef{Kind: "rivet", Model: "m"}}}},
		{ID: "test", Kind: policy.StepTest, Participants: []policy.Participant{{Agent: &policy.AgentRef{}}}},
		{ID: "merge", Kind: policy.StepMerge},
	}})
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "nat", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: as.Stage, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	test := f.out.lastAssign(t)
	if test.Stage != pb.StageResult_TESTING || test.GetEndpoint() != nil {
		t.Fatalf("проверки назначены без модели: %+v", test)
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.WaitReason != "" {
		t.Fatalf("причины ожидания нет: %q", task.WaitReason)
	}
}

// Явная модель участника, которой нет среди доступных: причина про
// подключение (нет подключения, выключено), а не «нет runner'а rivet/X», в
// том числе когда свободного runner'а нет.
func TestExplicitModelConnectionReasons(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withLocalModel(t, f)
	key := fakeConnKey
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "deepseek", Name: "DeepSeek", Kind: "vendor", API: "openai",
		BaseURL: "https://api.deepseek.com", Key: &key}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConnectionModels(ctx, "deepseek", []domain.ModelEntry{{ID: "deepseek-v4-pro"}}, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	setProcess(t, f, policy.Process{Steps: []policy.Step{
		{ID: "code", Kind: policy.StepCode, Participants: []policy.Participant{{Agent: &policy.AgentRef{Kind: "rivet", Model: "deepseek-v4-pro"}}}},
		{ID: "merge", Kind: policy.StepMerge},
	}})
	off := false
	if _, _, err := f.st.UpsertConnection(ctx, store.ConnectionInput{ID: "deepseek", Name: "DeepSeek", Kind: "vendor", API: "openai",
		BaseURL: "https://api.deepseek.com", Enabled: &off}, f.e.Box, f.owner.ID, f.owner.Login); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.WaitReason != "подключение deepseek к модели deepseek-v4-pro выключено: включите его в Управление → Подключения" {
		t.Fatalf("причина для выключенного подключения: %q", task.WaitReason)
	}

	// Модель пропала совсем, а свободного runner'а нет: причина та же по
	// смыслу, её пишет markWaiting.
	if _, err := f.st.Pool.Exec(ctx, `DELETE FROM model_connections WHERE id='deepseek'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE tasks SET wait_reason='' WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET status='running'`); err != nil {
		t.Fatal(err)
	}
	retryNow(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.WaitReason != "нет подключения к модели deepseek-v4-pro: добавьте его в Управление → Подключения" {
		t.Fatalf("причина без подключения при занятом runner'е: %q", task.WaitReason)
	}
}

// Вердикт connection эскалирует задачу MODEL_CONNECTION, даже если переход
// fail шага ведёт на другой шаг: другой шаг настройку не исправит.
func TestConnectionVerdictIgnoresFailTransition(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat-review", Agent: store.AgentRivet,
		Capabilities: []string{"review"}, Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	proc := policy.DefaultProcess()
	proc.Steps[2].On = &policy.Transitions{Fail: "code"}
	setProcess(t, f, proc)
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW {
		t.Fatalf("назначение review: %+v", as)
	}
	if err := f.e.OnStageResult(ctx, mustReviewRunner(t, f), &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId,
		Stage: pb.StageResult_REVIEW, Ok: false, Verdict: verdictModelConnection, Detail: "ревьюер завершился с ошибкой: провайдер отклонил ключ"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed || task.AttemptUsed != 0 {
		t.Fatalf("эскалация без перехода fail и без попытки: %s %s %d", task.Status, task.StepID, task.AttemptUsed)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttModelConnection {
		t.Fatalf("эскалация MODEL_CONNECTION: %+v", atts)
	}
	ev := stepEventOf(t, f, policy.StepReview, policy.OutcomeFail)
	if ev.Payload["next"] != policy.TargetEscalate {
		t.Fatalf("событие перехода называет эскалацию: %+v", ev.Payload)
	}
}

// stepEventOf — событие task.step шага с исходом.
func stepEventOf(t *testing.T, f policyFixture, step, outcome string) domain.Event {
	t.Helper()
	for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.step") {
		if ev.Payload["step"] == step && ev.Payload["outcome"] == outcome {
			return ev
		}
	}
	t.Fatalf("нет события task.step %s/%s", step, outcome)
	return domain.Event{}
}

// Режим секретов never: ключ каталога runner'у не уходит, runner работает
// на своём ключе, поэтому подключение без ключа стадию не держит.
func TestNeverModeIgnoresCatalogKey(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	deepSeekWithoutKey(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agents SET secrets='never' WHERE id=$1`, store.AgentRivet); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_CODING || as.GetEndpoint().GetConnectionId() != "deepseek" || as.GetEndpoint().GetKey() != "" {
		t.Fatalf("в режиме never стадия идёт без ключа каталога: %+v", as)
	}
}

// Ключ шифрования rivetd не задан: причина говорит об этом, а не просит
// задать ключ подключения заново.
func TestNoSecretKeyReason(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	f.e.Box = nil
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if !strings.HasPrefix(task.WaitReason, "у rivetd не задан ключ шифрования (RIVET_SECRET_KEY)") {
		t.Fatalf("причина без ключа шифрования: %q", task.WaitReason)
	}
}

// Сессия доработки без подключения: отказ до изменений задачи, эскалация
// MODEL_CONNECTION остаётся открытой.
func TestUserSessionChecksConnectionFirst(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	if err := f.e.OnStageResult(ctx, "nat", &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_CODING,
		Ok: false, Verdict: verdictModelConnection, Detail: "агент завершился с ошибкой: провайдер отклонил ключ: HTTP 401"}); err != nil {
		t.Fatal(err)
	}
	// Ключ каталога дошёл, провайдер его отклонил, проверка control plane
	// подтвердила отказ: подключение invalid.
	if c, err := f.st.GetConnection(ctx, "deepseek"); err != nil || c.State != domain.LLMStateInvalid {
		t.Fatalf("подключение переведено в invalid: %+v %v", c, err)
	}
	_, err := f.e.StartUserSession(ctx, f.task.ID, "доделай", f.owner.Login, false)
	var mc *ModelConnectionError
	if !errorsAs(err, &mc) || !strings.Contains(mc.Reason, "провайдер отклонил ключ подключения deepseek") {
		t.Fatalf("отказ сессии по подключению: %v", err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if task.Status != domain.TaskFailed || len(atts) != 1 || atts[0].Reason != domain.AttModelConnection {
		t.Fatalf("задача и эскалация не тронуты: %s %+v", task.Status, atts)
	}
}

func errorsAs(err error, target **ModelConnectionError) bool {
	for err != nil {
		if mc, ok := err.(*ModelConnectionError); ok {
			*target = mc
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// teamCounts — события занят/свободен агента команды.
func teamCounts(t *testing.T, f policyFixture, agent string) (busy, free int) {
	t.Helper()
	_ = f.st.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE type='team.agent_busy' AND payload->>'id'=$1),
		count(*) FILTER (WHERE type='team.agent_free' AND payload->>'id'=$1) FROM events`, agent).Scan(&busy, &free)
	return busy, free
}

// Ревью, проход 2, P2-A: у участника своя рабочая модель, у агента роли —
// модель без ключа. Модель стадии — модель участника, стадия назначается
// агенту роли на ней.
func TestRoleWithExplicitModelIgnoresAgentModel(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withTester(t, f)
	deepSeekWithoutKey(t, f)
	withLocalModel(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=false WHERE role_id='developer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		('bad', 'Бэд', 'developer', 'deepseek', 'deepseek-v4-flash', 1)`); err != nil {
		t.Fatal(err)
	}
	setProcess(t, f, policy.Process{Steps: []policy.Step{
		{ID: "code", Kind: policy.StepCode, Participants: []policy.Participant{{Agent: &policy.AgentRef{Role: "developer", Model: "m"}}}},
		{ID: "merge", Kind: policy.StepMerge},
	}})
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	as := f.out.assignsSince(0)
	if len(as) != 1 || as[0].Model != "m" || as[0].GetEndpoint().GetConnectionId() != "local" || as[0].GetRole().GetName() != "Бэд" {
		t.Fatalf("стадия на модели участника: %+v", as)
	}
}

// Ревью, проход 2, P2-B: режим secure, свободный runner защищён, второй на
// открытом канале занят, у единственного агента роли модель без ключа.
// Агента не назначают и не снимают каждый проход, причина видна.
func TestMixedChannelsNoAgentChurn(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withTester(t, f)
	deepSeekWithoutKey(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE agents SET secrets='secure' WHERE id=$1`, store.AgentRivet); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=false WHERE role_id='developer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		('bad', 'Бэд', 'developer', 'deepseek', 'deepseek-v4-flash', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "open", Agent: store.AgentRivet, Capabilities: []string{"coding"},
		Features: []string{"native"}, Secure: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE runners SET status='running' WHERE id='open'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	busy, free := teamCounts(t, f, "bad")
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if busy != 0 || free != 0 || len(f.out.assignsSince(0)) != 0 ||
		!strings.Contains(task.WaitReason, "у подключения deepseek к модели deepseek-v4-flash нет ключа") {
		t.Fatalf("5 проходов: busy=%d free=%d назначений %d, причина %q", busy, free, len(f.out.assignsSince(0)), task.WaitReason)
	}
}

// Ревью, проход 2, P3-3: агент без рабочей модели исключён, агент с рабочей
// моделью занят: у ждущей задачи понятная причина.
func TestBadAgentExcludedGoodBusyReason(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	withTester(t, f)
	deepSeekWithoutKey(t, f)
	withLocalModel(t, f)
	if _, err := f.st.Pool.Exec(ctx, `UPDATE team_agents SET enabled=false WHERE role_id='developer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO team_agents (id, name, role_id, connection_id, model, position) VALUES
		('bad', 'Бэд', 'developer', 'deepseek', 'deepseek-v4-flash', 1),
		('good', 'Гуд', 'developer', 'local', 'm', 2)`); err != nil {
		t.Fatal(err)
	}
	second, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "B", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat2", Agent: store.AgentRivet, Capabilities: []string{"coding"},
		Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if as := f.out.assignsSince(0); len(as) != 1 || as[0].GetRole().GetName() != "Гуд" {
		t.Fatalf("одна задача у агента с рабочей моделью: %+v", as)
	}
	waiting := f.task.ID
	if as := f.out.lastAssign(t); as.TaskId == f.task.ID {
		waiting = second.ID
	}
	task, _ := f.st.GetTask(ctx, waiting)
	if !strings.HasPrefix(task.WaitReason, "нет свободного агента роли «Разработчик» с рабочим подключением к модели: агенты с рабочей моделью заняты") {
		t.Fatalf("причина ожидания второй задачи: %q", task.WaitReason)
	}
	if busy, _ := teamCounts(t, f, "bad"); busy != 0 {
		t.Fatalf("агента без модели не занимали: %d", busy)
	}
}

// Ревью, проход 2, P3-7: провайдер отклонил ключ каталога во время стадии —
// подключение invalid и одна эскалация; другая задача на том же ключе не
// эскалируется, а ждёт подключения.
func TestProviderRejectionOneEscalationPerKey(t *testing.T) {
	ctx := context.Background()
	f := nativeFixture(t)
	addDeepSeek(t, f)
	second, err := f.st.CreateTask(ctx, f.epic.ID, store.NewTask{Title: "B", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertRunner(ctx, domain.Runner{ID: "nat2", Agent: store.AgentRivet, Capabilities: []string{"coding"},
		Features: []string{"native"}, Secure: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.assignsSince(0)
	if len(as) != 2 {
		t.Fatalf("обе задачи назначены: %d", len(as))
	}
	runnerOf := func(taskID string) string { return mustTaskRunner(t, f.st, taskID) }
	for _, a := range as {
		if err := f.e.OnStageResult(ctx, runnerOf(a.TaskId), &pb.StageResult{TaskId: a.TaskId, SessionId: a.SessionId, Stage: pb.StageResult_CODING,
			Ok: false, Verdict: verdictModelConnection, Detail: "агент завершился с ошибкой: провайдер отклонил ключ: HTTP 401"}); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := f.st.GetConnection(ctx, "deepseek"); c.State != domain.LLMStateInvalid {
		t.Fatalf("подключение invalid: %s", c.State)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	if len(atts) != 1 || atts[0].Reason != domain.AttModelConnection || !strings.Contains(atts[0].Message, "провайдер отклонил ключ подключения deepseek") {
		t.Fatalf("одна эскалация на ключ: %+v", atts)
	}
	// Следующий проход: вторая задача ждёт подключения с причиной.
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if again := f.out.assignsSince(mark); len(again) != 0 {
		t.Fatalf("на invalid подключение не назначается: %+v", again)
	}
	first, _ := f.st.GetTask(ctx, as[0].TaskId)
	other, _ := f.st.GetTask(ctx, as[1].TaskId)
	if first.Status != domain.TaskFailed || other.Status == domain.TaskFailed || other.AttemptUsed != 0 ||
		!strings.Contains(other.WaitReason, "провайдер отклонил ключ подключения deepseek") {
		t.Fatalf("первая эскалирована, вторая ждёт подключения: %s / %s %q", first.Status, other.Status, other.WaitReason)
	}
	_ = second
}
