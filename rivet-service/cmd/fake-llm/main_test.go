package main

import (
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/ops"
)

// Ответы по маркерам проходят разбор control plane, а пункт не из состава
// в release notes отбрасывается.
func TestOpsAnswers(t *testing.T) {
	comp := []ops.MergedTask{
		{TaskID: "a", Num: 7, Title: "Импорт контактов", Kind: "work", MergedAt: time.Now()},
		{TaskID: "b", Num: 9, Title: "Пагинация падает", Kind: "bug", IncidentID: "i", MergedAt: time.Now()},
	}
	raw, ok := opsAnswer(ops.ReleaseNotesPrompt(ops.ReleaseNotesInput{Env: "Демо", Version: "abc", Composition: comp}))
	if !ok {
		t.Fatal("маркер release_notes не распознан")
	}
	notes, dropped, err := ops.ParseNotes(raw, comp)
	if err != nil || dropped != 1 || len(notes.New) != 1 || len(notes.Fixes) != 1 {
		t.Fatalf("release notes: %+v dropped=%d err=%v raw=%s", notes, dropped, err, raw)
	}

	raw, ok = opsAnswer(ops.DeployAnalysisPrompt(ops.DeployAnalysisInput{Env: "Демо", Version: "abc", Stage: "verify", LogTail: "error", Composition: comp}))
	if !ok {
		t.Fatal("маркер deploy_analysis не распознан")
	}
	if a, err := ops.ParseAnalysis(raw, comp); err != nil || len(a.Tasks) != 1 {
		t.Fatalf("разбор: %+v %v", a, err)
	}

	raw, ok = opsAnswer(ops.IncidentDescriptionPrompt(ops.IncidentInput{Env: "Демо", Source: "monitor", ErrorLine: "500", CheckOutput: "500"}))
	if !ok {
		t.Fatal("маркер incident_description не распознан")
	}
	if _, err := ops.ParseDescription(raw); err != nil {
		t.Fatalf("описание: %v (%s)", err, raw)
	}
	if _, ok := opsAnswer("# Epic: CRM"); ok {
		t.Fatal("декомпозиция принята за задание агента")
	}
}
