package runner

import (
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"strings"
	"testing"
)

func TestParseCriteria(t *testing.T) {
	text := "CRITERION 1: FAIL старое\n CRITERION 1: FAIL отступ\nCRITERION 2: FAIL нет теста\r\nCRITERION 1: OK\nCRITERION 3: OK\nCRITERION 0: FAIL x\nCRITERION 9999999999999999999999: OK\nCRITERION 2: UNKNOWN"
	got := parseCriteria(text, 2)
	if len(got) != 2 || !got[0].Ok || got[0].Index != 1 || got[0].Note != "" || got[1].Ok || got[1].Note != "нет теста" {
		t.Fatalf("отчёт: %+v", got)
	}
	if len(parseCriteria("CRITERION 1: OK", 0)) != 0 {
		t.Fatal("нет критериев")
	}
}
func TestReviewPromptNumbersCriteria(t *testing.T) {
	p := reviewPrompt(&pb.Assignment{Criteria: []string{"первый", "второй"}})
	for _, s := range []string{"1. первый", "2. второй", "CRITERION <номер>: OK", "CRITERION <номер>: FAIL"} {
		if !strings.Contains(p, s) {
			t.Fatalf("в промпте нет %q", s)
		}
	}
}
func FuzzParseCriteria(f *testing.F) {
	f.Add("CRITERION 1: OK\nCRITERION 2: FAIL ошибка")
	f.Add("CRITERION 9223372036854775808: OK")
	f.Fuzz(func(t *testing.T, s string) {
		last := int32(0)
		for _, c := range parseCriteria(s, 10) {
			if c.Index <= last || c.Index > 10 || len([]rune(c.Note)) > 1000 {
				t.Fatalf("невалидный результат %+v", c)
			}
			last = c.Index
		}
	})
}
