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

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// recordSynthesis — прокси к fake-llm Аналитика, который запоминает
// задания синтеза.
func recordSynthesis(t *testing.T, upstream string) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &in)
		if len(in.Messages) > 0 && strings.HasPrefix(in.Messages[0].Content, discovery.MarkerSynthesis) {
			mu.Lock()
			prompts = append(prompts, in.Messages[0].Content)
			mu.Unlock()
		}
		resp, err := http.Post(upstream+r.URL.Path, "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

// Спека product-discovery «Анализ интервью Аналитиком»: на синтезе
// Аналитик получает сводку снимка архитектуры, если он корректен. При
// сломанном описании — последнюю корректную версию с пометкой, без
// корректной версии задание говорит, что описания нет.
func TestDiscoverySynthesisArchitecture(t *testing.T) {
	llmSrv := fakeAnalystLLM(t, nil)
	rec, prompts := recordSynthesis(t, llmSrv.URL)
	f := seedDiscovery(t, rec.URL)
	ctx := context.Background()
	base := f.srv.URL + "/api/v1/projects/" + f.project + "/discovery"

	resp, _ := call(t, "POST", base+"/interviews", f.user, "", map[string]any{"title": "Анна", "respondent_role": "Менеджер", "text": interviewText("Анна")})
	mustStatus(t, resp, http.StatusCreated, "интервью")

	analyse := func() string {
		t.Helper()
		resp, body := call(t, "POST", base+"/analyses", f.user, "", nil)
		mustStatus(t, resp, http.StatusAccepted, "анализ")
		var run apiRun
		_ = json.Unmarshal(body, &run)
		done := f.waitRun(t, run.ID, func(r apiRun) bool { return r.Status != store.RunRunning })
		if done.Status != store.RunReady {
			t.Fatalf("анализ: %+v", done)
		}
		got := prompts()
		if len(got) == 0 {
			t.Fatal("задания синтеза не было")
		}
		_, arch, _ := strings.Cut(got[len(got)-1], "## Архитектура\n")
		arch, _, _ = strings.Cut(arch, "## Epic'и проекта")
		return arch
	}

	// Снимка нет.
	if arch := analyse(); !strings.Contains(arch, "Корректного описания архитектуры нет.") {
		t.Fatalf("без снимка: %q", arch)
	}

	data, _ := json.Marshal(map[string]any{
		"system": map[string]string{"name": "CRM", "description": "учёт контактов\nи сделок"},
		"elements": []map[string]any{
			{"id": "crm", "kind": "system", "name": "CRM"},
			{"id": "api", "kind": "container", "name": "API контактов", "technology": "Node.js", "parent": "crm", "description": "REST"},
			{"id": "mail", "kind": "system", "name": "Почта", "external": true},
		},
		"hld": "# HLD\nКонтакты хранятся в JSON-файле.",
	})
	if _, _, err := f.st.SaveProjectArchitecture(ctx, f.project, store.ArchitectureSave{Status: "ok", SHA: "0123456789abcdef", Data: data}); err != nil {
		t.Fatal(err)
	}
	arch := analyse()
	for _, want := range []string{"Система: CRM. учёт контактов и сделок", "- container «API контактов», технология: Node.js, внутри «CRM»: REST",
		"- system «Почта», внешний", "Контакты хранятся в JSON-файле."} {
		if !strings.Contains(arch, want) {
			t.Fatalf("в задании синтеза нет %q:\n%s", want, arch)
		}
	}
	if strings.Contains(arch, "Корректного описания архитектуры нет") || strings.Contains(arch, "Последняя корректная версия") {
		t.Fatalf("корректный снимок с пометкой об ошибке:\n%s", arch)
	}

	// Описание сломали: последняя корректная версия с пометкой.
	if _, _, err := f.st.SaveProjectArchitecture(ctx, f.project, store.ArchitectureSave{Status: "invalid", SHA: "fedcba9876543210"}); err != nil {
		t.Fatal(err)
	}
	arch = analyse()
	if !strings.Contains(arch, "Последняя корректная версия описания (коммит 0123456)") || !strings.Contains(arch, "API контактов") {
		t.Fatalf("сломанное описание:\n%s", arch)
	}
}
