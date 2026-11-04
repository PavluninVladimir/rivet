package store

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Учёт конфликтов при merge (спека task-pipeline «Конфликт с базовой
// веткой при merge»): возвраты считаются отдельно от попыток задачи и от
// отказов самого шага merge, устаревший снимок ничего не меняет, сверх
// лимита — одна эскалация, вмешательство человека сбрасывает счётчик.
func TestRecordMergeConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "Epic", "цель")
	if err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, s, e.ID, NewTask{Title: "A"})
	// Задача ждёт на merge (поколение 5); у шага merge уже есть отказ
	// по замечанию человека на хостинге — он не должен смешиваться.
	enterMerge := func(gen int) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, `
			UPDATE tasks SET status='review', step_id='merge', step_gen=$2 WHERE id=$1`,
			task.ID, gen); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET step_rejections='{"merge": 1}'::jsonb WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	in := MergeConflictInput{TaskID: task.ID, ProjectID: p.ID, StepID: "merge", MergeStepID: "merge", Gen: 5, Limit: 2, Escalation: "конфликт"}

	// Устаревший снимок (задача не на этом входе в merge) ничего не меняет.
	enterMerge(6)
	if res, err := s.RecordMergeConflict(ctx, in); err != nil || !res.Stale {
		t.Fatalf("устаревший вход: %+v %v", res, err)
	}

	for want := 1; want <= 2; want++ {
		enterMerge(5)
		res, err := s.RecordMergeConflict(ctx, in)
		if err != nil || !res.Return || res.Exhausted || res.Returns != want {
			t.Fatalf("возврат %d: %+v %v", want, res, err)
		}
	}

	enterMerge(5)
	res, err := s.RecordMergeConflict(ctx, in)
	if err != nil || !res.Exhausted || res.Return || !res.Escalated || res.Returns != 2 {
		t.Fatalf("сверх лимита: %+v %v", res, err)
	}
	if res, err := s.RecordMergeConflict(ctx, in); err != nil || !res.Exhausted || res.Escalated {
		t.Fatalf("повтор сверх лимита не плодит эскалаций: %+v %v", res, err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptUsed != task.AttemptUsed || got.ReviewRejections != task.ReviewRejections {
		t.Fatalf("попытки задачи тронуты: used %d→%d, review %d→%d",
			task.AttemptUsed, got.AttemptUsed, task.ReviewRejections, got.ReviewRejections)
	}
	if got.StepRejections["merge"] != 1 || got.StepRejections[MergeConflictKey("merge")] != 2 {
		t.Fatalf("счётчики должны жить раздельно: %v", got.StepRejections)
	}

	// Человек подтверждает merge: счётчик сброшен, эскалация закрыта, возврат.
	in.Reset = true
	res, err = s.RecordMergeConflict(ctx, in)
	if err != nil || !res.Return || res.Returns != 1 {
		t.Fatalf("сброс человеком: %+v %v", res, err)
	}
	if open, _ := s.HasOpenAttention(ctx, task.ID, domain.AttMergeConflict); open {
		t.Fatal("эскалация MERGE_CONFLICT должна закрыться")
	}
	got, _ = s.GetTask(ctx, task.ID)
	if got.StepRejections["merge"] != 1 || got.StepRejections[MergeConflictKey("merge")] != 1 {
		t.Fatalf("после сброса: %v", got.StepRejections)
	}

	// Процесс без шага code: лимит 0 — сразу эскалация.
	other := mustTask(t, s, e.ID, NewTask{Title: "B"})
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET status='review', step_id='merge', step_gen=1 WHERE id=$1`, other.ID); err != nil {
		t.Fatal(err)
	}
	res, err = s.RecordMergeConflict(ctx, MergeConflictInput{TaskID: other.ID, ProjectID: p.ID, StepID: "merge", MergeStepID: "merge", Gen: 1, Limit: 0, Escalation: "нет code"})
	if err != nil || !res.Exhausted || !res.Escalated {
		t.Fatalf("лимит 0: %+v %v", res, err)
	}

	if _, err := s.RecordMergeConflict(ctx, MergeConflictInput{TaskID: "00000000-0000-0000-0000-000000000000", MergeStepID: "merge"}); err == nil {
		t.Fatal("неизвестная задача должна давать ошибку")
	}
}
