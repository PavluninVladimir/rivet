package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Интервью: нормализация, лимиты, мягкое удаление с пометкой версий
// устаревшими (спека product-discovery «Интервью проекта»).
func TestDiscoveryInterviews(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	iv, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{
		Title: "Менеджер", RespondentRole: "Менеджер по продажам", HeldAt: "2026-10-01",
		Text: "\ufeff**И:** Как вы работаете?\r\n**Р:** Веду клиентов в Excel и путаюсь в дублях.\r\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if iv.Lines != 2 || iv.CreatedBy != owner.Login || iv.HeldAt == nil || *iv.HeldAt != "2026-10-01" || strings.Contains(iv.Text, "\r") || strings.HasPrefix(iv.Text, "\ufeff") {
		t.Fatalf("интервью: %+v", iv)
	}
	for _, bad := range []NewInterview{
		{Title: "", Text: "текст"},
		{Title: "x", Text: "   \n "},
		{Title: "x", Text: "текст", HeldAt: "01.10.2026"},
		{Title: "x", Text: string([]byte{0xff, 0xfe})},
	} {
		var fe *DiscoveryFieldError
		if _, err := s.AddInterview(ctx, p.ID, owner.ID, bad); !errors.As(err, &fe) {
			t.Fatalf("ожидалась ошибка поля для %+v: %v", bad, err)
		}
	}
	if _, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{Title: "big", Text: strings.Repeat("я", MaxInterviewChars+1)}); !errors.Is(err, ErrInterviewTooLarge) {
		t.Fatalf("лимит размера: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO discovery_interviews (project_id, title, text, sha256, chars, lines)
		SELECT $1, 'x'||g, 'текст интервью', 'sha', 1, 1 FROM generate_series(1, $2) g`, p.ID, MaxInterviews-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddInterview(ctx, p.ID, owner.ID, NewInterview{Title: "лишнее", Text: "текст"}); !errors.Is(err, ErrInterviewsLimit) {
		t.Fatalf("лимит числа: %v", err)
	}

	run, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: []RunInterview{{ID: iv.ID, Title: iv.Title, SHA256: iv.SHA256}}, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteInterview(ctx, p.ID, iv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteInterview(ctx, p.ID, iv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("повторное удаление: %v", err)
	}
	got, err := s.GetRun(ctx, p.ID, run.ID)
	if err != nil || !got.Outdated {
		t.Fatalf("версия должна стать устаревшей: %+v %v", got, err)
	}
	list, _ := s.ListInterviews(ctx, p.ID)
	for _, x := range list {
		if x.ID == iv.ID {
			t.Fatal("удалённое интервью в списке")
		}
	}
	if del, err := s.GetInterview(ctx, p.ID, iv.ID); err != nil || !del.Deleted || del.Text == "" {
		t.Fatalf("текст удалённого интервью нужен для цитат: %+v %v", del, err)
	}
}

func syntheticResult() discovery.Synthesis {
	return discovery.Synthesis{
		Personas: []discovery.Persona{{ID: "p1", Name: "Менеджер", Interviews: []string{"i1"}}},
		Needs: []discovery.Need{
			{ID: "n1", Title: "Нет дублей контактов", Kind: "pain", Persona: "p1", Importance: "high",
				Evidence: []discovery.QuoteRef{{Interview: "i1", Line: 2, Text: "путаюсь в дублях"}}},
			{ID: "n2", Title: "Видеть этап сделки", Kind: "need", Persona: "p1", Importance: "high",
				Evidence: []discovery.QuoteRef{{Interview: "i1", Line: 3, Text: "не вижу, где сделка"}}},
		},
		Epics: []discovery.BacklogEpic{
			{Key: "e1", Title: "Компании и контакты", Goal: strings.Repeat("Цель про контакты и компании. ", 3), Needs: []string{"n1"}, Priority: "must", Size: "M", DependsOn: []string{}},
			{Key: "e2", Title: "Сделки и воронка", Goal: strings.Repeat("Цель про сделки и воронку продаж. ", 3), Needs: []string{"n2"}, Priority: "should", Size: "L", DependsOn: []string{"e1"}},
		},
	}
}

// Версии: один анализ на проект, ready вытесняет прошлую неутверждённую,
// правка бэклога с ревизией, утверждение создаёт Epic'и с потребностями и
// зависимостями, повторное утверждение отклоняется.
func TestDiscoveryRunsAndApprove(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "crm", "o/crm", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	set := []RunInterview{{ID: "i1", Title: "Менеджер", SHA256: "sha"}}
	r1, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m", AgentName: "Вера"})
	if err != nil || r1.Version != 1 || r1.Status != RunRunning || r1.ProgressTotal != 1 {
		t.Fatalf("первая версия: %+v %v", r1, err)
	}
	var running *AnalysisRunningError
	if _, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: set}); !errors.As(err, &running) || running.RunID != r1.ID {
		t.Fatalf("второй анализ должен отклоняться: %v", err)
	}
	if _, err := s.FinishRunFailed(ctx, r1.ID, "сбой", []string{"дефект"}, nil); err != nil {
		t.Fatal(err)
	}
	r2, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: set, Model: "m"})
	if err != nil || r2.Version != 2 {
		t.Fatalf("вторая версия: %+v %v", r2, err)
	}
	if err := s.AddRunUsage(ctx, r2.ID, ptr64(100), ptr64(20), nil, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRunUsage(ctx, r2.ID, ptr64(50), nil, nil, 2); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishRunReady(ctx, r2.ID, syntheticResult(), nil, nil); err != nil || !ok {
		t.Fatalf("ready: %v %v", ok, err)
	}
	old, _ := s.GetRun(ctx, p.ID, r1.ID)
	if old.Status != RunSuperseded {
		t.Fatalf("прошлая неутверждённая версия должна стать superseded: %s", old.Status)
	}
	cur, err := s.CurrentRun(ctx, p.ID)
	if err != nil || cur == nil || cur.ID != r2.ID || cur.BacklogRev != 1 || len(cur.Backlog) != 2 {
		t.Fatalf("текущая версия: %+v %v", cur, err)
	}
	if *cur.TokensIn != 150 || *cur.TokensOut != 20 || cur.CostUSD != nil || cur.DurationS != 5 {
		t.Fatalf("usage версии: in=%v out=%v cost=%v d=%d", *cur.TokensIn, *cur.TokensOut, cur.CostUSD, cur.DurationS)
	}

	// Правка бэклога: ревизия, проверка, конфликт.
	edited := append([]discovery.BacklogEpic{}, cur.Backlog...)
	edited[1].Priority = "must"
	var conflict *BacklogConflictError
	if _, err := s.SaveBacklog(ctx, p.ID, r2.ID, 7, edited); !errors.As(err, &conflict) || conflict.Rev != 1 {
		t.Fatalf("конфликт ревизии: %v", err)
	}
	bad := append([]discovery.BacklogEpic{}, edited...)
	bad[0].DependsOn = []string{"e2"}
	var fe *DiscoveryFieldError
	if _, err := s.SaveBacklog(ctx, p.ID, r2.ID, 1, bad); !errors.As(err, &fe) {
		t.Fatalf("цикл в бэклоге должен отклоняться: %v", err)
	}
	saved, err := s.SaveBacklog(ctx, p.ID, r2.ID, 1, edited)
	if err != nil || saved.BacklogRev != 2 || saved.Backlog[1].Priority != "must" || saved.Result.Epics[1].Priority != "should" {
		t.Fatalf("сохранение правки (результат модели не меняется): %+v %v", saved, err)
	}

	// Существующий Epic проекта, связь по ключу existing_epic.
	pre, err := s.CreateEpic(ctx, p.ID, "Импорт контактов", "старый Epic")
	if err != nil {
		t.Fatal(err)
	}
	edited = append(saved.Backlog, discovery.BacklogEpic{Key: "h1", Title: "Импорт из Excel", Goal: strings.Repeat("Импорт контактов из Excel. ", 3),
		Priority: "could", Size: "S", ExistingEpic: &pre.ID, DependsOn: []string{"e1"}})
	saved, err = s.SaveBacklog(ctx, p.ID, r2.ID, 2, edited)
	if err != nil {
		t.Fatalf("правка с человеческим Epic'ом: %v", err)
	}

	res, err := s.ApproveRun(ctx, p.ID, r2.ID, owner.ID, saved.BacklogRev, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.Status != RunApproved || len(res.Epics) != 3 || res.Epics[2].Action != "linked" || res.Epics[2].EpicID != pre.ID {
		t.Fatalf("утверждение: %+v", res)
	}
	if strings.Join(res.Order, ",") != "e1,e2" || res.Run.Decompose["e1"].Status != "pending" {
		t.Fatalf("порядок декомпозиции созданных: %v %v", res.Order, res.Run.Decompose)
	}
	if _, err := s.ApproveRun(ctx, p.ID, r2.ID, owner.ID, saved.BacklogRev, false); !errors.Is(err, ErrRunNotApprovable) {
		t.Fatalf("повторное утверждение: %v", err)
	}
	if _, err := s.SaveBacklog(ctx, p.ID, r2.ID, saved.BacklogRev, saved.Backlog); !errors.Is(err, ErrRunNotEditable) {
		t.Fatalf("правка утверждённой версии: %v", err)
	}

	epics, _ := s.ListEpics(ctx, p.ID)
	if err := s.EnrichEpics(ctx, epics); err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]domain.Epic{}
	for _, e := range epics {
		byTitle[e.Title] = e
	}
	deals := byTitle["Сделки и воронка"]
	if deals.Priority == nil || *deals.Priority != "must" || len(deals.DependsOn) != 1 || deals.DependsOn[0] != byTitle["Компании и контакты"].ID {
		t.Fatalf("Epic сделок: %+v", deals)
	}
	if deals.Discovery == nil || deals.Discovery.Version != 2 || len(deals.Discovery.Needs) != 1 || !strings.Contains(string(deals.Discovery.Needs[0]), "не вижу, где сделка") {
		t.Fatalf("обоснование Epic'а: %+v", deals.Discovery)
	}
	if len(deals.WaitingFor) != 1 || deals.WaitingFor[0].Title != "Компании и контакты" {
		t.Fatalf("ожидание зависимости: %+v", deals.WaitingFor)
	}
	needs, err := s.EpicNeeds(ctx, deals.ID)
	if err != nil || len(needs) != 1 || needs[0].ID != "n2" {
		t.Fatalf("потребности для декомпозиции: %+v %v", needs, err)
	}
	if err := s.SetRunDecompose(ctx, r2.ID, "e1", "done", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, p.ID, r2.ID)
	if got.Decompose["e1"].Status != "done" || got.Decompose["e2"].Status != "pending" {
		t.Fatalf("ход декомпозиции: %+v", got.Decompose)
	}
}

// Зависимости Epic'ов: сам Epic, чужой проект, цикл, закрытый Epic;
// ожидание незавершённой и архивированной зависимости.
func TestEpicDeps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "a", "o/a", nil, owner.ID)
	other, _ := s.CreateProject(ctx, "b", "o/b", nil, owner.ID)
	a, _ := s.CreateEpic(ctx, p.ID, "A", "")
	b, _ := s.CreateEpic(ctx, p.ID, "B", "")
	c, _ := s.CreateEpic(ctx, p.ID, "C", "")
	foreign, _ := s.CreateEpic(ctx, other.ID, "X", "")

	var fe *DiscoveryFieldError
	if _, err := s.SetEpicDeps(ctx, a.ID, []string{a.ID}); !errors.As(err, &fe) {
		t.Fatalf("сам Epic: %v", err)
	}
	if _, err := s.SetEpicDeps(ctx, a.ID, []string{foreign.ID}); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "другого проекта") {
		t.Fatalf("чужой проект: %v", err)
	}
	if _, err := s.SetEpicDeps(ctx, b.ID, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetEpicDeps(ctx, c.ID, []string{b.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetEpicDeps(ctx, a.ID, []string{c.ID}); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "цикл") {
		t.Fatalf("цикл: %v", err)
	}
	waiting, err := s.WaitingDeps(ctx)
	if err != nil || len(waiting[b.ID]) != 1 || len(waiting[c.ID]) != 1 || len(waiting[a.ID]) != 0 {
		t.Fatalf("ожидание: %+v %v", waiting, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='done' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='archived' WHERE id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	waiting, _ = s.WaitingDeps(ctx)
	if len(waiting[b.ID]) != 0 || len(waiting[c.ID]) != 1 || waiting[c.ID][0].Status != "archived" {
		t.Fatalf("архивированная незавершённая зависимость держит: %+v", waiting)
	}
	if _, err := s.SetEpicDeps(ctx, a.ID, nil); !errors.Is(err, ErrEpicClosed) {
		t.Fatalf("закрытый Epic: %v", err)
	}
}

// Кэш конспектов и прерванные анализы.
func TestDiscoverySummaryCacheAndRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, _ := s.CreateProject(ctx, "a", "o/a", nil, owner.ID)
	sum := discovery.Summary{Respondent: discovery.Respondent{Role: "менеджер"}, Findings: []discovery.Finding{{Kind: "pain", Text: "дубли", Importance: "high"}}}
	if _, _, ok, err := s.CachedSummary(ctx, "sha", "m", "instr"); ok || err != nil {
		t.Fatalf("пустой кэш: %v %v", ok, err)
	}
	if err := s.PutSummary(ctx, "sha", "m", "instr", sum, nil); err != nil {
		t.Fatal(err)
	}
	got, _, ok, err := s.CachedSummary(ctx, "sha", "m", "instr")
	if err != nil || !ok || got.Findings[0].Text != "дубли" {
		t.Fatalf("кэш: %+v %v %v", got, ok, err)
	}
	if _, _, ok, _ := s.CachedSummary(ctx, "sha", "other-model", "instr"); ok {
		t.Fatal("другая модель — промах кэша")
	}
	r, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: []RunInterview{{ID: "i1"}}})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.FailInterruptedRuns(ctx)
	if err != nil || len(failed) != 1 || failed[0].ID != r.ID {
		t.Fatalf("прерванные: %+v %v", failed, err)
	}
	got2, _ := s.GetRun(ctx, p.ID, r.ID)
	if got2.Status != RunFailed || !strings.Contains(got2.Error, "перезапуск") {
		t.Fatalf("после рестарта: %+v", got2)
	}
	if ok, err := s.FinishRunReady(ctx, r.ID, syntheticResult(), nil, nil); err != nil || ok {
		t.Fatalf("прерванная версия не становится ready: %v %v", ok, err)
	}
}

func ptr64(v int64) *int64 { return &v }
