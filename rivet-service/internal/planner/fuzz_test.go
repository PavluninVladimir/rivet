package planner

import (
	"encoding/json"
	"testing"
)

// Фаззинг разбора ответа модели (спека epic-decomposition: некорректный план
// не принимается; здесь — что он ещё и не роняет rivetd).

// FuzzParsePlan: любой текст ответа модели разбирается без паники; план,
// прошедший Validate, обязан проходить топосортировку.
func FuzzParsePlan(f *testing.F) {
	f.Add("Вот план:\n```json\n[{\"title\":\"A\",\"criteria\":[\"x\"],\"deps\":[]}]\n```")
	f.Add(`[{"title":"A","criteria":["x"]},{"title":"B","criteria":["y"],"deps":[0]}]`)
	f.Add(`[{"title":"A","criteria":["x"],"deps":[1]},{"title":"B","criteria":["y"],"deps":[0]}]`)
	f.Add("текст без массива")
	f.Add("[[[]]]")
	f.Add(`[{"deps":[-1]}]`)
	f.Fuzz(func(t *testing.T, text string) {
		plan, err := parsePlan(text)
		if err != nil {
			return
		}
		if err := Validate(plan); err != nil {
			return
		}
		order, err := topoOrder(plan)
		if err != nil {
			t.Fatalf("Validate принял план, topoOrder отклонил: %v", err)
		}
		if len(order) != len(plan) {
			t.Fatalf("топосортировка потеряла задачи: %d из %d", len(order), len(plan))
		}
	})
}

// FuzzValidatePlan: произвольный план (валидный JSON-массив задач) не роняет
// Validate; принятый план даёт в topoOrder перестановку всех индексов.
func FuzzValidatePlan(f *testing.F) {
	f.Add([]byte(`[{"title":"A","criteria":["c"]},{"title":"B","criteria":["c"],"deps":[0]}]`))
	f.Add([]byte(`[{"title":"A","criteria":["c"],"deps":[0]}]`))
	f.Add([]byte(`[{"title":"A","criteria":["c"],"deps":[9]}]`))
	f.Add([]byte(`[{"title":"A","criteria":["c"],"deps":[-2]}]`))
	f.Add([]byte(`[{"title":"A","criteria":["c"]},{"title":"B","criteria":["c"],"deps":[0,0]}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var plan []PlannedTask
		if err := json.Unmarshal(data, &plan); err != nil {
			return
		}
		if err := Validate(plan); err != nil {
			return
		}
		// Принятый план обязан материализоваться: дубль зависимости упал бы
		// на первичном ключе task_deps.
		for _, task := range plan {
			seen := map[int]bool{}
			for _, d := range task.Deps {
				if seen[d] {
					t.Fatalf("Validate принял план с дублем зависимости: %+v", task.Deps)
				}
				seen[d] = true
			}
		}
		order, err := topoOrder(plan)
		if err != nil {
			t.Fatalf("Validate принял план, topoOrder отклонил: %v", err)
		}
		if len(order) != len(plan) {
			t.Fatalf("topoOrder потерял задачи: %d из %d", len(order), len(plan))
		}
		seen := make([]bool, len(plan))
		for _, i := range order {
			if i < 0 || i >= len(plan) || seen[i] {
				t.Fatalf("topoOrder вернул не перестановку: %v", order)
			}
			seen[i] = true
		}
	})
}
