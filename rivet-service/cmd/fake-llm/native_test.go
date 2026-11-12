package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/runner/native"
	"github.com/PavluninVladimir/rivet/internal/scm"
)

// Сценарии fake-llm прогоняются настоящим агентским циклом Rivet по HTTP:
// так e2e-стенд получает детерминированного агента для всех ролей.

func runScenario(t *testing.T, srvURL, dir, task string) (native.Result, []native.Event) {
	return runScenarioModel(t, srvURL, dir, "fake-small", task)
}

func runScenarioModel(t *testing.T, srvURL, dir, model, task string) (native.Result, []native.Event) {
	t.Helper()
	var events []native.Event
	res, err := native.Run(context.Background(), native.Config{
		Client: llm.Client{API: llm.APIOpenAI, BaseURL: srvURL + "/v1", Key: "fake-key"},
		Model:  model, Dir: dir,
		Exec:   native.ExecConfig{KillGrace: 200 * time.Millisecond},
		OnStep: func(ev native.Event) { events = append(events, ev) },
	}, task)
	if err != nil {
		t.Fatalf("сценарий %q: %v", firstLine(task), err)
	}
	return res, events
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func writtenFiles(events []native.Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Files...)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestNativeScenarios(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	t.Run("разработчик", func(t *testing.T) {
		dir := t.TempDir()
		res, events := runScenario(t, srv.URL, dir, "Твоя роль в команде Rivet: Разработчик Макс.\n# Задача\nСделай компании")
		if res.Verdict != "ok" || !strings.HasSuffix(res.FinalText, "VERDICT: APPROVED") {
			t.Fatalf("итог: %+v", res)
		}
		if got := readFile(t, filepath.Join(dir, "e2e-result.txt")); strings.Count(got, "work ") != 1 {
			t.Fatalf("e2e-result.txt: %q", got)
		}
		// Второй проход дописывает строку, а не перезаписывает; в строке
		// номер, а не метка времени — содержимое детерминировано.
		runScenario(t, srv.URL, dir, "# Задача\nисправление")
		if got := readFile(t, filepath.Join(dir, "e2e-result.txt")); got != "work 1\nwork 2\n" {
			t.Fatalf("второй проход: %q", got)
		}
		var ranCheck bool
		for _, e := range events {
			if e.Tool == "run" && e.OK {
				ranCheck = true
			}
		}
		if !ranCheck || strings.Join(writtenFiles(events), ",") != "e2e-result.txt" {
			t.Fatalf("шаги разработчика: %+v", events)
		}
	})

	t.Run("вопрос человеку", func(t *testing.T) {
		dir := t.TempDir()
		res, _ := runScenario(t, srv.URL, dir, "# Задача\nПравило [e2e-block]")
		if res.FinalText != "BLOCKED: вопрос от e2e-агента: подтвердите поведение" {
			t.Fatalf("ask_human: %q", res.FinalText)
		}
		res, _ = runScenario(t, srv.URL, dir, "# Задача\nПравило [e2e-block]\nУточнение человека: да")
		if res.Verdict != "ok" {
			t.Fatalf("после ответа человека работа идёт: %+v", res)
		}
	})

	t.Run("fail", func(t *testing.T) {
		res, _ := runScenario(t, srv.URL, t.TempDir(), "# Задача\nНевозможное [e2e-fail]")
		if res.Verdict != "fail" || !strings.Contains(res.FinalText, "VERDICT: FAIL:") {
			t.Fatalf("fail: %+v", res)
		}
	})

	t.Run("ревьюер", func(t *testing.T) {
		dir := t.TempDir()
		// В общем файле e2e уже много строк других задач: проход
		// считается по diff задачи, а не по файлу.
		if err := os.WriteFile(filepath.Join(dir, "e2e-result.txt"), []byte("work 1\nwork 2\nwork 3\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		review := func(diff string) string {
			return "Ты — независимый ревьюер задачи task-1: компании [e2e-native-changes].\n\n# Diff PR\n```diff\n" + diff +
				"\n```\n\nЗакончи «VERDICT: APPROVED» или «VERDICT: CHANGES: …»."
		}
		res, events := runScenario(t, srv.URL, dir, review("--- a/e2e-result.txt\n+++ b/e2e-result.txt\n+work 4"))
		if res.Verdict != "changes" || !strings.Contains(res.FinalText, "VERDICT: CHANGES: e2e-ревьюер") {
			t.Fatalf("первый проход: %+v", res)
		}
		if len(writtenFiles(events)) != 0 {
			t.Fatal("ревьюер ничего не пишет")
		}
		if res, _ := runScenario(t, srv.URL, dir, review("+++ b/e2e-result.txt\n+work 4\n+work 5")); res.Verdict != "ok" {
			t.Fatalf("второй проход: %+v", res)
		}
		// [e2e-changes] — метка шага prompt, ревьюер на неё не реагирует.
		if res, _ := runScenario(t, srv.URL, t.TempDir(), "Ты — независимый ревьюер задачи task-2 [e2e-changes]"); res.Verdict != "ok" {
			t.Fatalf("ревью без метки native: %+v", res)
		}
	})

	// Стенд e2e: задание как у reviewPrompt с ролью (метка в заголовке,
	// описания нет), diff PR — заглушка fake SCM без файлов задачи. Проход
	// считается по e2e-result.txt ветки: раньше заглушка давала ноль строк
	// work, и сценарий не мог дойти до одобрения.
	t.Run("ревьюер с diff fake SCM", func(t *testing.T) {
		fakeDiff, err := scm.NewFakeGit("").Diff(context.Background(), "e2e/demo", 16)
		if err != nil {
			t.Fatal(err)
		}
		review := "# Роль\nТвоя роль в команде Rivet: Ревьюер Ирина.\nТы независимый ревьюер. Читай diff как опытный коллега.\n\n" +
			"Review задачи task-18: Ревьюер changes 1 [e2e-native-changes].\n\n# Acceptance criteria\n- e2e-результат записан\n\n" +
			"# Diff PR\n```diff\n" + fakeDiff + "\n```\n\n" +
			"Проверь изменения в текущем каталоге (ветка agent/task-18) по критериям и качеству кода.\n"
		dir := t.TempDir()
		result := filepath.Join(dir, "e2e-result.txt")
		if err := os.WriteFile(result, []byte("work 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res, _ := runScenario(t, srv.URL, dir, review); res.Verdict != "changes" {
			t.Fatalf("первый проход по ветке с одной строкой work: %+v", res)
		}
		if err := os.WriteFile(result, []byte("work 1\nwork 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res, _ := runScenario(t, srv.URL, dir, review); res.Verdict != "ok" {
			t.Fatalf("второй проход после исправления: %+v", res)
		}
	})

	t.Run("тестировщик", func(t *testing.T) {
		dir := t.TempDir()
		tester := func(diff string) string {
			return "# Роль\nТвоя роль в команде Rivet: Тестировщик Лев.\n\nТы проверяешь задачу task-1 в ветке feature/task-1 по критериям приёмки.\n\n" +
				"# Задача: компании\n\n[e2e-tester-changes]\n\n# Изменения задачи относительно main\n```diff\n" + diff + "\n```\n"
		}
		// Чужие тесты в общем файле не делают первый проход вторым.
		if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tests/e2e-tests.txt"), []byte("test 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, events := runScenario(t, srv.URL, dir, tester("+++ b/e2e-result.txt\n+work 1"))
		if res.Verdict != "changes" {
			t.Fatalf("первый проход тестировщика: %+v", res)
		}
		for _, f := range writtenFiles(events) {
			if !strings.HasPrefix(f, "tests/") {
				t.Fatalf("тестировщик правит только tests/: %v", f)
			}
		}
		if got := readFile(t, filepath.Join(dir, "tests/e2e-tests.txt")); got != "test 1\ntest 2\n" {
			t.Fatalf("тесты дописаны строкой с номером: %q", got)
		}
		if res, _ := runScenario(t, srv.URL, dir, tester("+++ b/tests/e2e-tests.txt\n+test 2")); res.Verdict != "ok" {
			t.Fatalf("второй проход: %+v", res)
		}
		code := "Ты проверяешь задачу task-3 в ветке feature/task-3.\n# Задача: компании [e2e-tester-code]"
		res, events = runScenario(t, srv.URL, t.TempDir(), code)
		if res.Verdict != "ok" || strings.Join(writtenFiles(events), ",") != "src/e2e-tester.js" {
			t.Fatalf("правка вне тестов (runner откатит): %+v %v", res, writtenFiles(events))
		}
		// Блок роли без задания тестировщика — не тестировщик.
		if _, events := runScenario(t, srv.URL, t.TempDir(), "Твоя роль в команде Rivet: Тестировщик Лев.\n# Задача\nx"); strings.Join(writtenFiles(events), ",") != "e2e-result.txt" {
			t.Fatalf("роль по заданию, а не по блоку роли: %v", writtenFiles(events))
		}
	})

	t.Run("политика", func(t *testing.T) {
		var tr strings.Builder
		_, err := native.Run(context.Background(), native.Config{
			Client: llm.Client{API: llm.APIOpenAI, BaseURL: srv.URL + "/v1", Key: "fake-key"},
			Model:  "fake-small", Dir: t.TempDir(),
			Exec:         native.ExecConfig{KillGrace: 200 * time.Millisecond},
			OnTranscript: func(b []byte) { tr.Write(b) },
		}, "# Задача\nx\n\n# Политика проекта (версия abc123def456)\n- Файлы политики в каталоге .rivet менять нельзя.\n")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tr.String(), "Учитываю политику: # Политика проекта (версия abc123def456) - Файлы политики") {
			t.Fatalf("строка политики в транскрипте:\n%s", tr.String())
		}
	})

	t.Run("шаг prompt", func(t *testing.T) {
		res, _ := runScenario(t, srv.URL, t.TempDir(), "Ты работаешь над задачей task-1.\n\n# Задание шага процесса\n\nпроверь миграции [e2e-changes]")
		if res.Verdict != "changes" {
			t.Fatalf("prompt: %+v", res)
		}
		res, _ = runScenario(t, srv.URL, t.TempDir(), "# Задание шага процесса\nобнови changelog")
		if res.Verdict != "ok" {
			t.Fatalf("prompt ok: %+v", res)
		}
	})
}

// msgsAfter — история: задание и n ходов с вызовами инструментов.
func msgsAfter(t *testing.T, task string, n int) []nativeMsg {
	t.Helper()
	raw := []map[string]any{{"role": "user", "content": task}}
	for i := 0; i < n; i++ {
		raw = append(raw, map[string]any{"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": "c", "function": map[string]any{"name": "list_dir", "arguments": "{}"}}}})
	}
	b, _ := json.Marshal(raw)
	var msgs []nativeMsg
	if err := json.Unmarshal(b, &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

// [e2e-slow]: после записи файла — run «sleep 6», затем обычная проверка.
func TestSlowScenario(t *testing.T) {
	task := "# Задача\nНаблюдение за сессией [e2e-slow]"
	if a := nextNativeAction(msgsAfter(t, task, 2)); a.tool != "write_file" {
		t.Fatalf("шаг 2: %+v", a)
	}
	if a := nextNativeAction(msgsAfter(t, task, 3)); a.tool != "run" || a.args["command"] != "sleep 6" {
		t.Fatalf("шаг 3: %+v", a)
	}
	if a := nextNativeAction(msgsAfter(t, task, 4)); a.tool != "run" || a.args["command"] != "test -s e2e-result.txt" {
		t.Fatalf("шаг 4: %+v", a)
	}
	if a := nextNativeAction(msgsAfter(t, task, 5)); a.tool != "finish" {
		t.Fatalf("шаг 5: %+v", a)
	}
	if a := nextNativeAction(msgsAfter(t, "# Задача\nобычная", 3)); a.args["command"] != "test -s e2e-result.txt" {
		t.Fatalf("без метки паузы нет: %+v", a)
	}
}

// Запрос без инструментов (планировщик) получает прежний план.
func TestPlannerResponseUnchanged(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{"model": "fake-planner", "messages": []map[string]string{{"role": "user", "content": "# Epic: CRM\nцель"}}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fake-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	var plan []map[string]any
	if err := json.Unmarshal([]byte(out.Choices[0].Message.Content), &plan); err != nil || len(plan) != 2 || plan[0]["title"] != "Подготовка: CRM" {
		t.Fatalf("план: %v %v", err, out)
	}
}

// Thinking-модель: запрос с tools, где у сообщения ассистента с tool_calls
// нет reasoning_content, отклоняется 400 (как DeepSeek V4). Цикл Rivet
// возвращает reasoning_content обратно, поэтому его сессия доходит до конца.
func TestThinkingReasoningRequired(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	// Прямой запрос без reasoning_content → 400.
	body := map[string]any{
		"model": "fake-think",
		"tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "run"}}},
		"messages": []map[string]any{
			{"role": "user", "content": "# Задача\nсделай"},
			{"role": "assistant", "tool_calls": []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "list_dir", "arguments": "{}"}}}},
			{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		},
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer fake-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("без reasoning_content ждём 400, получили %d", resp.StatusCode)
	}

	// Та же метка с reasoning_content → ответ (400 нет).
	body["messages"].([]map[string]any)[1]["reasoning_content"] = "думал"
	raw, _ = json.Marshal(body)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer fake-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("с reasoning_content ждём 200, получили %d", resp.StatusCode)
	}

	// Полная сессия цикла на thinking-модели доходит до finish: цикл
	// возвращает reasoning_content во всех последующих запросах.
	res, _ := runScenarioModel(t, srv.URL, t.TempDir(), "fake-think", "Твоя роль: Разработчик.\n# Задача\nсделай")
	if res.Verdict != "ok" {
		t.Fatalf("сессия thinking-модели: %+v", res)
	}
}

// [e2e-break] ломает проверку наблюдения стенда, задача с критерием
// «Проверка наблюдения окружения» чинит её (add-sdlc-cycle).
func TestHealthBreakAndFix(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	dir := t.TempDir()
	runScenario(t, srv.URL, dir, "# Задача\nРелиз [e2e-break]")
	if got := readFile(t, filepath.Join(dir, "health.txt")); got != "fail\n" {
		t.Fatalf("[e2e-break]: %q", got)
	}
	runScenario(t, srv.URL, dir, "# Задача\nПочинить\n# Acceptance criteria\n- Проверка наблюдения окружения")
	if got := readFile(t, filepath.Join(dir, "health.txt")); got != "ok\n" {
		t.Fatalf("починка: %q", got)
	}
}

// Ключ «fake-key-…» принимается, пока сценарий его не отзовёт; основной
// ключ стенда не отзывается.
func TestRevokeKey(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	status := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	revoke := func(key string) int {
		resp, err := http.Post(srv.URL+"/_e2e/revoke", "application/json", strings.NewReader(`{"key":"`+key+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status("fake-key-a") != http.StatusOK || status("other") != http.StatusUnauthorized {
		t.Fatal("ключи до отзыва")
	}
	if revoke("fake-key-a") != http.StatusNoContent || revoke("fake-key") != http.StatusBadRequest {
		t.Fatal("отзыв ключа")
	}
	if status("fake-key-a") != http.StatusUnauthorized || status("fake-key") != http.StatusOK || status("fake-key-b") != http.StatusOK {
		t.Fatal("после отзыва отклоняется только отозванный ключ")
	}
}
