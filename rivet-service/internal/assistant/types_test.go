package assistant

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssistantDiagnosisEvidenceAndInjection(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	s := Snapshot{Refs: []EvidenceRef{{ID: "task:" + id, Kind: "task", ResourceID: id}}}
	d := Diagnosis{Summary: "HTTP жив, приёмка неизвестна", RefIDs: []string{"task:" + id}, Intents: []Intent{{Type: "task.retry", TargetID: id, EvidenceIDs: []string{"task:" + id}}}}
	raw, _ := json.Marshal(d)
	if _, err := ParseDiagnosis(string(raw), s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Diagnosis){func(d *Diagnosis) { d.RefIDs = []string{"task:foreign"} }, func(d *Diagnosis) { d.Intents[0].Type = "shell.exec" }, func(d *Diagnosis) { d.Intents[0].TargetID = "22222222-2222-2222-2222-222222222222" }, func(d *Diagnosis) { d.Intents[0].EvidenceIDs = nil }} {
		copy := Diagnosis{}
		_ = json.Unmarshal(raw, &copy)
		mutate(&copy)
		bad, _ := json.Marshal(copy)
		if _, err := ParseDiagnosis(string(bad), s); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if got := Redact("token=secret123 api_key=secret456 bearer abc123 ghp_12345678901234567890"); strings.Contains(got, "secret") || strings.Contains(got, "abc123") || strings.Contains(got, "ghp_") {
		t.Fatal(got)
	}
}
func TestAssistantFailureEpisodeStableAndLimits(t *testing.T) {
	a := FailureEpisode("task", "code", "timeout", "input-v1")
	if a != FailureEpisode("task", "code", "timeout", "input-v1") {
		t.Fatal("unstable")
	}
	if a == FailureEpisode("task", "code", "timeout", "input-v2") {
		t.Fatal("basis change ignored")
	}
	l := DefaultLimits()
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	l.MaxRunActions = 4
	if l.Validate() == nil {
		t.Fatal("unbounded actions")
	}
}
