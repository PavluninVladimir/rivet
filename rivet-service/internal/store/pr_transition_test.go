package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Переход через промежуточный статус идёт одной транзакцией
// (fix-pr-without-test-step): сбой изменений оставляет задачу как была,
// успех пишет событие на каждый шаг перехода.
func TestTransitionTaskVia(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	task := runningEpic(t, s, 1)[0]
	if got, _ := s.GetTask(ctx, task.ID); got.Status != domain.TaskReady {
		t.Fatalf("задача в ready: %s", got.Status)
	}
	ev := func(status domain.TaskStatus) EventInput {
		return EventInput{ActorKind: domain.ActorSystem, Type: "task.status", Text: string(status),
			Payload: map[string]any{"status": string(status)}}
	}
	boom := errors.New("сбой")
	err := s.TransitionTaskVia(ctx, task.ID, domain.TaskReview, domain.TaskFailed, ev(domain.TaskReview), ev(domain.TaskFailed),
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET step_id='review' WHERE id=$1`, task.ID); err != nil {
				return err
			}
			return boom
		})
	if !errors.Is(err, boom) {
		t.Fatalf("ошибка изменений: %v", err)
	}
	got, _ := s.GetTask(ctx, task.ID)
	var n int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id=$1 AND type='task.status' AND text IN ('review', 'failed')`, task.ID).Scan(&n)
	if got.Status != domain.TaskReady || got.StepID == "review" || n != 0 {
		t.Fatalf("сбой откатывает всё: %s шаг %q событий %d", got.Status, got.StepID, n)
	}
	if err := s.TransitionTaskVia(ctx, task.ID, domain.TaskReview, domain.TaskFailed, ev(domain.TaskReview), ev(domain.TaskFailed),
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tasks SET step_id='review' WHERE id=$1`, task.ID)
			return err
		}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, task.ID)
	rows, err := s.Pool.Query(ctx, `SELECT text FROM events WHERE task_id=$1 AND type='task.status' AND text IN ('review', 'failed') ORDER BY id`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskFailed || got.StepID != "review" || len(texts) != 2 || texts[0] != "review" || texts[1] != "failed" {
		t.Fatalf("переход через review: %s шаг %q события %v", got.Status, got.StepID, texts)
	}
	// Недопустимый первый шаг не выполняется.
	if err := s.TransitionTaskVia(ctx, task.ID, domain.TaskReview, domain.TaskFailed, ev(domain.TaskReview), ev(domain.TaskFailed), nil); err == nil {
		t.Fatal("failed → review → failed: переход не по матрице")
	}
}
