package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func opsInt(n int64) *int64 { return &n }

func TestOpsCostRequiresCompleteKnownUsage(t *testing.T) {
	base := domain.ModelEntry{ID: "m", InputPrice: opsInt(6000000), OutputPrice: opsInt(18000000)}
	known := llm.Usage{InputTokens: opsInt(100), OutputTokens: opsInt(20)}
	cases := []struct {
		name  string
		usage llm.Usage
		price domain.ModelEntry
		want  *float64
	}{
		{"known", known, base, opsFloat(.000960)},
		{"zero output is known", llm.Usage{InputTokens: opsInt(100), OutputTokens: opsInt(0)}, base, opsFloat(.000600)},
		{"missing output", llm.Usage{InputTokens: opsInt(100)}, base, nil},
		{"missing input price", known, domain.ModelEntry{OutputPrice: base.OutputPrice}, nil},
		{"negative rate", known, domain.ModelEntry{InputPrice: opsInt(-1), OutputPrice: base.OutputPrice}, nil},
		{"negative tokens", llm.Usage{InputTokens: opsInt(-1), OutputTokens: opsInt(0)}, base, nil},
		{"invisible retry", llm.Usage{InputTokens: known.InputTokens, OutputTokens: known.OutputTokens, CostIncomplete: true}, base, nil},
		{"explicit free rate", known, domain.ModelEntry{InputPrice: opsInt(0), OutputPrice: opsInt(0)}, opsFloat(0)},
		{"unknown cache split", known, domain.ModelEntry{InputPrice: base.InputPrice, OutputPrice: base.OutputPrice, CachedInputPrice: opsInt(3000000)}, nil},
		{"cache same price needs no split", known, domain.ModelEntry{InputPrice: base.InputPrice, OutputPrice: base.OutputPrice, CachedInputPrice: base.InputPrice}, opsFloat(.000960)},
		{"known cache split", llm.Usage{InputTokens: opsInt(100), OutputTokens: opsInt(20), CachedInputTokens: opsInt(40)}, domain.ModelEntry{InputPrice: base.InputPrice, OutputPrice: base.OutputPrice, CachedInputPrice: opsInt(3000000)}, opsFloat(.000840)},
		{"unknown cache write rate", llm.Usage{InputTokens: opsInt(100), OutputTokens: opsInt(20), CacheWriteTokens: opsInt(10)}, base, nil},
		{"invalid cached count", llm.Usage{InputTokens: opsInt(100), OutputTokens: opsInt(20), CachedInputTokens: opsInt(101)}, base, nil},
		{"numeric column overflow", llm.Usage{InputTokens: opsInt(math.MaxInt64), OutputTokens: opsInt(20)}, domain.ModelEntry{InputPrice: opsInt(math.MaxInt64), OutputPrice: base.OutputPrice}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertOpsCost(t, opsCost(tc.usage, &tc.price), tc.want) })
	}
	price := opsPriceSnapshot([]domain.ModelEntry{base}, "m")
	*base.InputPrice = 1
	assertOpsCost(t, opsCost(known, price), opsFloat(.000960))
	if opsPriceSnapshot([]domain.ModelEntry{base, base}, "m") != nil || opsPriceSnapshot([]domain.ModelEntry{base}, "other") != nil {
		t.Fatal("missing/ambiguous model must not choose a tariff")
	}
}

func opsFloat(n float64) *float64 { return &n }
func assertOpsCost(t *testing.T, got, want *float64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("unknown cost became %g", *got)
		}
		return
	}
	if got == nil || math.Abs(*got-*want) > 1e-10 {
		t.Fatalf("cost got %v want %g", got, *want)
	}
}

