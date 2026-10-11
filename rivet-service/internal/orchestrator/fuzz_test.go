package orchestrator

import (
	"strconv"
	"strings"
	"testing"
)

func TestPRNumber(t *testing.T) {
	n, err := prNumber("https://github.com/owner/repo/pull/42")
	if err != nil || n != 42 {
		t.Fatalf("want 42, got %d, %v", n, err)
	}
	for _, bad := range []string{"мусор без номера", "/-5", "/0", "/42abc", "/042", "/+42", ""} {
		if _, err := prNumber(bad); err == nil {
			t.Fatalf("%q должен давать ошибку", bad)
		}
	}
}

// FuzzPRNumber: URL PR приходит из webhook и БД — произвольная строка не должна
// ронять разбор; принятый номер положителен и канонически совпадает с хвостом URL.
func FuzzPRNumber(f *testing.F) {
	f.Add("https://github.com/owner/repo/pull/42")
	f.Add("")
	f.Add("без слэшей")
	f.Add("https://github.com/owner/repo/pull/")
	f.Add("/-5")
	f.Add("/42abc")
	f.Fuzz(func(t *testing.T, url string) {
		n, err := prNumber(url)
		if err != nil {
			return
		}
		if n <= 0 {
			t.Fatalf("prNumber принял неположительный номер %d из %q", n, url)
		}
		suffix := url[strings.LastIndexByte(url, '/')+1:]
		if strconv.Itoa(n) != suffix {
			t.Fatalf("номер %d не канонический хвост %q URL %q", n, suffix, url)
		}
	})
}
