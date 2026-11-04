package store

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// agentWork — события занятости агентов по порядку записи: «id:busy».
func agentWork(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(), `
		SELECT type, project_id IS NULL AND epic_id IS NULL AND task_id IS NULL, payload
		FROM events WHERE type = ANY($1) ORDER BY id`, []string{EventAgentBusy, EventAgentFree})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		var installation bool
		var payload map[string]any
		if err := rows.Scan(&typ, &installation, &payload); err != nil {
			t.Fatal(err)
		}
		if !installation {
			t.Fatalf("событие занятости с данными проекта: %s %+v", typ, payload)
		}
		if len(payload) != 2 {
			t.Fatalf("в payload только id и busy: %+v", payload)
		}
		id, _ := payload["id"].(string)
		busy, _ := payload["busy"].(bool)
		if busy != (typ == EventAgentBusy) {
			t.Fatalf("busy не совпадает с типом: %s %+v", typ, payload)
		}
		state := "free"
		if busy {
			state = "busy"
		}
		out = append(out, id+":"+state)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Занятость агента команды — событие уровня установки (add-agent-roles):
// назначение, отвязка, вердикт и отмена дают team.agent_busy и
// team.agent_free, событие приходит в поток другого проекта без данных
// чужого проекта и не засоряет ленту аудита.
func TestAgentWorkEvents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.Pool.Exec(ctx, `DELETE FROM team_agents WHERE id='eva'`); err != nil {
		t.Fatal(err)
	}
	tasks := runningEpic(t, s, 2)
	// Runner'ов с запасом: отвязка запуска runner не освобождает.
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		if err := s.UpsertRunner(ctx, domain.Runner{ID: id, Agent: "claude-code", Capabilities: []string{"coding"}}); err != nil {
			t.Fatal(err)
		}
	}
	enterCode(t, s, tasks, nil)

	a, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Run.AgentID != "maks" {
		t.Fatalf("назначение Максу: %v %v %+v", ok, err, a.Run)
	}
	if got := strings.Join(agentWork(t, s), ","); got != "maks:busy" {
		t.Fatalf("назначение: %s", got)
	}
	evs, _ := s.Events(ctx, EventFilter{Installation: true, Type: EventAgentBusy})
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "«Макс» занят") || evs[0].ActorKind != domain.ActorScheduler {
		t.Fatalf("событие по фильтру типа в ленте аудита: %+v", evs)
	}

	// Поток другого проекта получает сигнал, но не события чужого проекта.
	owner := mustOwner(t, s)
	other, err := s.CreateProject(ctx, "other", "o/other", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.Events(ctx, EventFilter{ProjectID: other.ID, GlobalTypes: RoleStreamTypes, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range stream {
		if e.ProjectID != "" {
			t.Fatalf("в потоке другого проекта событие проекта: %+v", e)
		}
		seen = seen || e.Type == EventAgentBusy
	}
	if !seen {
		t.Fatalf("поток другого проекта без team.agent_busy: %+v", stream)
	}
	// С начала живых событий история установки в поток не попадает.
	liveFrom, err := s.LastEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live, _ := s.Events(ctx, EventFilter{ProjectID: other.ID, GlobalTypes: RoleStreamTypes, GlobalAfterID: liveFrom}); len(live) != 0 {
		t.Fatalf("история событий установки в живом потоке: %+v", live)
	}
	// Лента аудита без фильтра их не показывает.
	audit, _ := s.Events(ctx, EventFilter{Installation: true, Limit: 500})
	for _, e := range audit {
		if e.Type == EventAgentBusy || e.Type == EventAgentFree {
			t.Fatalf("занятость агента в ленте аудита: %+v", e)
		}
	}

	// Недоставленная стадия: запуск отвязан, агент свободен.
	if err := s.UnbindRun(ctx, a.Run.ID); err != nil {
		t.Fatal(err)
	}
	// Повторное назначение и вердикт.
	a, ok, err = s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Run.AgentID != "maks" {
		t.Fatalf("повторное назначение: %v %v %+v", ok, err, a.Run)
	}
	if closed, err := s.RecordVerdict(ctx, a.Run.ID, "ok", "", false); err != nil || !closed {
		t.Fatalf("вердикт: %v %v", closed, err)
	}
	// Отмена открытого запуска с агентом.
	b, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || b.Run.AgentID != "maks" || b.Run.TaskID == a.Run.TaskID {
		t.Fatalf("вторая задача Максу: %v %v %+v", ok, err, b.Run)
	}
	if _, err := s.CancelOpenRuns(ctx, b.Run.TaskID, 0); err != nil {
		t.Fatal(err)
	}
	want := "maks:busy,maks:free,maks:busy,maks:free,maks:busy,maks:free"
	if got := strings.Join(agentWork(t, s), ","); got != want {
		t.Fatalf("сигналы занятости:\n got %s\nwant %s", got, want)
	}

	// Правка запуска без смены агента сигнала не даёт.
	if _, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET detail='x' WHERE id=$1`, b.Run.ID); err != nil {
		t.Fatal(err)
	}
	// Удалённый агент сигнала не даёт: команду перечитывают по team.agent_deleted.
	more := runningEpic(t, s, 1)
	enterCode(t, s, more, nil)
	c, ok, err := s.AssignRun(ctx, nil, nil)
	if err != nil || !ok || c.Run.AgentID != "maks" {
		t.Fatalf("третья задача Максу: %v %v %+v", ok, err, c.Run)
	}
	if err := s.DeleteTeamAgent(ctx, "maks", owner.Login); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(agentWork(t, s), ","); got != want+",maks:busy" {
		t.Fatalf("после удаления агента:\n got %s", got)
	}
}

// Исключение по бюджету снимает только причину ожидания агента команды:
// причина про runner'а и причина, переписанная другим путём, остаются.
func TestClearTeamWaitsKeepsOtherReasons(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tasks := runningEpic(t, s, 3)
	team, runner, rewritten := tasks[0].ID, tasks[1].ID, tasks[2].ID
	if err := s.SetTaskTeamWait(ctx, team, "нет свободного агента роли «Разработчик»: все заняты"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskWaitReason(ctx, runner, "нет runner'а codex/gpt-5"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskTeamWait(ctx, rewritten, "нет свободного агента роли «Разработчик»: все заняты"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskWaitReason(ctx, rewritten, "нет runner'а со стадией PROMPT"); err != nil {
		t.Fatal(err)
	}
	projectID, epicID, err := s.TaskRefs(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	// Другие проект и Epic задачи не трогают.
	if got, err := s.ClearTeamWaits(ctx, []string{"00000000-0000-0000-0000-000000000001"}, []string{"00000000-0000-0000-0000-000000000002"}); err != nil || len(got) != 0 {
		t.Fatalf("чужое исключение: %v %v", got, err)
	}
	for name, ex := range map[string][2][]string{"Epic": {nil, {epicID}}, "проект": {{projectID}, nil}} {
		if err := s.SetTaskTeamWait(ctx, team, "нет свободного агента роли «Разработчик»: все заняты"); err != nil {
			t.Fatal(err)
		}
		got, err := s.ClearTeamWaits(ctx, ex[0], ex[1])
		if err != nil || len(got) != 1 || got[0] != team {
			t.Fatalf("%s исключён: снята причина у %v %v", name, got, err)
		}
		for id, want := range map[string]string{team: "", runner: "нет runner'а codex/gpt-5", rewritten: "нет runner'а со стадией PROMPT"} {
			if tk, _ := s.GetTask(ctx, id); tk.WaitReason != want {
				t.Fatalf("%s исключён: причина %q, ждали %q", name, tk.WaitReason, want)
			}
		}
	}
}
