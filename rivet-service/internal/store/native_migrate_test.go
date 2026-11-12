package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/policy"
)

// migrationsBefore — миграции сборки с именем меньше stop (база до обновления).
func migrationsBefore(t *testing.T, stop string) map[string][]byte {
	t.Helper()
	files, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for name, raw := range files {
		if name < stop {
			out[name] = raw
		}
	}
	return out
}

// Миграция 0034 (add-native-agent, задача 1.11) на снимке установки со
// всеми видами ссылок на удалённые профили: режим секретов и модель по
// умолчанию переходят агенту Rivet, агенты команды, шаблоны ролей, открытые
// запуски, снимки процессов задач и действующие политики ссылаются на
// rivet, политики получают новые версии с событием policy.migrated, а
// снятые runner'ы уходят в offline штатным watchdog'ом и отпускают задачу.
func TestMigration0034NativeAgent(t *testing.T) {
	ctx := context.Background()
	url := emptyDB(t)
	if err := migrateFiles(ctx, url, migrationsBefore(t, "0034")); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner := mustOwner(t, s)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Каталог подключений: две модели Anthropic, gpt-5 в каталоге нет.
	exec(`INSERT INTO model_connections (id, name, kind, api, base_url, models, updated_by) VALUES
		('anthropic', 'Anthropic', 'vendor', 'anthropic', 'https://api.anthropic.com',
		 '[{"id":"claude-opus-x","label":"Opus","source":"manual"},{"id":"claude-sonnet-x","label":"Sonnet","source":"manual"}]', $1)`, owner.ID)
	// Профили до обновления: claude-code запрещает отдавать ключи runner'ам
	// и выбирает sonnet, у codex и своей обёртки — always.
	exec(`UPDATE agents SET secrets='never', default_model='{"connection_id":"anthropic","model":"claude-sonnet-x"}' WHERE id='claude-code'`)
	exec(`UPDATE agents SET secrets='always', default_model='{"connection_id":"anthropic","model":"claude-opus-x"}' WHERE id='codex'`)
	exec(`INSERT INTO agents (id, name, adapter, command, secrets) VALUES ('my-wrap', 'Своя обёртка', 'wrap', 'my-agent', 'always')`)
	exec(`UPDATE team_agents SET agent='claude-code', connection_id='anthropic', model='claude-sonnet-x' WHERE id='maks'`)
	exec(`UPDATE team_agents SET agent='codex' WHERE id='eva'`)
	exec(`UPDATE team_agents SET agent='my-wrap' WHERE id='lev'`)
	exec(`UPDATE agent_roles SET agent='codex' WHERE id='reviewer'`)

	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateProject(ctx, "other", "o/other", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := s.CreateEpic(ctx, p.ID, "E", "g")
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE epics SET status='running' WHERE id=$1`, epic.ID)
	// Seed the old schema directly: current CreateTask requires schema 0051.
	legacyTask := func(title string) struct{ ID string } {
		var out struct{ ID string }
		if err := s.Pool.QueryRow(ctx, `INSERT INTO tasks(epic_id,title) VALUES($1,$2) RETURNING id`, epic.ID, title).Scan(&out.ID); err != nil {
			t.Fatal(err)
		}
		return out
	}
	running := legacyTask("running on retired runner")
	waiting := legacyTask("waiting for reviewer")
	done := legacyTask("completed")

	snapshot := `{"steps":[
		{"id":"code","kind":"code","participants":[{"id":"p1","agent":{"role":"developer","kind":"claude-code","model":"claude-sonnet-x"}}],"x_future":1},
		{"id":"review","kind":"review","participants":[{"id":"p1","agent":{"kind":"codex","model":"gpt-5"}},{"id":"p2","user":{"login":"vova"}}]}]}`
	exec(`UPDATE tasks SET process=$2::jsonb, step_id='code', status='running' WHERE id=$1`, running.ID, snapshot)
	exec(`UPDATE tasks SET process=$2::jsonb, step_id='review', status='review' WHERE id=$1`, waiting.ID, snapshot)
	exec(`UPDATE tasks SET process=$2::jsonb, status='done' WHERE id=$1`, done.ID, snapshot)

	// Runner'ы: занятый claude-code, агент вне каталога, runner выкатки.
	exec(`INSERT INTO runners (id, agent, status, capabilities, task_id, last_seen) VALUES ('old', 'claude-code', 'running', '{coding}', $1, now())`, running.ID)
	exec(`INSERT INTO runners (id, agent, status, capabilities, last_seen) VALUES ('ghost', 'aider', 'idle', '{coding}', now())`)
	exec(`INSERT INTO runners (id, agent, status, capabilities, last_seen) VALUES ('deployer', '', 'idle', '{deploy}', now())`)
	exec(`UPDATE tasks SET runner_id='old' WHERE id=$1`, running.ID)
	// Открытые запуски: идущий на old и ожидающий ревьюера; закрытый
	// запуск — история, его не трогаем.
	exec(`INSERT INTO task_step_runs (task_id, step_id, step_kind, participant, agent_kind, model, runner_id, role, agent_id)
		VALUES ($1, 'code', 'code', 'p1', 'claude-code', 'claude-sonnet-x', 'old', 'developer', 'maks')`, running.ID)
	exec(`INSERT INTO task_step_runs (task_id, step_id, step_kind, participant, agent_kind, model, role, role_agent, pref_agent)
		VALUES ($1, 'review', 'review', 'p1', 'codex', 'gpt-5', 'reviewer', 'codex', 'opencode')`, waiting.ID)
	exec(`INSERT INTO task_step_runs (task_id, step_id, step_kind, participant, agent_kind, verdict)
		VALUES ($1, 'code', 'code', 'p1', 'claude-code', 'ok')`, done.ID)

	// Политики: установка с участником opencode, у проекта история из двух
	// версий (действует вторая), у второго проекта ссылок нет.
	exec(`INSERT INTO policy_versions (scope, version, hash, content, created_by) VALUES ('installation', 1, 'h-inst',
		'{"auto_merge":false,"human_review_paths":[],"attempt_limit":3,"review_limit":3,"daily_token_budget":null,"auto_publish":true,
		  "process":{"steps":[{"id":"code","kind":"code","participants":[{"agent":{"kind":"opencode"}}]},{"id":"review","kind":"review"},{"id":"merge","kind":"merge"}]}}', 'adm')`)
	exec(`INSERT INTO policy_versions (scope, project_id, version, hash, content, created_by) VALUES ('project', $1, 1, 'h-p1',
		'{"agent_models":{"claude-code":{"connection_id":"anthropic","model":"claude-opus-x"}}}', 'adm')`, p.ID)
	projectDoc := `{"auto_merge":null,"human_review_paths":null,"attempt_limit":4,"review_limit":null,"daily_token_budget":null,"auto_publish":null,
		"agent_models":{"claude-code":{"connection_id":"anthropic","model":"claude-sonnet-x"},"codex":{"connection_id":"anthropic","model":"claude-opus-x"}},
		"role_models":{"reviewer":{"agent":"codex","connection_id":"anthropic","model":"claude-opus-x"},
		               "developer":{"agent":"claude-code","connection_id":"openai","model":"gpt-5"}},
		"process":{"steps":[
			{"id":"code","kind":"code","participants":[{"agent":{"kind":"claude-code","model":"claude-sonnet-x"}}]},
			{"id":"review","kind":"review","participants":[{"agent":{"kind":"codex","model":"gpt-5"}}]},
			{"id":"merge","kind":"merge"}]}}`
	exec(`INSERT INTO policy_versions (scope, project_id, version, hash, content, created_by) VALUES ('project', $1, 2, 'h-p2', $2::jsonb, 'adm')`, p.ID, projectDoc)
	exec(`INSERT INTO policy_versions (scope, project_id, version, hash, content, created_by) VALUES ('project', $1, 1, 'h-o1', '{"auto_merge":true}', 'adm')`, other.ID)

	if err := migrateFiles(ctx, url, migrationsBefore(t, "9999")); err != nil {
		t.Fatal(err)
	}

	// Каталог: один профиль rivet, самый строгий режим секретов среди
	// профилей со ссылками, модель по умолчанию claude-code (больше ссылок).
	var ids string
	if err := s.Pool.QueryRow(ctx, `SELECT string_agg(id, ',' ORDER BY id) FROM agents`).Scan(&ids); err != nil || ids != AgentRivet {
		t.Fatalf("профили после миграции: %q %v", ids, err)
	}
	prof, err := s.GetAgent(ctx, AgentRivet)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Secrets != "never" || prof.Adapter != AdapterNative || prof.DefaultModel == nil ||
		prof.DefaultModel.ConnectionID != "anthropic" || prof.DefaultModel.Model != "claude-sonnet-x" {
		t.Fatalf("профиль rivet: secrets=%s default=%+v", prof.Secrets, prof.DefaultModel)
	}

	// Агенты команды и шаблоны ролей.
	rows, err := s.Pool.Query(ctx, `SELECT id, agent, connection_id, model FROM team_agents WHERE id IN ('maks','eva','lev') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	team := map[string]string{}
	for rows.Next() {
		var id, ag, c, m string
		_ = rows.Scan(&id, &ag, &c, &m)
		team[id] = ag + " " + c + "/" + m
	}
	rows.Close()
	if team["maks"] != "rivet anthropic/claude-sonnet-x" || team["eva"] != "rivet /" || team["lev"] != "rivet /" {
		t.Fatalf("агенты команды: %v", team)
	}
	var roleAgent string
	_ = s.Pool.QueryRow(ctx, `SELECT agent FROM agent_roles WHERE id='reviewer'`).Scan(&roleAgent)
	if roleAgent != AgentRivet {
		t.Fatalf("шаблон ревьюера: %q", roleAgent)
	}

	// Открытые запуски на rivet, модель вне каталога снята; закрытый не тронут.
	type runRow struct{ kind, roleAgent, prefAgent, model string }
	readRun := func(taskID string) runRow {
		t.Helper()
		var r runRow
		if err := s.Pool.QueryRow(ctx, `SELECT agent_kind, role_agent, pref_agent, model FROM task_step_runs WHERE task_id=$1`, taskID).
			Scan(&r.kind, &r.roleAgent, &r.prefAgent, &r.model); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := readRun(running.ID); r != (runRow{"rivet", "", "", "claude-sonnet-x"}) {
		t.Fatalf("идущий запуск: %+v", r)
	}
	if r := readRun(waiting.ID); r != (runRow{"rivet", "rivet", "rivet", ""}) {
		t.Fatalf("ожидающий запуск: %+v", r)
	}
	if r := readRun(done.ID); r.kind != "claude-code" {
		t.Fatalf("закрытый запуск изменён: %+v", r)
	}

	// Снимки процессов незавершённых задач: kind rivet, модель вне каталога
	// снята, прочие поля (в том числе незнакомые) на месте; снимок готовой
	// задачи — история.
	readSnap := func(taskID string) string {
		t.Helper()
		var raw string
		if err := s.Pool.QueryRow(ctx, `SELECT process::text FROM tasks WHERE id=$1`, taskID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, id := range []string{running.ID, waiting.ID} {
		snap := readSnap(id)
		if strings.Contains(snap, "claude-code") || strings.Contains(snap, "codex") || strings.Contains(snap, "gpt-5") ||
			!strings.Contains(snap, `"model": "claude-sonnet-x"`) || !strings.Contains(snap, `"role": "developer"`) ||
			!strings.Contains(snap, `"x_future": 1`) || !strings.Contains(snap, `"login": "vova"`) {
			t.Fatalf("снимок процесса задачи: %s", snap)
		}
	}
	if snap := readSnap(done.ID); !strings.Contains(snap, "claude-code") {
		t.Fatalf("снимок готовой задачи изменён: %s", snap)
	}

	// Политика проекта: новая версия 3 от system, прежние версии не тронуты.
	versions, err := s.ListPolicyVersions(ctx, PolicyScopeProject, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 || versions[0].Version != 3 || versions[0].CreatedBy != policyMigrationActor ||
		versions[1].Hash != "h-p2" || versions[2].Hash != "h-p1" {
		t.Fatalf("версии политики проекта: %+v", versions)
	}
	var o policy.Overrides
	if err := json.Unmarshal(versions[0].Content, &o); err != nil {
		t.Fatal(err)
	}
	if versions[0].Hash != policy.Hash(o) {
		t.Fatalf("хэш версии не канонический: %s", versions[0].Hash)
	}
	if len(o.AgentModels) != 1 || o.AgentModels[AgentRivet] != (policy.AgentModel{ConnectionID: "anthropic", Model: "claude-sonnet-x"}) {
		t.Fatalf("agent_models: %+v", o.AgentModels)
	}
	if len(o.RoleModels) != 1 || o.RoleModels["reviewer"] != (policy.RoleModel{Agent: AgentRivet, ConnectionID: "anthropic", Model: "claude-opus-x"}) {
		t.Fatalf("role_models: %+v", o.RoleModels)
	}
	if o.AttemptLimit == nil || *o.AttemptLimit != 4 || o.Process == nil ||
		*o.Process.Steps[0].Participants[0].Agent != (policy.AgentRef{Kind: AgentRivet, Model: "claude-sonnet-x"}) ||
		*o.Process.Steps[1].Participants[0].Agent != (policy.AgentRef{Kind: AgentRivet}) {
		t.Fatalf("документ проекта: %s", versions[0].Content)
	}
	// Установка — тоже новой версией; у второго проекта версий не прибавилось.
	inst, instV, err := s.InstallationPolicy(ctx)
	if err != nil || instV == nil || instV.Version != 2 || inst.Process.Steps[0].Participants[0].Agent.Kind != AgentRivet {
		t.Fatalf("политика установки: %+v %v", instV, err)
	}
	if vs, _ := s.ListPolicyVersions(ctx, PolicyScopeProject, other.ID); len(vs) != 1 {
		t.Fatalf("политика без ссылок получила версию: %d", len(vs))
	}

	// Событие policy.migrated с перечнем изменений.
	type migrated struct {
		Scope       string   `json:"scope"`
		ProjectID   string   `json:"project_id"`
		Version     int      `json:"version"`
		FromVersion int      `json:"from_version"`
		Changes     []string `json:"changes"`
	}
	var evs []migrated
	rows, err = s.Pool.Query(ctx, `SELECT payload FROM events WHERE type='policy.migrated' AND actor_kind='system' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var m migrated
		_ = json.Unmarshal(raw, &m)
		evs = append(evs, m)
	}
	rows.Close()
	if len(evs) != 2 {
		t.Fatalf("событий policy.migrated: %+v", evs)
	}
	var proj migrated
	for _, e := range evs {
		if e.Scope == PolicyScopeProject {
			proj = e
		}
	}
	joined := strings.Join(proj.Changes, "\n")
	for _, want := range []string{
		"agent_models.claude-code → agent_models.rivet",
		"agent_models.codex снято",
		"role_models.reviewer.agent: codex → rivet",
		"role_models.developer снято",
		"process.code.participants[0].agent.kind: claude-code → rivet",
		"process.review.participants[0].agent.model gpt-5 снята",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("в событии нет %q: %+v", want, proj)
		}
	}
	if proj.ProjectID != p.ID || proj.Version != 3 || proj.FromVersion != 2 {
		t.Fatalf("событие проекта: %+v", proj)
	}

	// Переведённую политику можно сохранить без правки ссылок: устаревшее
	// переопределение не блокирует правку (ревью консоли, P1-1).
	limit := 5
	o.AttemptLimit = &limit
	if _, err := s.SaveProjectPolicy(ctx, p.ID, o, owner.Login); err != nil {
		t.Fatalf("сохранение переведённой политики: %v", err)
	}

	// Runner'ы: сторонние сняты, статус не тронут, last_seen давний;
	// runner выкатки остаётся. Watchdog снимает их штатно и отпускает задачу.
	var retired []string
	rows, err = s.Pool.Query(ctx, `SELECT id FROM runners WHERE retired ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		retired = append(retired, id)
	}
	rows.Close()
	if strings.Join(retired, ",") != "ghost,old" {
		t.Fatalf("снятые runner'ы: %v", retired)
	}
	var st string
	var seen time.Time
	_ = s.Pool.QueryRow(ctx, `SELECT status, last_seen FROM runners WHERE id='old'`).Scan(&st, &seen)
	if st != "running" || time.Since(seen) < 23*time.Hour {
		t.Fatalf("runner old после миграции: %s %v", st, seen)
	}
	if rn, err := s.GetRunner(ctx, "old"); err != nil || !rn.Retired {
		t.Fatalf("runner old в DTO не снят с работы: %+v %v", rn, err)
	}
	lost, _, err := s.MarkStaleRunnersOffline(ctx, 90)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0] != running.ID {
		t.Fatalf("задачи потерянных runner'ов: %v", lost)
	}
	var taskID *string
	_ = s.Pool.QueryRow(ctx, `SELECT status, task_id::text FROM runners WHERE id='old'`).Scan(&st, &taskID)
	if st != "offline" || taskID != nil {
		t.Fatalf("runner old после watchdog'а: %s %v", st, taskID)
	}
	if got := status(t, s, running.ID); got != "ready" {
		t.Fatalf("задача снятого runner'а: %s", got)
	}
	var depRetired bool
	_ = s.Pool.QueryRow(ctx, `SELECT retired FROM runners WHERE id='deployer'`).Scan(&depRetired)
	if depRetired {
		t.Fatal("runner выкатки снят с работы")
	}

	// Повтор миграции ничего не меняет: ни версий, ни событий, ни режима.
	var before, after, evBefore, evAfter int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM policy_versions`).Scan(&before)
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='policy.migrated'`).Scan(&evBefore)
	sql, _ := migrationsFS.ReadFile("migrations/0034_native_agent.sql")
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		return migrateNativeAgentDocs(ctx, tx)
	}); err != nil {
		t.Fatalf("повтор 0034: %v", err)
	}
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM policy_versions`).Scan(&after)
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type='policy.migrated'`).Scan(&evAfter)
	if prof, _ = s.GetAgent(ctx, AgentRivet); before != after || evBefore != evAfter || prof.Secrets != "never" {
		t.Fatalf("повтор 0034: версий %d → %d, событий %d → %d, secrets %s", before, after, evBefore, evAfter, prof.Secrets)
	}
}

// Чистая установка: один профиль rivet в режиме secure без модели по
// умолчанию (неиспользуемые профили настройки не передают), повторный
// Migrate ничего не делает. Режим always у профиля без ссылок агента Rivet
// не ослабляет.
func TestMigration0034CleanInstall(t *testing.T) {
	ctx := context.Background()
	url := emptyDB(t)
	if err := migrateFiles(ctx, url, migrationsBefore(t, "0034")); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE agents SET secrets='always', default_model='{"connection_id":"x","model":"y"}' WHERE id='opencode'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	var got string
	var def *string
	if err := conn.QueryRow(ctx, `SELECT string_agg(id||':'||adapter||':'||secrets, ','), max(default_model::text) FROM agents`).Scan(&got, &def); err != nil {
		t.Fatal(err)
	}
	if got != "rivet:native:secure" || def != nil {
		t.Fatalf("каталог чистой установки: %s, модель по умолчанию %v", got, def)
	}
	var versions int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM policy_versions`).Scan(&versions)
	if versions != 0 {
		t.Fatalf("версии политик на чистой установке: %d", versions)
	}
}

