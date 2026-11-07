package domain

import (
	"strings"
	"testing"
)

func TestRunnerIDProblem(t *testing.T) {
	for id, ok := range map[string]bool{
		"local-worker": true, "office.1": true, "сервер-1": true, strings.Repeat("я", MaxRunnerIDLen): true,
		"": false, "local worker": false, "w1\n": false, "w\t1": false, "\xff": false,
		strings.Repeat("r", MaxRunnerIDLen+1): false,
	} {
		if got := RunnerIDProblem(id) == ""; got != ok {
			t.Errorf("RunnerIDProblem(%q) допустим=%v, ждём %v", id, got, ok)
		}
	}
}
