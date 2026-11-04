package ops

import (
	"testing"
	"time"
)

// FuzzOpsParsers — разбор ответов модели и отпечаток не паникуют на
// произвольном вводе (ответ модели и вывод программ — недоверенные данные).
func FuzzOpsParsers(f *testing.F) {
	f.Add(`{"new":[{"task":7,"text":"a"}],"fixes":[],"other":[]}`)
	f.Add(`{"summary":"s","cause":"c","reproduction":["x"],"bug":{"title":"t","description":"d","criteria":["k"]}}`)
	f.Add(`{"cause":"c","tasks":[7],"recommendation":"r"}`)
	f.Add("```json\n{\"new\":null}\n```")
	comp := []MergedTask{{TaskID: "a", Num: 7, Title: "T", Kind: "work", MergedAt: time.Now()}, {TaskID: "b", Num: 9, Title: "B", Kind: "bug", IncidentID: "i"}}
	f.Fuzz(func(t *testing.T, raw string) {
		if n, _, err := ParseNotes(raw, comp); err == nil {
			_ = RenderNotes(n, comp, "e", "v")
		}
		if d, err := ParseDescription(raw); err == nil {
			_, _, _ = BugTask(d, "e", raw)
		}
		if a, err := ParseAnalysis(raw, comp); err == nil {
			_ = ReleaseIncident(&a, "e", "v", "s", raw)
		}
		_ = ErrorLine(raw)
		_ = Fingerprint("e", "monitor", raw)
	})
}
