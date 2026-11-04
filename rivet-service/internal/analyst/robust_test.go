package analyst

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

type panicModel struct{}

func (panicModel) CompleteWithUsage(context.Context, string, string, int) (string, llm.Usage, error) {
	panic("сбой разбора в горутине конспекта")
}

// Паника в горутине конспекта не роняет процесс: версия failed с причиной
// (репро ревью: errgroup не переносит панику в Wait).
func TestPanicInSummaryGoroutine(t *testing.T) {
	st := testStore(t)
	f := seed(t, st, "Анна", "Борис")
	job := f.job(t, &fakeModel{})
	job.Model.Client = panicModel{}
	(&Runner{St: st}).Run(context.Background(), job)
	run, _ := st.GetRun(context.Background(), f.project, job.Run.ID)
	if run.Status != store.RunFailed || !strings.Contains(run.Error, "внутренняя ошибка конспекта") {
		t.Fatalf("паника конспекта: %s %q", run.Status, run.Error)
	}
}

// Временная ошибка модели (обрыв потока, перегрузка) повторяется один
// раз, анализ доходит до ready. Пониженный провайдером лимит ответа —
// предупреждение версии.
func TestTransientRetryAndLoweredLimit(t *testing.T) {
	st := testStore(t)
	f := seed(t, st, "Анна")
	m := &fakeModel{
		summary: func(n int, prompt string) (string, error, bool) {
			if n == 1 {
				return "", llm.ErrStreamCut, true
			}
			return "", nil, false
		},
	}
	job := f.job(t, &lowered{m})
	(&Runner{St: st, RetryDelay: time.Millisecond}).Run(context.Background(), job)
	run, _ := st.GetRun(context.Background(), f.project, job.Run.ID)
	if run.Status != store.RunReady {
		t.Fatalf("временная ошибка должна повториться: %s %q %v", run.Status, run.Error, run.Defects)
	}
	if m.summaries != 2 {
		t.Fatalf("конспектов %d, ждали повтор", m.summaries)
	}
	if !strings.Contains(strings.Join(run.Defects, "\n"), "не принял лимит ответа") {
		t.Fatalf("предупреждение о пониженном лимите: %v", run.Defects)
	}
}

// lowered — модель, у которой провайдер понизил лимит ответа до 8192.
type lowered struct{ *fakeModel }

func (l *lowered) CompleteWithUsage(ctx context.Context, model, prompt string, maxTokens int) (string, llm.Usage, error) {
	text, u, err := l.fakeModel.CompleteWithUsage(ctx, model, prompt, maxTokens)
	u.MaxTokens = 8192
	return text, u, err
}
