package llm

import (
	"encoding/json"
	"testing"
)

// Консоль читает models у каждого пресета как массив: null у пресета без
// своего списка моделей ронял вкладку подключений (e2e add-native-agent).
func TestPresetsModelsNeverNull(t *testing.T) {
	raw, err := json.Marshal(Presets())
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("пресетов нет")
	}
	for _, p := range got {
		m, ok := p["models"]
		if !ok || string(m) == "null" || m[0] != '[' {
			t.Fatalf("пресет %s: models = %s, ждали массив", p["id"], m)
		}
	}
	if p, ok := PresetByID("openai"); !ok || p.Models == nil {
		t.Fatalf("PresetByID отдаёт nil вместо пустого списка моделей: %+v", p)
	}
}

// Модели пресетов проходят проверку каталога: предел ответа (он же
// предельный выход модели) меньше окна контекста, иначе подключение из
// пресета нельзя было бы сохранить правкой списка.
func TestPresetModelsFitWindow(t *testing.T) {
	for _, p := range Presets() {
		for _, m := range p.Models {
			if m.ContextWindow > 0 && m.MaxOutputTokens >= m.ContextWindow {
				t.Errorf("пресет %s, модель %s: предел ответа %d не меньше окна %d", p.ID, m.ID, m.MaxOutputTokens, m.ContextWindow)
			}
		}
	}
}
