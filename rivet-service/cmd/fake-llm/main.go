// fake-llm — OpenAI-совместимый провайдер для e2e-стенда: список моделей
// и chat completions с готовым планом декомпозиции. Ключ «fake-key»
// принимается, любой другой отклоняется 401 (сценарий «неверный ключ»).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func main() {
	addr := flag.String("addr", ":8283", "адрес HTTP")
	flag.Parse()
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fake-key" {
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
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var in struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &in)
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
		out := map[string]any{"object": "chat.completion", "model": in.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": string(content)}}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	for _, p := range []string{"/models", "/v1/models"} {
		mux.HandleFunc("GET "+p, models)
	}
	for _, p := range []string{"/chat/completions", "/v1/chat/completions"} {
		mux.HandleFunc("POST "+p, complete)
	}
	fmt.Println("fake-llm:", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
