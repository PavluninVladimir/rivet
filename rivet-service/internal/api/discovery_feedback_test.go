package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/analyst"
	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/store"
)

type apiFeedbackState struct {
	Interviews  []store.DiscoveryInterview `json:"interviews"`
	Feedback    []store.DiscoveryInterview `json:"feedback"`
	Unaccounted int                        `json:"feedback_unaccounted"`
	Coverage    *struct {
		Total           int `json:"total"`
		Synthesized     int `json:"synthesized"`
		WithoutFindings int `json:"without_findings"`
		Trimmed         int `json:"trimmed"`
	} `json:"feedback_coverage"`
}

// POST /projects/{id}/discovery/feedback (api-contract add-sdlc-cycle,
// add-user-feedback): отзыв добавляет любой участник, ошибки по контракту,
// событие feedback.created, отзывы и число неучтённых в состоянии
// исследования, анализ по одним отзывам с цитатами из них.
func TestFeedbackAPI(t *testing.T) {
	llmSrv := fakeAnalystLLM(t, nil)
	f := seedDiscovery(t, llmSrv.URL)
	ctx := context.Background()
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"

	// Участник member добавляет отзыв.
	resp, body := call(t, "POST", base+"/feedback", f.member, "", map[string]any{
		"text": "После импорта Excel пропали телефоны у половины контактов.", "source": "менеджер Анна"})
	mustStatus(t, resp, http.StatusCreated, "отзыв участника")
	var fb store.DiscoveryInterview
	_ = json.Unmarshal(body, &fb)
	if fb.Kind != "feedback" || fb.Source != "менеджер Анна" || fb.Title != "Отзыв · менеджер Анна" || fb.ID == "" {
		t.Fatalf("отзыв: %s", body)
	}
	var evText string
	if err := f.st.Pool.QueryRow(ctx, `SELECT text FROM events WHERE project_id=$1 AND type='feedback.created'
		AND payload->>'material_id'=$2 AND payload->>'source'='менеджер Анна'`, f.project, fb.ID).Scan(&evText); err != nil {
		t.Fatalf("событие feedback.created: %v", err)
	}

	// Ошибки по контракту.
	for _, c := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"text": "", "source": "Анна"}, "text"},
		{map[string]any{"text": "текст", "source": ""}, "source"},
		{map[string]any{"text": strings.Repeat("я", 2001), "source": "Анна"}, "text"},
		{map[string]any{"text": "текст", "source": strings.Repeat("я", 201)}, "source"},
	} {
		resp, body := call(t, "POST", base+"/feedback", f.member, "", c.body)
		if code, field, _ := errCode(body); resp.StatusCode != http.StatusBadRequest || code != "invalid" || field != c.field {
			t.Fatalf("%v: %d %s", c.field, resp.StatusCode, body)
		}
	}
	resp, _ = call(t, "POST", base+"/feedback", f.admin, "", map[string]any{"text": "x", "source": "y"})
	mustStatus(t, resp, http.StatusNotFound, "отзыв администратором без членства")
	if _, err := f.st.Pool.Exec(ctx, `
		INSERT INTO discovery_interviews (project_id, kind, title, respondent_role, text, sha256, chars, lines)
		SELECT $1, 'feedback', 'Отзыв · ' || g, g::text, 'отзыв ' || g, md5(g::text), 8, 1 FROM generate_series(1, 498) g`, f.project); err != nil {
		t.Fatal(err)
	}
	resp, _ = call(t, "POST", base+"/feedback", f.member, "", map[string]any{"text": "Нет выгрузки в Excel для отчёта.", "source": "РОП Олег"})
	mustStatus(t, resp, http.StatusCreated, "500-й отзыв")
	resp, body = call(t, "POST", base+"/feedback", f.member, "", map[string]any{"text": "лишний", "source": "Анна"})
	if code, _, e := errCode(body); resp.StatusCode != http.StatusConflict || code != "feedback_limit" || e["limit"] != float64(500) {
		t.Fatalf("лимит отзывов: %d %s", resp.StatusCode, body)
	}
	if _, err := f.st.Pool.Exec(ctx, `DELETE FROM discovery_interviews WHERE project_id=$1 AND title ~ '^Отзыв · [0-9]+$'`, f.project); err != nil {
		t.Fatal(err)
	}

	// Состояние исследования: интервью без отзывов, отзывы и неучтённые.
	resp, body = call(t, "GET", base, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "состояние исследования")
	var st apiFeedbackState
	_ = json.Unmarshal(body, &st)
	if len(st.Interviews) != 0 || len(st.Feedback) != 2 || st.Unaccounted != 2 || st.Feedback[1].ID != fb.ID {
		t.Fatalf("состояние: %s", body)
	}

	// Короткий отзыв не валит анализ: он уходит в отброшенное с причиной.
	resp, _ = call(t, "POST", base+"/feedback", f.member, "", map[string]any{"text": "Ужас", "source": "чат"})
	mustStatus(t, resp, http.StatusCreated, "короткий отзыв")

	// Анализ по одним отзывам: потребности с цитатами из отзывов.
	resp, body = call(t, "POST", base+"/analyses", f.user, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "анализ по отзывам")
	var started apiRun
	_ = json.Unmarshal(body, &started)
	run := f.waitRun(t, started.ID, func(r apiRun) bool { return r.Status != store.RunRunning })
	if run.Status != store.RunReady || len(run.Needs) == 0 {
		t.Fatalf("анализ по отзывам: %+v", run)
	}
	quoted := false
	for _, n := range run.Needs {
		for _, q := range n.Evidence {
			if q.Interview == fb.ID && q.Line == 1 && q.Kind == "feedback" && q.InterviewTitle == "Отзыв · менеджер Анна" && strings.HasPrefix("После импорта Excel пропали телефоны у половины контактов.", q.Text) {
				quoted = true
			}
		}
	}
	if !quoted {
		t.Fatalf("нет потребности с цитатой из отзыва: %+v", run.Needs)
	}
	resp, body = call(t, "GET", base+"/analyses/"+started.ID, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "версия с отброшенным")
	var withDropped struct {
		Dropped []struct{ Kind, Ref, Reason string } `json:"dropped"`
		Defects []string                             `json:"defects"`
	}
	_ = json.Unmarshal(body, &withDropped)
	short := map[string]bool{}
	for _, d := range withDropped.Dropped {
		if strings.Contains(d.Ref, "Отзыв · чат") {
			short[d.Reason] = true
		}
	}
	if !short["цитата короче 10 символов"] || !short["в отзыве нет подтверждённых находок"] {
		t.Fatalf("короткий отзыв в отброшенном: %+v", withDropped.Dropped)
	}
	// Короткий отзыв без находок в синтез не вошёл: версия это называет.
	if !strings.Contains(strings.Join(withDropped.Defects, "\n"), "в синтез вошли 2 из 3 отзывов: без находок 1") {
		t.Fatalf("предупреждение о вошедших отзывах: %v", withDropped.Defects)
	}
	resp, body = call(t, "GET", base, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "состояние после анализа")
	st = apiFeedbackState{}
	_ = json.Unmarshal(body, &st)
	if st.Unaccounted != 0 || len(st.Feedback) != 3 || st.Coverage == nil || st.Coverage.Total != 3 || st.Coverage.Synthesized != 2 ||
		st.Coverage.WithoutFindings != 1 || st.Coverage.Trimmed != 0 {
		t.Fatalf("неучтённые и вошедшие в синтез после анализа: %s", body)
	}

	// Удаление отзыва — владельцу, участнику нет.
	resp, _ = call(t, "DELETE", base+"/interviews/"+fb.ID, f.member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "удаление отзыва участником")
	resp, _ = call(t, "DELETE", base+"/interviews/"+fb.ID, f.user, "", nil)
	mustStatus(t, resp, http.StatusNoContent, "удаление отзыва владельцем")
	if err := f.st.Pool.QueryRow(ctx, `SELECT text FROM events WHERE project_id=$1 AND type='discovery.interview_deleted'
		AND payload->>'kind'='feedback'`, f.project).Scan(&evText); err != nil || evText != "удалён отзыв: менеджер Анна" {
		t.Fatalf("событие удаления отзыва: %q %v", evText, err)
	}
}

