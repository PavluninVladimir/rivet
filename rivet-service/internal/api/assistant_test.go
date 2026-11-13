package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/assistant"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func TestAssistantAPIFakeModelRecoveryAndQuestion(t *testing.T) {
	for _, intent := range []string{"recover", "question"} {
		t.Run(intent, func(t *testing.T) {
			st, existing := testServer(t)
			_ = existing
			ctx := context.Background()
			u, err := st.CreateUser(ctx, "assistant-owner", "Owner", "pw-testpass", false)
			if err != nil {
				t.Fatal(err)
			}
			p, err := st.CreateProject(ctx, "contracts", "owner/contracts", nil, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			e, err := st.CreateEpic(ctx, p.ID, "Contracts", "goal")
			if err != nil {
				t.Fatal(err)
			}
			task, err := st.CreateTask(ctx, e.ID, store.NewTask{Title: "Extract contracts"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.Pool.Exec(ctx, `UPDATE tasks SET status='failed',step_id='code',block_reason='timeout',attempt_used=3 WHERE id=$1`, task.ID); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req map[string]json.RawMessage
				if er := json.NewDecoder(r.Body).Decode(&req); er != nil {
					t.Error(er)
				}
				if tools, ok := req["tools"]; ok && string(tools) != "null" && string(tools) != "[]" {
					t.Errorf("model received tools %s", tools)
				}
				d := assistant.Diagnosis{Summary: "Задача failed. Срок неизвестен. Running не подтверждает приёмку.", Uncertainties: []string{"нужна проверка результата"}, RefIDs: []string{"task:" + task.ID}, Intents: []assistant.Intent{{Type: "task.retry", TargetID: task.ID, EvidenceIDs: []string{"task:" + task.ID}}}}
				content, _ := json.Marshal(d)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "fake-1", "model": "fake-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": string(content)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 80, "total_tokens": 180}})
			}))
			defer provider.Close()
			if _, _, err = st.UpsertConnection(ctx, store.ConnectionInput{ID: "assistant-local", Name: "Fake local", Kind: "local", API: "openai", BaseURL: provider.URL}, nil, u.ID, u.Login); err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			if _, err = st.SetConnectionModels(ctx, "assistant-local", []domain.ModelEntry{{ID: "fake-model", Label: "Fake", InputPrice: &zero, OutputPrice: &zero, Source: "manual"}}, u.ID, u.Login); err != nil {
				t.Fatal(err)
			}
			cID, mID := "assistant-local", "fake-model"
			limits := assistant.DefaultLimits()
			limits.MaxInputTokens = 100000
			limits.DailyTokens = 1000000
			if _, err = st.SaveAssistantConfig(ctx, p.ID, u.ID, 0, assistant.Config{Enabled: true, ConnectionID: &cID, ModelID: &mID, Limits: limits}); err != nil {
				t.Fatal(err)
			}
			server := &Server{St: st, Policy: policy.Default()}
			srv := httptest.NewServer(server.Handler())
			defer srv.Close()
			cookie := loginSession(t, srv, "assistant-owner", "pw-testpass")
			post := func(key string, body any) (int, []byte) {
				b, _ := json.Marshal(body)
				req, _ := http.NewRequest("POST", srv.URL+"/api/v1/projects/"+p.ID+"/assistant/messages", bytes.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", key)
				req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
				res, er := http.DefaultClient.Do(req)
				if er != nil {
					t.Fatal(er)
				}
				defer res.Body.Close()
				raw, _ := io.ReadAll(res.Body)
				return res.StatusCode, raw
			}
			status, raw := post("message-1", map[string]any{"text": "восстанови", "intent": intent})
			if status != 202 {
				t.Fatalf("POST %d %s", status, raw)
			}
			var created struct {
				Run assistant.Run `json:"run"`
			}
			if err = json.Unmarshal(raw, &created); err != nil {
				t.Fatal(err)
			}
			status, raw = post("message-1", map[string]any{"text": "восстанови", "intent": intent})
			if status != 202 {
				t.Fatalf("replay %d %s", status, raw)
			}
			status, raw = post("message-1", map[string]any{"text": "other", "intent": intent})
			if status != 409 {
				t.Fatalf("body conflict %d %s", status, raw)
			}
			job, err := st.ClaimAssistantJob(ctx)
			if err != nil || job == nil {
				t.Fatalf("claim %v %v", job, err)
			}
			server.runAssistantJob(ctx, *job)
			run, actions, err := st.AssistantRun(ctx, p.ID, u.ID, created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "completed" {
				t.Fatalf("run status %s summary %v actions %+v", run.Status, run.Summary, actions)
			}
			got, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if intent == "recover" {
				if got.Status != domain.TaskQueued || len(actions) != 1 || actions[0].Status != "succeeded" {
					t.Fatalf("recovery %s %+v", got.Status, actions)
				}
			} else if got.Status != domain.TaskFailed || len(actions) != 0 {
				t.Fatalf("question mutated %s %+v", got.Status, actions)
			}
			if calls.Load() != 1 {
				t.Fatalf("provider calls %d", calls.Load())
			}
			// Replaying the intent, idle ticks or heartbeats cannot make a second call.
			if err = server.observeAssistantEvents(ctx); err != nil {
				t.Fatal(err)
			}
			if err = server.observeAssistantEvents(ctx); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("event noise called provider")
			}
			resp, body := call(t, "GET", srv.URL+"/api/v1/projects/"+p.ID+"/assistant", cookie, "", nil)
			if resp.StatusCode != 200 || !strings.Contains(string(body), `"capability":true`) {
				t.Fatalf("snapshot %d %s", resp.StatusCode, body)
			}
		})
	}
}
