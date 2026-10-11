package policy

import (
	"encoding/json"
	"testing"
)

// FuzzProcessDocument: произвольный документ либо отклоняется валидацией,
// либо разрешается в процесс, где каждый переход ведёт на существующий
// включённый шаг, escalate или done, а лимиты не меньше единицы.
func FuzzProcessDocument(f *testing.F) {
	seed, _ := json.Marshal(DefaultProcess())
	f.Add(string(seed))
	f.Add(`{"steps":[{"id":"code","kind":"code","participants":[{"agent":{}}]},{"id":"merge","kind":"merge"}]}`)
	f.Add(`{"steps":[{"id":"a","kind":"review","participants":[{"agent":{"kind":"codex","model":"gpt-5"}},{"agent":{}}],"mode":"sequential","require":"any","attempts":2,"on":{"changes":"escalate"}},{"id":"m","kind":"merge"},{"id":"d","kind":"deploy"}]}`)
	f.Add(`{"steps":[{"id":"x","kind":"code","enabled":false},{"id":"m","kind":"merge","on":{"ok":"x"}}]}`)
	f.Add(`{"steps":[{"id":"code","kind":"code","participants":[{"user":{"login":"v"}}]},{"id":"merge","kind":"merge"}]}`)
	f.Fuzz(func(t *testing.T, raw string) {
		var p Process
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return
		}
		if err := p.Validate(); err != nil {
			return
		}
		r := Resolve(p, Defaults())
		ids := map[string]bool{}
		for _, s := range r.Steps {
			ids[s.ID] = true
		}
		merges := 0
		for _, s := range r.Steps {
			if s.Attempts < 1 {
				t.Fatalf("лимит < 1 у шага %s", s.ID)
			}
			if s.Kind == StepMerge {
				merges++
			}
			for _, target := range []string{s.On.Ok, s.On.Changes, s.On.Fail} {
				if target == "" || target == TargetEscalate || target == TargetDone {
					continue
				}
				if !ids[target] {
					t.Fatalf("переход шага %s на несуществующий %q", s.ID, target)
				}
			}
			if s.On.Changes != "" && s.On.Changes != TargetEscalate && s.On.Changes != TargetDone {
				if cs, _ := r.Step(s.On.Changes); cs.Kind != StepCode {
					t.Fatalf("changes шага %s ведёт не на code: %s", s.ID, cs.Kind)
				}
			}
		}
		if merges != 1 {
			t.Fatalf("включённых merge: %d", merges)
		}
	})
}