// Источник отзыва не уходит модели на этапе конспекта и не переходит
// через общий кэш конспектов (P2 ревью add-user-feedback): одинаковый
// текст в двух проектах с разными источниками, синтез второго проекта
// видит только свой источник.
func TestFeedbackSourceNotShared(t *testing.T) {
	llmSrv := fakeAnalystLLM(t, nil)
	var mu sync.Mutex
	var summaries, syntheses []string
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &in)
		if len(in.Messages) > 0 {
			mu.Lock()
			switch first, _, _ := strings.Cut(in.Messages[0].Content, "\n"); first {
			case discovery.MarkerSummary:
				summaries = append(summaries, in.Messages[0].Content)
			case discovery.MarkerSynthesis:
				syntheses = append(syntheses, in.Messages[0].Content)
			}
			mu.Unlock()
		}
		resp, err := http.Post(llmSrv.URL+r.URL.Path, "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(rec.Close)
	f := seedDiscovery(t, rec.URL)
	other, err := f.st.CreateProject(context.Background(), "CRM 2", "o/crm2", nil, f.uid.ID)
	if err != nil {
		t.Fatal(err)
	}
	const text = "Не работает поиск по телефону клиента."
	analyse := func(project, source string) apiRun {
		t.Helper()
		base := f.srv.URL + "/api/v1/projects/" + project + "/discovery"
		resp, _ := call(t, "POST", base+"/feedback", f.user, "", map[string]any{"text": text, "source": source})
		mustStatus(t, resp, http.StatusCreated, "отзыв")
		resp, body := call(t, "POST", base+"/analyses", f.user, "", nil)
		mustStatus(t, resp, http.StatusAccepted, "анализ")
		var run apiRun
		_ = json.Unmarshal(body, &run)
		deadline := time.Now().Add(15 * time.Second)
		for {
			resp, body := call(t, "GET", base+"/analyses/"+run.ID, f.user, "", nil)
			mustStatus(t, resp, http.StatusOK, "версия")
			_ = json.Unmarshal(body, &run)
			if run.Status != store.RunRunning {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("анализ не завершился: %s", body)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if run.Status != store.RunReady {
			t.Fatalf("анализ: %+v", run)
		}
		return run
	}
	analyse(f.project, "Иван Петров, ООО Альфа")
	runB := analyse(other.ID, "РОП Олег")

	mu.Lock()
	defer mu.Unlock()
	if len(summaries) != 1 {
		t.Fatalf("конспект одинакового текста должен браться из кэша: заданий %d", len(summaries))
	}
	for _, p := range summaries {
		if strings.Contains(p, "Иван") || strings.Contains(p, "Олег") {
			t.Fatalf("источник в задании конспекта:\n%s", p)
		}
	}
	last := syntheses[len(syntheses)-1]
	if strings.Contains(last, "Иван") || strings.Contains(last, "Альфа") || !strings.Contains(last, "РОП Олег") {
		t.Fatalf("синтез второго проекта видит чужой источник:\n%s", last)
	}
	for _, n := range runB.Needs {
		for _, q := range n.Evidence {
			if q.RespondentRole != "РОП Олег" {
				t.Fatalf("цитата второго проекта с чужим источником: %+v", q)
			}
		}
	}
}

// feedbackLLM — fake-llm Аналитика с подменой: на конспект отзыва, в
// тексте которого есть garbage, модель отвечает не JSON. Считает задания
// конспекта такого отзыва и задания синтеза.
func feedbackLLM(t *testing.T, garbage string) (*httptest.Server, func() (int, int)) {
	t.Helper()
	upstream := fakeAnalystLLM(t, nil)
	var mu sync.Mutex
	broken, syntheses := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &in)
		prompt := ""
		if len(in.Messages) > 0 {
			prompt = in.Messages[0].Content
		}
		first, _, _ := strings.Cut(prompt, "\n")
		mu.Lock()
		if first == discovery.MarkerSynthesis {
			syntheses++
		}
		bad := garbage != "" && first == discovery.MarkerSummary && strings.Contains(prompt, garbage)
		if bad {
			broken++
		}
		mu.Unlock()
		if bad {
			writeJSON(w, http.StatusOK, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "Извините, не могу ответить в JSON."}, "finish_reason": "stop"}}})
			return
		}
		resp, err := http.Post(upstream.URL+r.URL.Path, "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return broken, syntheses
	}
}

