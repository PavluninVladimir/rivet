// fake-llm — OpenAI-совместимый провайдер для e2e-стенда: список моделей,
// chat completions с готовым планом декомпозиции и сценарии вызовов
// инструментов для агентского цикла Rivet (native.go). Ключ «fake-key»
// принимается, любой другой отклоняется 401 (сценарий «неверный ключ»).
// Ключи вида «fake-key-<суффикс>» тоже принимаются, пока сценарий не
// отзовёт ключ запросом POST /_e2e/revoke (сценарий «провайдер отозвал
// ключ, а подключение в каталоге ещё рабочее»).
// Задания агентов control plane (Аналитик add-product-discovery, Релиз-инженер
// и Инженер сопровождения add-sdlc-cycle) узнаются по маркеру в первой
// строке промпта и получают фиксированные корректные ответы.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", ":8283", "адрес HTTP")
	flag.Parse()
	fmt.Println("fake-llm:", *addr)
	log.Fatal(http.ListenAndServe(*addr, newMux()))
}

// newMux — маршруты провайдера (вынесены для тестов).
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	var mu sync.Mutex
	revoked := map[string]bool{}
	valid := func(header string) bool {
		key, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || (key != "fake-key" && !strings.HasPrefix(key, "fake-key-")) {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		return !revoked[key]
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !valid(r.Header.Get("Authorization")) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
				return
			}
			h(w, r)
		}
	}
	models := auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"fake-planner","object":"model","name":"Fake Planner"},{"id":"fake-small","object":"model"},{"id":"fake-large","object":"model"}]}`))
	})
	complete := auth(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		// Агентский цикл Rivet (запрос с tools): сценарии вызовов
		// инструментов (native.go).
		if isNativeRequest(raw) {
			w.Header().Set("Content-Type", "application/json")
			if msg, bad := deepseekThinkingViolation(raw); bad {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
				return
			}
			_ = json.NewEncoder(w).Encode(nativeResponse(raw))
			return
		}
		var in struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &in)
		// Задания Аналитика (add-product-discovery) узнаются по маркеру в
		// первой строке сообщения.
		for _, msg := range in.Messages {
			if content, ok := analystReply(msg.Content); ok {
				if d := analystDelay(msg.Content); d > 0 {
					select {
					case <-time.After(d):
					case <-r.Context().Done():
						return
					}
				}
				writeCompletion(w, in.Model, content)
				return
			}
		}
		// Задания Релиз-инженера и Инженера сопровождения (add-sdlc-cycle).
		prompt := ""
		for _, msg := range in.Messages {
			prompt += msg.Content + "\n"
		}
		if answer, ok := opsAnswer(prompt); ok {
			writeCompletion(w, in.Model, answer)
			return
		}
		// Epic виден в промпте строкой «# Epic: <название>».
		title := "Epic"
		for _, msg := range in.Messages {
			for _, line := range strings.Split(msg.Content, "\n") {
				if strings.HasPrefix(line, "# Epic: ") {
					title = strings.TrimPrefix(line, "# Epic: ")
				}
			}
		}
		plan := []map[string]any{
			{"title": "Подготовка: " + title, "description": "Заготовка для " + title + " (fake-llm, модель " + in.Model + ")",
				"criteria": []string{"файл result.txt создан"}, "deps": []int{}, "capabilities": []string{"coding"}, "estimate": 1},
			{"title": "Завершение: " + title, "description": "Финальный шаг для " + title,
				"criteria": []string{"result.txt содержит итог"}, "deps": []int{0}, "capabilities": []string{"coding"}, "estimate": 2},
		}
		content, _ := json.Marshal(plan)
		writeCompletion(w, in.Model, string(content))
	})
	for _, p := range []string{"/models", "/v1/models"} {
		mux.HandleFunc("GET "+p, models)
	}
	for _, p := range []string{"/chat/completions", "/v1/chat/completions"} {
		mux.HandleFunc("POST "+p, complete)
	}
	// Отзыв ключа на стороне провайдера (только стенд): основной ключ
	// стенда «fake-key» не отзывается.
	mux.HandleFunc("POST /_e2e/revoke", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || !strings.HasPrefix(in.Key, "fake-key-") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		revoked[in.Key] = true
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeCompletion(w http.ResponseWriter, model, content string) {
	out := map[string]any{"object": "chat.completion", "model": model,
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// reTask — задача состава в задании: «## task-7 · Название (баг)» или
// «- task-7 · Название (работа)».
var reTask = regexp.MustCompile(`(?m)^(?:## |- )task-(\d+) · (.+) \((работа|баг|архитектура)\)$`)

// opsAnswer — ответ на задание Релиз-инженера или Инженера сопровождения.
// В release notes один пункт ссылается на задачу не из состава: проверка
// ответа должна его отбросить.
func opsAnswer(prompt string) (string, bool) {
	first := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	switch first {
	case "[rivet:release_notes]":
		type item struct {
			Task int    `json:"task"`
			Text string `json:"text"`
		}
		resp := map[string][]item{"new": {}, "fixes": {}, "other": {}}
		for _, m := range reTask.FindAllStringSubmatch(prompt, -1) {
			var n int
			_, _ = fmt.Sscan(m[1], &n)
			sec := "new"
			if m[3] == "баг" {
				sec = "fixes"
			}
			resp[sec] = append(resp[sec], item{Task: n, Text: m[2] + " (fake-llm)"})
		}
		resp["other"] = append(resp["other"], item{Task: 99999, Text: "пункт не из состава"})
		b, _ := json.Marshal(resp)
		return string(b), true
	case "[rivet:deploy_analysis]":
		tasks := []int{}
		if m := reTask.FindStringSubmatch(prompt); m != nil {
			var n int
			_, _ = fmt.Sscan(m[1], &n)
			tasks = append(tasks, n)
		}
		b, _ := json.Marshal(map[string]any{"cause": "проверка окружения не прошла после выкатки (fake-llm)",
			"tasks": tasks, "recommendation": "воспроизвести проверку локально и исправить причину (fake-llm)"})
		return string(b), true
	case "[rivet:incident_description]":
		b, _ := json.Marshal(map[string]any{
			"summary":      "Проверка наблюдения не проходит (fake-llm)",
			"cause":        "последнее изменение сломало проверку (fake-llm)",
			"reproduction": []string{"выполнить проверку наблюдения окружения"},
			"bug": map[string]any{"title": "Исправить сбой проверки наблюдения",
				"description": "Проверка наблюдения окружения завершилась ошибкой (fake-llm).",
				"criteria":    []string{"Проверка наблюдения проходит"}},
		})
		return string(b), true
	}
	return "", false
}
