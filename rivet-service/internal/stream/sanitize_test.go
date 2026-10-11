package stream

import (
	"math"
	"testing"
)

func p[T any](v T) *T { return &v }

// Отчёт агента — внешний ввод: отрицательные и не-конечные значения не должны
// доходить до usage_records.
func TestNonNegative(t *testing.T) {
	if got := nonNegative(p(int64(5))); got == nil || *got != 5 {
		t.Fatalf("валидное значение потеряно: %v", got)
	}
	if got := nonNegative(p(0.0)); got == nil || *got != 0 {
		t.Fatalf("ноль — валидное известное значение: %v", got)
	}
	for name, v := range map[string]*float64{
		"nil":           nil,
		"отрицательное": p(-0.01),
		"NaN":           p(math.NaN()),
		"+Inf":          p(math.Inf(1)),
		"-Inf":          p(math.Inf(-1)),
	} {
		if nonNegative(v) != nil {
			t.Fatalf("%s должно давать nil", name)
		}
	}
	if nonNegative(p(int64(-1))) != nil {
		t.Fatal("отрицательные токены должны давать nil")
	}
}

// ctx_pct от произвольного gRPC-клиента: только 0–100.
func TestCtxPctBounds(t *testing.T) {
	if got := ctxPct(p(int32(60))); got == nil || *got != 60 {
		t.Fatalf("валидный ctx_pct потерян: %v", got)
	}
	for _, v := range []*int32{nil, p(int32(-1)), p(int32(101))} {
		if ctxPct(v) != nil {
			t.Fatalf("ctx_pct %v должен давать nil", v)
		}
	}
}