func (f discFixture) analyseFinished(t *testing.T) (apiRun, []struct{ Kind, Ref, Reason string }) {
	t.Helper()
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"
	resp, body := call(t, "POST", base+"/analyses", f.user, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "анализ")
	var run apiRun
	_ = json.Unmarshal(body, &run)
	run = f.waitRun(t, run.ID, func(r apiRun) bool { return r.Status != store.RunRunning })
	resp, body = call(t, "GET", base+"/analyses/"+run.ID, f.user, "", nil)
	mustStatus(t, resp, http.StatusOK, "версия")
	var d struct {
		Dropped []struct{ Kind, Ref, Reason string } `json:"dropped"`
	}
	_ = json.Unmarshal(body, &d)
	return run, d.Dropped
}

// Только короткие отзывы (раунд 2 ревью add-user-feedback): синтеза без
// находок нет, версия failed с понятной причиной, отзыв в отброшенном.
func TestFeedbackOnlyShort(t *testing.T) {
	llm, calls := feedbackLLM(t, "")
	f := seedDiscovery(t, llm.URL)
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"
	resp, _ := call(t, "POST", base+"/feedback", f.member, "", map[string]any{"text": "Ужас", "source": "чат"})
	mustStatus(t, resp, http.StatusCreated, "короткий отзыв")
	run, dropped := f.analyseFinished(t)
	if run.Status != store.RunFailed || run.Error != analyst.NoFindingsReason {
		t.Fatalf("версия по одному короткому отзыву: %s %q", run.Status, run.Error)
	}
	if _, syntheses := calls(); syntheses != 0 {
		t.Fatalf("синтез без находок: %d запросов", syntheses)
	}
	found := false
	for _, d := range dropped {
		found = found || (strings.Contains(d.Ref, "Отзыв · чат") && d.Reason == discovery.ReasonFeedbackNoFinding)
	}
	if !found {
		t.Fatalf("короткий отзыв в отброшенном: %+v", dropped)
	}
}

