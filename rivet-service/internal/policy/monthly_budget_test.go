package policy

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// Плановый бюджет месяца (add-project-lead-view, задача 1.4): больше нуля,
// переносится в действующую политику, не меняет хэши прежних версий.
func TestMonthlyBudgetUSD(t *testing.T) {
	for _, bad := range []float64{0, -1, math.Inf(1), math.NaN()} {
		v := bad
		err := Overrides{MonthlyBudgetUSD: &v}.Validate()
		var pe *ProcessError
		if !errors.As(err, &pe) || pe.Field != "monthly_budget_usd" {
			t.Fatalf("бюджет %v: %v", bad, err)
		}
	}
	v := 150.5
	if err := (Overrides{MonthlyBudgetUSD: &v}).Validate(); err != nil {
		t.Fatal(err)
	}
	eff := Effective(Defaults(), Overrides{MonthlyBudgetUSD: &v})
	if eff.MonthlyBudgetUSD == nil || *eff.MonthlyBudgetUSD != 150.5 {
		t.Fatalf("действующая политика: %v", eff.MonthlyBudgetUSD)
	}
	v = 200
	if *eff.MonthlyBudgetUSD != 150.5 {
		t.Fatal("действующая политика держит копию, а не ссылку на переопределение")
	}
	raw, _ := json.Marshal(Defaults())
	if strings.Contains(string(raw), "monthly_budget_usd") {
		t.Fatal("без бюджета поле не попадает в документ: хэши прежних версий не меняются")
	}
	if Effective(Defaults(), Overrides{}).MonthlyBudgetUSD != nil {
		t.Fatal("без переопределения бюджета нет")
	}
}
