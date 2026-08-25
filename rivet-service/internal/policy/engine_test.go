package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Стандартная политика: те же решения, что давали гейты до движка
// (спека access-policy «Точки принуждения»).
func TestEmbeddedStandardPolicy(t *testing.T) {
	e, err := NewEngine(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode() != ModeEmbedded {
		t.Fatalf("режим: %s", e.Mode())
	}
	ctx := context.Background()
	merge := func(auto, unknown bool, policyFiles, protected []string) Decision {
		d, err := e.Decide(ctx, PointMerge, map[string]any{
			"presets":       map[string]any{"auto_merge": auto},
			"files_unknown": unknown,
			"policy_files":  policyFiles,
			"protected":     protected,
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, tc := range []struct {
		name       string
		auto, unkn bool
		pol, prot  []string
		allow      bool
		reason     string
	}{
		{"пресет выключен", false, false, nil, nil, false, "auto_merge_off"},
		{"чистый PR", true, false, []string{}, []string{}, true, ""},
		{"файлы неизвестны", true, true, []string{}, []string{}, false, "files_unknown"},
		{"файл политики", true, false, []string{".rivet/policy.yaml"}, []string{}, false, "policy_file"},
		{"защищённый путь", true, false, []string{}, []string{"deploy/prod.yaml"}, false, "human_review_path"},
	} {
		d := merge(tc.auto, tc.unkn, tc.pol, tc.prot)
		if d.Allow != tc.allow || d.Reason != tc.reason {
			t.Fatalf("%s: %+v", tc.name, d)
		}
	}

	pub := func(auto bool) Decision {
		d, err := e.Decide(ctx, PointPublish, map[string]any{"presets": map[string]any{"auto_publish": auto}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := pub(true); !d.Allow {
		t.Fatalf("автопубликация разрешена: %+v", d)
	}
	if d := pub(false); d.Allow || d.Reason != "auto_publish_off" {
		t.Fatalf("автопубликация запрещена: %+v", d)
	}

	assign := func(instUsed, instBudget, prUsed, prBudget, epUsed, epBudget int64) Decision {
		d, err := e.Decide(ctx, PointAssign, map[string]any{
			"installation": map[string]any{"used": instUsed, "budget": instBudget},
			"project":      map[string]any{"used": prUsed, "budget": prBudget},
			"epic":         map[string]any{"used": epUsed, "budget": epBudget},
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := assign(0, 0, 0, 0, 0, 0); !d.Allow {
		t.Fatalf("без бюджетов назначение разрешено: %+v", d)
	}
	if d := assign(100, 100, 0, 0, 0, 0); d.Allow || d.Reason != "installation" {
		t.Fatalf("бюджет установки: %+v", d)
	}
	if d := assign(0, 1000, 100, 100, 0, 0); d.Allow || d.Reason != "project" {
		t.Fatalf("бюджет проекта: %+v", d)
	}
	if d := assign(0, 0, 99, 100, 100, 100); d.Allow || d.Reason != "epic" {
		t.Fatalf("бюджет Epic: %+v", d)
	}

	// Мутации: стандартная политика прав не сужает, но записывать политику
	// автоматике запрещает.
	d, err := e.Decide(ctx, PointMutation, map[string]any{
		"action": "task.merge", "actor": map[string]any{"kind": "user"}})
	if err != nil || !d.Allow {
		t.Fatalf("мутация человека: %v %+v", err, d)
	}
	d, err = e.Decide(ctx, PointMutation, map[string]any{
		"action": "policy.write", "actor": map[string]any{"kind": "runner"}})
	if err != nil || d.Allow || d.Reason != "automation_cannot_write_policy" {
		t.Fatalf("запись политики автоматикой: %v %+v", err, d)
	}
	if _, err := e.Decide(ctx, "unknown", nil); err == nil {
		t.Fatal("неизвестная точка должна давать ошибку")
	}
}

// Внешний движок: успешный ответ, пустой result, ошибка и таймаут —
// «решения нет», вызывающий трактует это как запрет.
func TestExternalEngine(t *testing.T) {
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "ok":
			var in struct {
				Input map[string]any `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Input == nil {
				http.Error(w, "нет input", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"allow": true, "reason": ""}})
		case "empty":
			_, _ = w.Write([]byte(`{}`))
		case "slow":
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	if _, err := NewEngine(Config{Mode: ModeExternal}); err == nil {
		t.Fatal("external без адреса не должен стартовать")
	}
	if _, err := NewEngine(Config{Mode: "opa-cloud"}); err == nil {
		t.Fatal("неизвестный режим не должен стартовать")
	}
	e, err := NewEngine(Config{Mode: ModeExternal, URL: srv.URL + "/", Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode() != ModeExternal {
		t.Fatalf("режим: %s", e.Mode())
	}
	ctx := context.Background()
	mode = "ok"
	if d, err := e.Decide(ctx, PointMerge, map[string]any{"presets": map[string]any{}}); err != nil || !d.Allow {
		t.Fatalf("успешный ответ: %v %+v", err, d)
	}
	if err := e.Health(ctx); err != nil {
		t.Fatalf("здоровье: %v", err)
	}
	mode = "empty"
	if _, err := e.Decide(ctx, PointMerge, nil); err == nil {
		t.Fatal("пустой result — не разрешение")
	}
	mode = "fail"
	if _, err := e.Decide(ctx, PointMerge, nil); err == nil {
		t.Fatal("ответ 500 — не разрешение")
	}
	if err := e.Health(ctx); err == nil {
		t.Fatal("недоступный движок должен быть виден в состоянии")
	}
	mode = "slow"
	if _, err := e.Decide(ctx, PointMerge, nil); err == nil {
		t.Fatal("таймаут — не разрешение")
	}
}