// Реальный HTTP provider -> completeBounded -> PostgreSQL. Цена меняется
// во время запроса, затем подключение удаляется: исторический cost не дрейфует.
func TestOpsCostProviderLedgerSnapshotAndUnknown(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	u := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "Ops cost", "e2e/demo", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{St: st}
	cases := []struct {
		name        string
		usage       string
		finish      string
		missingRate bool
		freeRates   bool
		changeRate  bool
		retry       bool
		want        *float64
		wantOutput  *int64
	}{
		{name: "known and rate changes", usage: `"usage":{"prompt_tokens":100,"completion_tokens":20}`, finish: "stop", changeRate: true, want: opsFloat(.000960), wantOutput: opsInt(20)},
		{name: "explicit zero output", usage: `"usage":{"prompt_tokens":100,"completion_tokens":0}`, finish: "stop", want: opsFloat(.000600), wantOutput: opsInt(0)},
		{name: "explicit zero cost", usage: `"usage":{"prompt_tokens":100,"completion_tokens":20}`, finish: "stop", freeRates: true, want: opsFloat(0), wantOutput: opsInt(20)},
		{name: "missing output", usage: `"usage":{"prompt_tokens":100}`, finish: "stop"},
		{name: "missing usage", usage: `"id":"no-usage"`, finish: "stop"},
		{name: "missing price", usage: `"usage":{"prompt_tokens":100,"completion_tokens":20}`, finish: "stop", missingRate: true, wantOutput: opsInt(20)},
		{name: "truncation still costs", usage: `"usage":{"prompt_tokens":100,"completion_tokens":20}`, finish: "length", want: opsFloat(.000960), wantOutput: opsInt(20)},
		{name: "hidden limit attempt", usage: `"usage":{"prompt_tokens":100,"completion_tokens":20}`, finish: "stop", retry: true, wantOutput: opsInt(20)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connection := fmt.Sprintf("ops-cost-%d", i)
			model := fmt.Sprintf("ops-model-%d", i)
			var requests atomic.Int32
			requestErrors := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestNumber := requests.Add(1)
				if tc.retry && requestNumber == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"message":"max_tokens valid range is [1, 8192]"}}`))
					return
				}
				if tc.changeRate {
					_, updateErr := st.SetConnectionModels(r.Context(), connection, []domain.ModelEntry{{ID: model, InputPrice: opsInt(99000000), OutputPrice: opsInt(99000000), Source: "manual"}}, u.ID, u.Login)
					requestErrors <- updateErr
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"done"},"finish_reason":%q}],%s}`, tc.finish, tc.usage)
			}))
			defer srv.Close()
			if _, _, err := st.UpsertConnection(ctx, store.ConnectionInput{ID: connection, Name: connection, Kind: "local", API: "openai", BaseURL: srv.URL}, nil, u.ID, u.Login); err != nil {
				t.Fatal(err)
			}
			price := domain.ModelEntry{ID: model, InputPrice: opsInt(6000000), OutputPrice: opsInt(18000000), Source: "manual"}
			if tc.missingRate {
				price.InputPrice = nil
			}
			if tc.freeRates {
				price.InputPrice, price.OutputPrice = opsInt(0), opsInt(0)
			}
			if _, err := st.SetConnectionModels(ctx, connection, []domain.ModelEntry{price}, u.ID, u.Login); err != nil {
				t.Fatal(err)
			}
			_, err := engine.completeBounded(ctx, opsAgent{role: domain.AgentRole{ID: domain.RoleArchitect}, connID: connection, model: model}, p.ID, "prompt", 16000)
			var requestErr error
			if tc.changeRate {
				requestErr = <-requestErrors
			}
			if (tc.finish == "length" && !errors.Is(err, llm.ErrTruncated)) || (tc.finish != "length" && err != nil) || requestErr != nil {
				t.Fatalf("call: %v price update: %v", err, requestErr)
			}
			if tc.retry && requests.Load() != 2 {
				t.Fatalf("retry fixture did not execute: %d", requests.Load())
			}
			var cost *float64
			var output *int64
			var role string
			if err := st.Pool.QueryRow(ctx, `SELECT cost_usd,tokens_out,role FROM usage_records WHERE project_id=$1 AND model=$2`, p.ID, model).Scan(&cost, &output, &role); err != nil {
				t.Fatal(err)
			}
			assertOpsCost(t, cost, tc.want)
			if role != domain.RoleArchitect || (output == nil) != (tc.wantOutput == nil) || (output != nil && *output != *tc.wantOutput) {
				t.Fatalf("usage presence/role changed: output=%v role=%s", output, role)
			}
			if err := st.DeleteConnection(ctx, connection, u.Login); err != nil {
				t.Fatal(err)
			}
			if err := st.Pool.QueryRow(ctx, `SELECT cost_usd FROM usage_records WHERE project_id=$1 AND model=$2`, p.ID, model).Scan(&cost); err != nil {
				t.Fatal(err)
			}
			assertOpsCost(t, cost, tc.want)
		})
	}
	engine.OpsComplete = func(context.Context, string, string, string) (string, error) { return "callback", nil }
	if _, err := engine.complete(ctx, opsAgent{role: domain.AgentRole{ID: domain.RoleSRE}, model: "callback-no-usage"}, p.ID, "prompt"); err != nil {
		t.Fatal(err)
	}
	var cost *float64
	if err := st.Pool.QueryRow(ctx, `SELECT cost_usd FROM usage_records WHERE project_id=$1 AND model='callback-no-usage'`, p.ID).Scan(&cost); err != nil || cost != nil {
		t.Fatalf("callback fabricated cost: %v %v", cost, err)
	}
}
