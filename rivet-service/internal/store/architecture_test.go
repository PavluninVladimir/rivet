package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/jackc/pgx/v5"
)

// Спека project-architecture «Снимок описания»: ok обновляет текущую и
// последнюю корректную версию, invalid и error последнюю корректную не
// трогают, absent снимает обе.
func TestSaveProjectArchitecture(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetProjectArchitecture(ctx, p.ID); err != nil || ok {
		t.Fatalf("снимка ещё нет: ok=%v err=%v", ok, err)
	}
	if st, err := s.ArchitectureStatus(ctx, p.ID); err != nil || st != "" {
		t.Fatalf("статус без снимка: %q %v", st, err)
	}

	prev, existed, err := s.SaveProjectArchitecture(ctx, p.ID, ArchitectureSave{
		Status: "ok", SHA: "sha1", Data: json.RawMessage(`{"system":"Демо"}`)})
	if err != nil || existed || prev != "" {
		t.Fatalf("первая запись: prev=%q existed=%v err=%v", prev, existed, err)
	}
	r, ok, err := s.GetProjectArchitecture(ctx, p.ID)
	if err != nil || !ok || r.Status != "ok" || r.ValidSHA != "sha1" || string(r.Defects) != "[]" || len(r.ValidData) == 0 {
		t.Fatalf("после ok: %+v %v %v", r, ok, err)
	}
	// При ok данные не дублируются в valid_data.
	var dup bool
	if err := s.Pool.QueryRow(ctx, `SELECT valid_data IS NOT NULL FROM project_architecture WHERE project_id=$1`, p.ID).Scan(&dup); err != nil || dup {
		t.Fatalf("копия данных при ok: %v %v", dup, err)
	}

	prev, existed, err = s.SaveProjectArchitecture(ctx, p.ID, ArchitectureSave{
		Status: "invalid", SHA: "sha2", Defects: json.RawMessage(`[{"severity":"error"}]`),
		Data: json.RawMessage(`{"ignored":true}`)})
	if err != nil || !existed || prev != "sha1" {
		t.Fatalf("вторая запись: prev=%q existed=%v err=%v", prev, existed, err)
	}
	r, _, _ = s.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "invalid" || r.SHA != "sha2" || r.Data != nil || r.ValidSHA != "sha1" || r.ValidReadAt == nil {
		t.Fatalf("invalid сохраняет последнюю корректную: %+v", r)
	}
	var valid map[string]any
	if err := json.Unmarshal(r.ValidData, &valid); err != nil || valid["system"] != "Демо" {
		t.Fatalf("последняя корректная версия: %s %v", r.ValidData, err)
	}

	if _, _, err := s.SaveProjectArchitecture(ctx, p.ID, ArchitectureSave{
		Status: "error", SHA: "sha2", Error: "лимит"}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "error" || r.Error != "лимит" || r.ValidSHA != "sha1" {
		t.Fatalf("error сохраняет последнюю корректную: %+v", r)
	}
	if err := json.Unmarshal(r.ValidData, &valid); err != nil || valid["system"] != "Демо" {
		t.Fatalf("данные последней корректной после второго сбоя: %s %v", r.ValidData, err)
	}

	if _, _, err := s.SaveProjectArchitecture(ctx, p.ID, ArchitectureSave{Status: "absent", SHA: "sha3"}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.GetProjectArchitecture(ctx, p.ID)
	if r.Status != "absent" || r.ValidSHA != "" || r.ValidReadAt != nil || r.ValidData != nil {
		t.Fatalf("absent снимает последнюю корректную: %+v", r)
	}

	if _, _, err := s.SaveProjectArchitecture(ctx, p.ID, ArchitectureSave{Status: "weird"}); err == nil {
		t.Fatal("неизвестный статус принят")
	}
}

func architectureInput(projectID string) ArchitectureTaskInput {
	return ArchitectureTaskInput{
		ProjectID: projectID, Title: "Архитектура проекта", Goal: "описать архитектуру",
		TaskTitle: "Описать архитектуру", Description: "промпт",
		Criteria:    []domain.Criterion{{Text: "описание проходит проверку"}},
		Process:     &policy.Resolved{Steps: []policy.ResolvedStep{{ID: "describe", Kind: "code"}}},
		ProcessHash: "hash", Trigger: "manual", UserLogin: "owner",
	}
}

// Спека project-architecture «Задача описания»: системный Epic вида
// architecture, одна незавершённая задача на проект, событие о постановке.
func TestCreateArchitectureTask(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epicID, taskID, err := s.CreateArchitectureTask(ctx, architectureInput(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	epic, err := s.GetEpic(ctx, epicID)
	if err != nil || epic.Kind != domain.KindArchitecture || epic.Status != domain.EpicRunning {
		t.Fatalf("Epic описания: %+v %v", epic, err)
	}
	// Epic создаётся сразу running: начало ставит INSERT, а не триггер
	// смены статуса (ревью переноса, P3-2).
	if epic.StartedAt == nil {
		t.Fatal("у системного Epic'а нет started_at")
	}
	// То же у Epic'а «Сопровождение».
	var maintStarted *time.Time
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		id, err := maintenanceEpicTx(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT started_at FROM epics WHERE id=$1`, id).Scan(&maintStarted)
	}); err != nil || maintStarted == nil {
		t.Fatalf("started_at Сопровождения: %v %v", maintStarted, err)
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil || task.Kind != domain.KindArchitecture || task.ProcessHash != "hash" ||
		len(task.Capabilities) != 1 || task.Capabilities[0] != "architecture" {
		t.Fatalf("задача описания: %+v %v", task, err)
	}

	_, _, err = s.CreateArchitectureTask(ctx, architectureInput(p.ID))
	var active *ErrArchitectureTaskActive
	if !errors.As(err, &active) || active.EpicID != epicID || active.TaskID != taskID {
		t.Fatalf("вторая задача описания: %v %+v", err, active)
	}

	info, ok, err := s.ArchitectureTask(ctx, p.ID)
	if err != nil || !ok || info.TaskID != taskID || info.Trigger != "manual" {
		t.Fatalf("задача для показа: %+v %v %v", info, ok, err)
	}

	evs, err := s.Events(ctx, EventFilter{ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		if ev.Type == "architecture.describe_started" && ev.TaskID == taskID && ev.Payload["trigger"] == "manual" {
			found = true
		}
	}
	if !found {
		t.Fatal("нет события architecture.describe_started")
	}

	// Задачу описания нельзя править через план, только отменить.
	title := "другое"
	if _, err := s.UpdateTaskPlan(ctx, taskID, PlanEdit{Title: &title}, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("правка задачи описания через план: %v", err)
	}

	// Обычный Epic остаётся видом work.
	work, err := s.CreateEpic(ctx, p.ID, "фича", "цель")
	if err != nil || work.Kind != domain.KindWork {
		t.Fatalf("обычный Epic: %+v %v", work, err)
	}
}

// Отметка автоописания ставится в одной транзакции с задачей: при
// активной задаче её нет, повтор после завершения задачи отклоняется
// (ревью Opus, круг 1).
func TestArchitectureAutoMark(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := s.ArchitectureAutoDone(ctx, p.ID); err != nil || done {
		t.Fatalf("до отметки: %v %v", done, err)
	}
	// Ручная задача идёт: автоописание отклоняется, отметки нет.
	_, manualID, err := s.CreateArchitectureTask(ctx, architectureInput(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	auto := architectureInput(p.ID)
	auto.MarkAuto, auto.Trigger = true, "epic_start"
	var active *ErrArchitectureTaskActive
	if _, _, err := s.CreateArchitectureTask(ctx, auto); !errors.As(err, &active) {
		t.Fatalf("автоописание при активной задаче: %v", err)
	}
	if done, _ := s.ArchitectureAutoDone(ctx, p.ID); done {
		t.Fatal("отметка осталась без задачи")
	}
	// Ручная задача завершена: автоописание ставит задачу и отметку.
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status = 'cancelled' WHERE id = $1`, manualID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status = 'done' WHERE id = (SELECT epic_id FROM tasks WHERE id = $1)`, manualID); err != nil {
		t.Fatal(err)
	}
	_, autoID, err := s.CreateArchitectureTask(ctx, auto)
	if err != nil {
		t.Fatal(err)
	}
	if done, _ := s.ArchitectureAutoDone(ctx, p.ID); !done {
		t.Fatal("отметка не поставлена вместе с задачей")
	}
	// Второй раз автоописание не срабатывает, даже когда задачи нет.
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status = 'cancelled' WHERE id = $1`, autoID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status = 'done' WHERE id = (SELECT epic_id FROM tasks WHERE id = $1)`, autoID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateArchitectureTask(ctx, auto); !errors.Is(err, ErrArchitectureAutoDone) {
		t.Fatalf("повторное автоописание: %v", err)
	}
}

// Хук у репозитория один на URL, секрет у последнего зарегистрировавшегося
// проекта: переподписка берёт только его, а при неизвестном порядке
// регистраций не трогает общий хук (ревью Opus, круг 2).
func TestProjectsToResubscribeSharedHook(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, repo string) domain.Project {
		p, err := s.CreateProjectWithRepo(ctx, name, nil, owner.ID, NewRepoConnection{
			Provider: "github", BaseURL: "https://github.com", RepoPath: repo, DefaultBranch: "main",
			Token: "ghp_" + name, TokenOwner: "bot",
		}, box)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetWebhookRegistered(ctx, p.ID, true); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ids := func() map[string]bool {
		ps, err := s.ProjectsToResubscribe(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, p := range ps {
			out[p.ID] = true
		}
		return out
	}
	solo := mk("solo", "o/solo")
	a := mk("a", "o/shared")
	b := mk("b", "o/shared")
	// a перерегистрировался позже b: хук с секретом a.
	if _, err := s.Pool.Exec(ctx, `UPDATE projects SET webhook_registered_at = now() + interval '1 minute' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	got := ids()
	if !got[solo.ID] || !got[a.ID] || got[b.ID] {
		t.Fatalf("к переподписке: %v (solo %s, a %s, b %s)", got, solo.ID, a.ID, b.ID)
	}
	// Переподписка a закрывает и b: хук общий.
	if err := s.SetWebhookEvents(ctx, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := ids(); got[a.ID] || got[b.ID] {
		t.Fatalf("после переподписки общего хука: %v", got)
	}
	// Порядок регистраций неизвестен: общий хук не трогается.
	c := mk("c", "o/legacy")
	d := mk("d", "o/legacy")
	if _, err := s.Pool.Exec(ctx, `UPDATE projects SET webhook_registered_at = NULL WHERE id IN ($1, $2)`, c.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if got := ids(); got[c.ID] || got[d.ID] {
		t.Fatalf("общий хук с неизвестным порядком: %v", got)
	}
}