// Режим never у профиля без ссылок (администратор запретил отдавать ключи
// до регистрации первых runner'ов, агенты команды на профиле по умолчанию)
// переходит агенту Rivet: первые runner'ы по TLS или loopback ключей не
// получат (финальное ревью add-native-agent, P3-2; порт
// TestReproNeverWithoutRefs). То же у своего профиля с id rivet.
func TestMigration0034KeepsNeverWithoutRefs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"claude-code без ссылок", `UPDATE agents SET secrets='never', updated_at=now() WHERE id='claude-code'`},
		{"свой профиль без ссылок", `INSERT INTO agents (id, name, adapter, command, secrets) VALUES ('my-wrap', 'Обёртка', 'wrap', 'my-agent', 'never')`},
		{"профиль с id rivet", `INSERT INTO agents (id, name, adapter, command, secrets) VALUES ('rivet', 'Свой rivet', 'wrap', 'rivet-cli', 'never')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := emptyDB(t)
			if err := migrateFiles(ctx, url, migrationsBefore(t, "0034")); err != nil {
				t.Fatal(err)
			}
			conn, err := pgx.Connect(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			var refs int
			if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM runners WHERE agent <> '') + (SELECT count(*) FROM team_agents WHERE agent <> '')`).Scan(&refs); err != nil || refs != 0 {
				t.Fatalf("у установки не должно быть ссылок на профили: %d %v", refs, err)
			}
			if _, err := conn.Exec(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, url); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := conn.QueryRow(ctx, `SELECT string_agg(id||':'||adapter||':'||secrets, ',') FROM agents`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != "rivet:native:never" {
				t.Fatalf("режим never потерян: %s", got)
			}
		})
	}
}
