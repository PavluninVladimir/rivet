package store

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Регистрация со списком моделей (спека runners «Регистрация runner'а»,
// протокол v11) и совместимость с одиночной моделью v10.
func TestRunnerModels(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "multi", Agent: "claude-code", Model: "opus", Models: []string{"opus", "sonnet"}, Capabilities: []string{"coding"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "single", Agent: "codex", Model: "gpt-5", Capabilities: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	multi, err := s.GetRunner(ctx, "multi")
	if err != nil || len(multi.Models) != 2 || multi.Models[1] != "sonnet" || multi.Model != "opus" {
		t.Fatalf("список моделей: %+v %v", multi, err)
	}
	single, _ := s.GetRunner(ctx, "single")
	if len(single.Models) != 1 || single.Models[0] != "gpt-5" {
		t.Fatalf("одиночная модель v10 как список: %+v", single)
	}
	list, _ := s.ListRunners(ctx)
	if len(list) != 2 || len(list[0].Models) == 0 {
		t.Fatalf("список runner'ов с моделями: %+v", list)
	}
}

// Запуск, отменённый между назначением и отправкой Assignment, сессию не
// получает (гонка Tick с any/blocked другого участника).
func TestSetRunSessionOnCancelledRun(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, _ := s.CreateUser(ctx, "o", "", "pw-testpass", false)
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	ep, _ := s.CreateEpic(ctx, p.ID, "e", "")
	task, err := s.CreateTask(ctx, ep.ID, NewTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionEpic(ctx, ep.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, ActorID: "t", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeEpic(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	proc := policy.Defaults().EffectiveProcess()
	step, _ := proc.Step("review")
	runs, err := s.EnterStep(ctx, EnterStep{TaskID: task.ID, Step: step, Entry: "ok", Process: &proc, ProcessHash: "h"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("вход на шаг: %v %d", err, len(runs))
	}
	if _, err := s.CancelOpenRuns(ctx, task.ID, 0); err != nil {
		t.Fatal(err)
	}
	sid, err := s.CreateSession(ctx, domain.Session{TaskID: task.ID, DriverKind: "scheduler", Scope: "REVIEW", Depth: domain.DepthMinimal})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := s.SetRunSession(ctx, runs[0].ID, sid)
	if err != nil || bound {
		t.Fatalf("отменённый запуск не должен получать сессию: %v %v", err, bound)
	}
}
