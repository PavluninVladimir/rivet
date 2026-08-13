package redact

import (
	"strings"
	"testing"
)

// Сценарии спеки team-visibility «Секрет в транскрипте»: каждое правило
// ловит свой формат, совпавший фрагмент заменяется маской.
func TestStringMasksKnownSecrets(t *testing.T) {
	cases := []struct {
		name, in, gone string
	}{
		{"github classic", "push с ghp_Abcdefghij0123456789 ok", "ghp_Abcdefghij0123456789"},
		{"github fine-grained", "PAT github_pat_11ABCDEF0123456789_tail", "github_pat_11ABCDEF0123456789_tail"},
		{"anthropic/openai", "key sk-ant-api03-0123456789abcdef", "sk-ant-api03-0123456789abcdef"},
		{"slack", "bot xoxb-1234567890-abcdef", "xoxb-1234567890-abcdef"},
		{"aws", "AKIAIOSFODNN7EXAMPLE used", "AKIAIOSFODNN7EXAMPLE"},
		{"rivet pat", "token rvt_0123456789abcdef_x", "rvt_0123456789abcdef_x"},
		{"rivet session", "cookie rvs_0123456789abcdef_x", "rvs_0123456789abcdef_x"},
		{"bearer", "Authorization: Bearer eyJhbGciOi.payload.sig", "eyJhbGciOi.payload.sig"},
		{"password kv", "password=SuperSecret1", "SuperSecret1"},
		{"api key colon", `"api_key": "abcd1234efgh"`, "abcd1234efgh"},
		{"secret env", "export DB_SECRET=topsecretvalue", "topsecretvalue"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := String(c.in)
			if strings.Contains(got, c.gone) {
				t.Fatalf("секрет не замаскирован: %q -> %q", c.in, got)
			}
			if !strings.Contains(got, mask) {
				t.Fatalf("нет маски в результате: %q -> %q", c.in, got)
			}
		})
	}
}

func TestStringMasksPEMBlock(t *testing.T) {
	in := "до\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nMIIEowIBAAKCAQEB\n-----END RSA PRIVATE KEY-----\nпосле"
	got := String(in)
	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Fatalf("тело PEM не замаскировано: %q", got)
	}
	if !strings.Contains(got, "до") || !strings.Contains(got, "после") {
		t.Fatalf("текст вокруг блока потерян: %q", got)
	}
}

// PEM без закрывающего маркера (обрыв вывода) тоже не должен утечь.
func TestStringMasksUnterminatedPEM(t *testing.T) {
	got := String("x\n-----BEGIN PRIVATE KEY-----\nMIIEowIBAAKCAQEA")
	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Fatalf("тело оборванного PEM не замаскировано: %q", got)
	}
}

// Обычный вывод агента (сборка, тесты, git) проходит без изменений.
func TestStringNoFalsePositives(t *testing.T) {
	cases := []string{
		"go build ./... ok",
		"=== RUN TestParseUsage --- PASS",
		"git push -u origin agent/task-42",
		"проверка: lint прошла за 3s",
		"tokens_in=1200 tokens_out=340", // usage-цифры — не секрет
		"skip test: short mode",
	}
	for _, in := range cases {
		if got := String(in); got != in {
			t.Fatalf("ложное срабатывание: %q -> %q", in, got)
		}
	}
}

// Секрет, разрезанный между чанками внутри одной строки, маскируется:
// строка публикуется только после разделителя.
func TestStreamMasksSecretSplitAcrossChunks(t *testing.T) {
	var st Stream
	out := string(st.Feed([]byte("token ghp_Abcdefgh")))
	out += string(st.Feed([]byte("ij0123456789 done\nnext")))
	out += string(st.Flush())
	if strings.Contains(out, "ghp_Abcdefghij0123456789") {
		t.Fatalf("секрет утёк через границу чанков: %q", out)
	}
	if !strings.Contains(out, "next") {
		t.Fatalf("хвост после Flush потерян: %q", out)
	}
}

// \r — тоже разделитель: прогресс-бары не задерживают публикацию.
func TestStreamCarriageReturnSeparator(t *testing.T) {
	var st Stream
	out := string(st.Feed([]byte("progress 10%\rpassword=hunter42\rprogress 90%")))
	if !strings.Contains(out, "progress 10%\r") {
		t.Fatalf("строка до \\r не опубликована: %q", out)
	}
	if strings.Contains(out, "hunter42") {
		t.Fatalf("секрет в \\r-строке утёк: %q", out)
	}
}

// Хвост длиннее лимита сбрасывается принудительно, буфер не растёт бесконечно;
// последние keepTail байт удерживаются для стыковки со следующим чанком.
func TestStreamLongLineFlushed(t *testing.T) {
	var st Stream
	chunk := strings.Repeat("a", maxTail+100) // без разделителей
	out := st.Feed([]byte(chunk))
	if len(out) == 0 {
		t.Fatal("длинная строка без разделителей не сброшена")
	}
	if len(st.tail) != keepTail {
		t.Fatalf("хвост должен удержать %d байт, осталось %d", keepTail, len(st.tail))
	}
}

// Секрет, разрезанный границей принудительного сброса, не утекает в live:
// начало секрета удерживается в keepTail-окне до добора.
func TestStreamSecretAtForcedFlushBoundary(t *testing.T) {
	var st Stream
	secret := "ghp_BoundaryLeak0123456789"
	head, rest := secret[:10], secret[10:]
	// Секрет начинается прямо перед границей сброса (после пробела, как в
	// реальном выводе): начало обязано удержаться в keepTail-окне.
	out := string(st.Feed([]byte(strings.Repeat("a", maxTail+1) + " " + head)))
	out += string(st.Feed([]byte(rest + "\n")))
	out += string(st.Flush())
	if strings.Contains(out, secret) {
		t.Fatal("секрет утёк через границу принудительного сброса")
	}
}

// Тело PEM-блока, идущего через несколько Feed, маскируется построчно.
func TestStreamPEMAcrossFeeds(t *testing.T) {
	var st Stream
	out := string(st.Feed([]byte("-----BEGIN PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n")))
	out += string(st.Feed([]byte("MIIEowIBAAKCAQEB\n-----END PRIVATE KEY-----\nдальше\n")))
	if strings.Contains(out, "MIIEowIBAAKCAQEA") || strings.Contains(out, "MIIEowIBAAKCAQEB") {
		t.Fatalf("тело PEM утекло в поток: %q", out)
	}
	if !strings.Contains(out, "дальше") {
		t.Fatalf("вывод после блока потерян: %q", out)
	}
}

func TestStreamFlushEmpty(t *testing.T) {
	var st Stream
	if out := st.Flush(); len(out) != 0 {
		t.Fatalf("пустой Flush вернул данные: %q", out)
	}
}