// Неразобранный ответ модели на отзыв (раунд 2 ревью): анализ не падает,
// причина говорит о модели, а не об отзыве, и конспект не кэшируется:
// следующий анализ снова спрашивает модель.
func TestFeedbackUnparsedNotCached(t *testing.T) {
	const garbage = "Поиск по телефону клиента не находит"
	llm, calls := feedbackLLM(t, garbage)
	f := seedDiscovery(t, llm.URL)
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"
	for _, fb := range []map[string]any{
		{"text": garbage + " никого с плюсом в номере.", "source": "чат"},
		{"text": "После импорта Excel пропали телефоны у половины контактов.", "source": "менеджер Анна"},
	} {
		resp, _ := call(t, "POST", base+"/feedback", f.member, "", fb)
		mustStatus(t, resp, http.StatusCreated, "отзыв")
	}
	for i := 1; i <= 2; i++ {
		run, dropped := f.analyseFinished(t)
		if run.Status != store.RunReady {
			t.Fatalf("анализ %d: %s %q", i, run.Status, run.Error)
		}
		found := false
		for _, d := range dropped {
			if strings.Contains(d.Ref, "Отзыв · чат") {
				if d.Reason == discovery.ReasonFeedbackNoFinding {
					t.Fatalf("неразобранный ответ выдан за отзыв без находок: %+v", d)
				}
				found = found || d.Reason == discovery.ReasonFeedbackUnparsed
			}
		}
		if !found {
			t.Fatalf("анализ %d: нет причины неразобранного ответа: %+v", i, dropped)
		}
		if broken, _ := calls(); broken != 2*i {
			t.Fatalf("анализ %d: конспект неразобранного отзыва взят из кэша, запросов %d", i, broken)
		}
	}
}

// Сводная цифра отзывов во входе синтеза не на критическом пути: если её
// не удалось записать или прочитать, анализ доходит до ready, а
// «Исследование» открывается с feedback_coverage null.
func TestFeedbackCoverageNonFatal(t *testing.T) {
	llm, _ := feedbackLLM(t, "")
	f := seedDiscovery(t, llm.URL)
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"
	resp, _ := call(t, "POST", base+"/feedback", f.member, "", map[string]any{"text": "После импорта Excel пропали телефоны у половины контактов.", "source": "менеджер Анна"})
	mustStatus(t, resp, http.StatusCreated, "отзыв")
	if _, err := f.st.Pool.Exec(context.Background(), `ALTER TABLE discovery_runs DROP COLUMN feedback_without_findings`); err != nil {
		t.Fatal(err)
	}
	if run, _ := f.analyseFinished(t); run.Status != store.RunReady {
		t.Fatalf("анализ при сбое записи покрытия: %s %q", run.Status, run.Error)
	}
	resp, body := call(t, "GET", base, f.member, "", nil)
	mustStatus(t, resp, http.StatusOK, "исследование при сбое чтения покрытия")
	var st apiFeedbackState
	_ = json.Unmarshal(body, &st)
	if st.Coverage != nil || len(st.Feedback) != 1 {
		t.Fatalf("состояние при сбое: %s", body)
	}
}
