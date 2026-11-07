package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
)

// Спека product-discovery «Обратная связь пользователей»: отзыв хранится
// материалом вида feedback по правилам интервью, со своим лимитом, в
// анализ идёт вместе с интервью, неучтённые считаются по последней
// готовой версии.
func TestFeedback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "CRM", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Проверки полей.
	for _, c := range []struct {
		in    NewFeedback
		field string
	}{
		{NewFeedback{Text: " \n ", Source: "Анна"}, "text"},
		{NewFeedback{Text: "", Source: ""}, "text"},
		{NewFeedback{Text: "текст", Source: "  "}, "source"},
		{NewFeedback{Text: strings.Repeat("я", MaxFeedbackChars+1), Source: "Анна"}, "text"},
		{NewFeedback{Text: "текст", Source: strings.Repeat("я", MaxFeedbackSource+1)}, "source"},
		{NewFeedback{Text: "a\x00b", Source: "Анна"}, "text"},
		{NewFeedback{Text: "текст", Source: "\xff"}, "source"},
	} {
		_, err := s.AddFeedback(ctx, p.ID, owner.ID, c.in)
		var fe *DiscoveryFieldError
		if !errors.As(err, &fe) || fe.Field != c.field {
			t.Fatalf("%+v: ждали ошибку поля %s, получено %v", c.in, c.field, err)
		}
	}

	// Текст нормализуется как у интервью, источник одной строкой.
	fb, err := s.AddFeedback(ctx, p.ID, owner.ID, NewFeedback{
		Text: "После импорта Excel пропали телефоны\r\nу половины контактов.", Source: " менеджер Анна,\n чат поддержки "})
	if err != nil {
		t.Fatal(err)
	}
	if fb.Kind != MaterialFeedback || fb.Source != "менеджер Анна, чат поддержки" || fb.RespondentRole != fb.Source ||
		fb.Title != "Отзыв · менеджер Анна, чат поддержки" || fb.Lines != 2 || fb.CreatedBy != owner.Login ||
		fb.Text != "После импорта Excel пропали телефоны\nу половины контактов." {
		t.Fatalf("отзыв: %+v", fb)
	}
	if _, err := s.AddFeedback(ctx, p.ID, owner.ID, NewFeedback{Text: strings.Repeat("я", MaxFeedbackChars), Source: "Олег"}); err != nil {
		t.Fatalf("отзыв ровно на лимите длины: %v", err)
	}
	iv, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{Title: "Анна", Text: "**Р:** Сделки теряются."})
	if err != nil {
		t.Fatal(err)
	}
	if iv.Kind != MaterialInterview || iv.Source != "" {
		t.Fatalf("интервью: %+v", iv)
	}

	// Списки по видам, анализ по всем материалам.
	ivs, err := s.ListInterviews(ctx, p.ID)
	if err != nil || len(ivs) != 1 || ivs[0].ID != iv.ID {
		t.Fatalf("интервью без отзывов: %+v %v", ivs, err)
	}
	fbs, err := s.ListFeedback(ctx, p.ID)
	if err != nil || len(fbs) != 2 || fbs[1].ID != fb.ID || fbs[1].Text != "" || fbs[1].Excerpt != fb.Text ||
		len([]rune(fbs[0].Excerpt)) != FeedbackExcerptChars {
		t.Fatalf("отзывы новыми первыми с началом текста: %+v %v", fbs, err)
	}
	if n, err := s.UnaccountedFeedback(ctx, p.ID); err != nil || n != 2 {
		t.Fatalf("неучтённых до анализа: %d %v", n, err)
	}
	run, mats, err := s.StartRun(ctx, p.ID, NewRun{AgentID: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mats) != 3 || len(run.Interviews) != 3 {
		t.Fatalf("анализ по интервью и отзывам: %d %+v", len(mats), run.Interviews)
	}
	// Идущий анализ отзывы не учитывает, готовая версия учитывает.
	if n, _ := s.UnaccountedFeedback(ctx, p.ID); n != 2 {
		t.Fatalf("неучтённых во время анализа: %d", n)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET status='ready', result='{}', finished_at=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnaccountedFeedback(ctx, p.ID); n != 0 {
		t.Fatalf("неучтённых после анализа: %d", n)
	}
	late, err := s.AddFeedback(ctx, p.ID, "", NewFeedback{Text: "Нет выгрузки в Excel.", Source: "РОП"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnaccountedFeedback(ctx, p.ID); n != 1 {
		t.Fatalf("неучтённых после нового отзыва: %d", n)
	}

	// Агрегат цикла: отзыв — элемент «Исследования» и его счётчик.
	v, err := s.LoadCycle(ctx, p.ID, "7d", time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	disc := cyclePhase(t, v, cycle.Discovery)
	if disc.FeedbackUnaccounted != 1 || disc.Counters.Waiting != 1 || len(disc.Items) != 2 {
		t.Fatalf("фаза исследования: %+v", disc)
	}
	for _, it := range disc.Items {
		if it.Kind == "material" && (it.ID != late.ID || it.Title != "Отзыв · РОП") {
			t.Fatalf("элемент-отзыв: %+v", it)
		}
	}
	for _, a := range disc.Artifacts {
		if a.Kind == "interview" && a.ID != iv.ID {
			t.Fatalf("отзыв среди интервью в артефактах: %+v", disc.Artifacts)
		}
	}

	// Удаление отзыва владельцем помечает версию с ним устаревшей.
	del, err := s.DeleteInterview(ctx, p.ID, fb.ID)
	if err != nil || del.Kind != MaterialFeedback {
		t.Fatalf("удаление отзыва: %+v %v", del, err)
	}
	if r, _ := s.GetRun(ctx, p.ID, run.ID); !r.Outdated {
		t.Fatal("версия с удалённым отзывом не устарела")
	}
}

// Лимиты: 500 отзывов на проект отдельно от 50 интервью.
func TestFeedbackLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "CRM", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO discovery_interviews (project_id, kind, title, respondent_role, text, sha256, chars, lines)
		SELECT $1, 'feedback', 'Отзыв · ' || g, g::text, 'отзыв ' || g, md5(g::text), 8, 1 FROM generate_series(1, $2) g`,
		p.ID, MaxFeedback-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeedback(ctx, p.ID, owner.ID, NewFeedback{Text: "последний", Source: "Анна"}); err != nil {
		t.Fatalf("500-й отзыв: %v", err)
	}
	if _, err := s.AddFeedback(ctx, p.ID, owner.ID, NewFeedback{Text: "лишний", Source: "Анна"}); !errors.Is(err, ErrFeedbackLimit) {
		t.Fatalf("501-й отзыв: %v", err)
	}
	// Интервью отзывы не считают: прежние строки без вида — интервью.
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO discovery_interviews (project_id, title, text, sha256, chars, lines)
		SELECT $1, 'Интервью ' || g, 'текст ' || g, md5('i' || g), 7, 1 FROM generate_series(1, $2) g`,
		p.ID, MaxInterviews-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{Title: "Последнее", Text: "**Р:** текст"}); err != nil {
		t.Fatalf("50-е интервью при 500 отзывах: %v", err)
	}
	if _, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{Title: "Лишнее", Text: "**Р:** текст"}); !errors.Is(err, ErrInterviewsLimit) {
		t.Fatalf("51-е интервью: %v", err)
	}
	ivs, _ := s.ListInterviews(ctx, p.ID)
	if len(ivs) != MaxInterviews || ivs[0].Kind != MaterialInterview {
		t.Fatalf("интервью по умолчанию: %d %+v", len(ivs), ivs[0])
	}
}
