package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/llm"
)

// Вызов, помеченный провайдером как обрезанный, не исполняется даже с
// валидным JSON; повтор хода — с лимитом не выше предела модели.
func TestLoopTruncatedCallNeverExecuted(t *testing.T) {
	half := llm.ChatResponse{Stop: llm.StopLength, ToolCalls: []llm.ToolCall{{ID: "x", Name: ToolWriteFile,
		Arguments: json.RawMessage(`{"path":"a.txt","content":"полови"}`), Truncated: true}}}
	var limits []int
	sc := &script{steps: []func(llm.ChatRequest) (llm.ChatResponse, error){
		func(req llm.ChatRequest) (llm.ChatResponse, error) {
			limits = append(limits, req.MaxTokens)
			return half, nil
		},
		func(req llm.ChatRequest) (llm.ChatResponse, error) {
			limits = append(limits, req.MaxTokens)
			return half, nil
		},
	}}
	rec := &recorder{}
	cfg, dir := newCfg(t, sc, rec)
	cfg.MaxOutputTokens, cfg.ModelMaxOutput = 8000, 12000
	if _, err := Run(context.Background(), cfg, "задача"); !errors.Is(err, ErrTruncatedCall) {
		t.Fatalf("обрезанный вызов: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("обрезанный вызов не должен исполняться")
	}
	if len(limits) != 2 || limits[1] != 12000 {
		t.Fatalf("повтор с лимитом не выше предела модели: %v", limits)
	}
	// Предел модели уже исчерпан: повтора нет, сразу неуспех.
	sc2 := &script{steps: []func(llm.ChatRequest) (llm.ChatResponse, error){reply(half)}}
	cfg.Client, cfg.MaxOutputTokens, cfg.ModelMaxOutput = sc2, 8000, 8000
	if _, err := Run(context.Background(), cfg, "задача"); !errors.Is(err, ErrTruncatedCall) || len(sc2.reqs) != 1 {
		t.Fatalf("без повтора: %v, запросов %d", err, len(sc2.reqs))
	}
}

// Отказ модели (refusal, фильтр контента): вызовы не исполняются,
// стадия неуспешна.
func TestLoopRefusalStopsSession(t *testing.T) {
	sc := &script{steps: []func(llm.ChatRequest) (llm.ChatResponse, error){
		reply(llm.ChatResponse{Stop: llm.StopRefused, Text: "не могу"}),
	}}
	rec := &recorder{}
	cfg, _ := newCfg(t, sc, rec)
	if _, err := Run(context.Background(), cfg, "задача"); !errors.Is(err, ErrRefused) {
		t.Fatalf("отказ: %v", err)
	}
}

// Маркер вердикта в первом же текстовом ответе завершает сессию без
// напоминания: маркер не теряется.
func TestLoopMarkerInFirstTextAnswer(t *testing.T) {
	sc := &script{steps: []func(llm.ChatRequest) (llm.ChatResponse, error){
		reply(llm.ChatResponse{Text: "Замечаний нет.\nVERDICT: APPROVED", Stop: llm.StopEnd}),
		reply(llm.ChatResponse{Text: "ещё текст", Stop: llm.StopEnd}),
	}}
	rec := &recorder{}
	cfg, _ := newCfg(t, sc, rec)
	res, err := Run(context.Background(), cfg, "review")
	if err != nil || !strings.HasSuffix(res.FinalText, "VERDICT: APPROVED") || len(sc.reqs) != 1 {
		t.Fatalf("итог по первому ответу: %v %+v, запросов %d", err, res, len(sc.reqs))
	}
	if strings.Contains(rec.notes(), "напоминание") {
		t.Fatal("напоминание не нужно")
	}
}

// Шаг run подписан длительностью и кодом выхода.
func TestRunStepHasDuration(t *testing.T) {
	sc := &script{steps: []func(llm.ChatRequest) (llm.ChatResponse, error){
		reply(call(ToolRun, map[string]any{"command": "exit 3"})),
		reply(call(ToolFinish, map[string]any{"verdict": "ok", "summary": "ок"})),
	}}
	rec := &recorder{}
	cfg, _ := newCfg(t, sc, rec)
	if _, err := Run(context.Background(), cfg, "задача"); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^run exit 3 · [0-9.]+m?s, код 3$`)
	for _, e := range rec.events {
		if e.Tool == ToolRun {
			if !re.MatchString(e.Text) || e.OK {
				t.Fatalf("подпись шага run: %q ok=%v", e.Text, e.OK)
			}
			return
		}
	}
	t.Fatal("нет шага run")
}

// Схемы инструментов: required — всегда массив (null OpenAI отвергает).
func TestToolSchemasRequiredArray(t *testing.T) {
	for _, spec := range ToolSpecs() {
		var schema struct {
			Required json.RawMessage `json:"required"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(schema.Required), "[") {
			t.Errorf("%s: required = %s", spec.Name, schema.Required)
		}
	}
}
