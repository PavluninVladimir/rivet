package architecture

import (
	"strings"
	"testing"
)

func TestAssessmentRejectsInvalidDecisionsAndHiddenCommands(t *testing.T) {
	valid := `{"decision":"no_change","summary":"existing boundary is sufficient","impact":[],"constraints":[],"documentation":[],"risks":[]}`
	if _, err := ParseAssessment(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(valid, `"no_change"`, `"merge_now"`, 1),
		strings.Replace(valid, `"risks":[]`, `"risks":[],"command":"deploy"`, 1),
		strings.Replace(valid, `"summary":"existing boundary is sufficient"`, `"summary":"`+strings.Repeat("x", 2001)+`"`, 1),
		`{"decision":"no_change","summary":"missing required fields"}`, valid + ` {}`,
		strings.Replace(valid, `"risks":[]`, `"risks":null`, 1),
	} {
		if _, err := ParseAssessment(raw); err == nil {
			t.Fatalf("invalid result accepted: %.90s", raw)
		}
	}
	if err := (Assessment{Decision: "no_change", Summary: "nil arrays"}).Validate(); err == nil {
		t.Fatal("nil arrays passed storage validation")
	}
}
