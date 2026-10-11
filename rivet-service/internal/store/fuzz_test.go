package store

import (
	"encoding/json"
	"testing"
)

// FuzzValidateDAG: произвольные карты зависимостей (планы задач из API) не
// роняют проверку ацикличности, и её вердикт детерминирован.
func FuzzValidateDAG(f *testing.F) {
	f.Add([]byte(`{"a":[],"b":["a"],"c":["a","b"]}`))
	f.Add([]byte(`{"a":["c"],"b":["a"],"c":["b"]}`))
	f.Add([]byte(`{"a":["zzz"]}`))
	f.Add([]byte(`{"":[""]}`))
	f.Add([]byte(`{"a":[],"b":["a","a"]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var deps map[string][]string
		if err := json.Unmarshal(data, &deps); err != nil {
			return
		}
		err1 := ValidateDAG(deps)
		err2 := ValidateDAG(deps)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("ValidateDAG недетерминирован: %v vs %v", err1, err2)
		}
	})
}
