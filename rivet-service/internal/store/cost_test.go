package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func i64(v int64) *int64 { return &v }

// Оценка стоимости плана (спека monetization «Прозрачность затрат до запуска»).
func TestEpicCostEstimate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-cost", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	histEpic, _ := s.CreateEpic(ctx, p.ID, "History", "")
	plan, _ := s.CreateEpic(ctx, p.ID, "Plan", "")
	// План: две задачи с суммарной оценкой 3.
	if _, err := s.CreateTask(ctx, plan.ID, NewTask{Title: "P1", Estimate: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, plan.ID, NewTask{Title: "P2", Estimate: 2}); err != nil {
		t.Fatal(err)
	}

	// Пустая история — оценка недоступна, не нули.
	est, err := s.EpicCostEstimate(ctx, plan.ID)
	if err != nil || est.Available || est.Reason == "" {
		t.Fatalf("пустая история: %v %+v", err, est)
	}

	// История: три done-задачи проекта с удельным расходом 1000/2000/3000
	// токенов на единицу оценки; стоимость только у двух (мало для денег).
	for i, tokens := range []int64{1000, 2000, 3000} {
		task, err := s.CreateTask(ctx, histEpic.ID, NewTask{Title: fmt.Sprintf("H%d", i), Estimate: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status='done' WHERE id=$1`, task.ID); err != nil {
			t.Fatal(err)
		}
		in := tokens
		u := UsageInput{SourceMsgID: fmt.Sprintf("cost-%d", i), ProjectID: p.ID,
			EpicID: histEpic.ID, TaskID: task.ID, TokensIn: &in}
		if i < 2 {
			c := float64(tokens) / 1000
			u.CostUSD = &c
		}
		if err := s.RecordUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	est, err = s.EpicCostEstimate(ctx, plan.ID)
	if err != nil || !est.Available || est.BasedOn != "project" || est.SampleTasks != 3 {
		t.Fatalf("оценка по проекту: %v %+v", err, est)
	}
	// p25=1500, p75=2500 на единицу; план весит 3 единицы.
	if est.TokensMin != 4500 || est.TokensMax != 7500 {
		t.Fatalf("диапазон: %+v", est)
	}
	// Денег в основе меньше трёх задач — только токены.
	if est.CostMin != nil || est.CostMax != nil {
		t.Fatalf("cost при неполной истории должен отсутствовать: %+v", est)
	}

	// Другой проект без истории — падение на историю установки.
	owner2, _ := s.CreateUser(ctx, "owner-cost2", "", "pw-testpass", false)
	p2, _ := s.CreateProject(ctx, "p2", "o/r2", nil, owner2.ID)
	plan2, _ := s.CreateEpic(ctx, p2.ID, "Plan2", "")
	if _, err := s.CreateTask(ctx, plan2.ID, NewTask{Title: "X", Estimate: 1}); err != nil {
		t.Fatal(err)
	}
	est, err = s.EpicCostEstimate(ctx, plan2.ID)
	if err != nil || !est.Available || est.BasedOn != "installation" {
		t.Fatalf("оценка по установке: %v %+v", err, est)
	}
	if est.TokensMin != 1500 || est.TokensMax != 2500 {
		t.Fatalf("диапазон установки: %+v", est)
	}
}

// Бюджет Epic (спека orchestration «Бюджет Epic»).
func TestEpicBudget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, _ := s.CreateUser(ctx, "owner-eb", "", "pw-testpass", false)
	p, _ := s.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := s.CreateEpic(ctx, p.ID, "E", "")
	task, _ := s.CreateTask(ctx, epic.ID, NewTask{Title: "A"})

	if _, err := s.SetEpicBudget(ctx, epic.ID, i64(0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("бюджет 0 должен отклоняться: %v", err)
	}
	if _, err := s.SetEpicBudget(ctx, epic.ID, i64(1000)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetEpic(ctx, epic.ID)
	if got.TokenBudget == nil || *got.TokenBudget != 1000 {
		t.Fatalf("бюджет: %+v", got)
	}
	if err := s.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		EventInput{ActorKind: domain.ActorUser, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	// Расход ниже бюджета — превышений нет.
	in := int64(900)
	if err := s.RecordUsage(ctx, UsageInput{SourceMsgID: "eb-1", ProjectID: p.ID,
		EpicID: epic.ID, TaskID: task.ID, TokensIn: &in}); err != nil {
		t.Fatal(err)
	}
	ex, err := s.ExceededEpicBudgets(ctx)
	if err != nil || len(ex) != 0 {
		t.Fatalf("превышений быть не должно: %v %+v", err, ex)
	}
	// Догоняем до бюджета (>=): Epic исключается.
	in2 := int64(100)
	if err := s.RecordUsage(ctx, UsageInput{SourceMsgID: "eb-2", ProjectID: p.ID,
		EpicID: epic.ID, TaskID: task.ID, TokensIn: &in2}); err != nil {
		t.Fatal(err)
	}
	ex, err = s.ExceededEpicBudgets(ctx)
	if err != nil || len(ex) != 1 || ex[0].EpicID != epic.ID || ex[0].Used != 1000 {
		t.Fatalf("превышение: %v %+v", err, ex)
	}
	used, _ := s.EpicTokensUsed(ctx, epic.ID)
	if used != 1000 {
		t.Fatalf("used: %d", used)
	}
	// Человек поднял бюджет — исключение снимается.
	if _, err := s.SetEpicBudget(ctx, epic.ID, i64(5000)); err != nil {
		t.Fatal(err)
	}
	if ex, _ = s.ExceededEpicBudgets(ctx); len(ex) != 0 {
		t.Fatalf("после поднятия бюджета: %+v", ex)
	}
	// Снятие бюджета.
	if _, err := s.SetEpicBudget(ctx, epic.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetEpic(ctx, epic.ID); got.TokenBudget != nil {
		t.Fatalf("бюджет должен сняться: %+v", got)
	}
}
