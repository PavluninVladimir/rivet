package runner

import (
	"strings"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Назначение с профилем агента (протокол v12): окружение и аргументы
// накладываются на конфигурацию адаптера, команда обёртки заменяется,
// секреты назначения маскируются в транскрипте.
func TestForAssignmentAndMask(t *testing.T) {
	cfg := Config{Adapter: AdapterWrap, AgentCmd: "old-cmd", Model: "default"}
	as := &pb.Assignment{
		Model: "gpt-fast", AgentEnv: map[string]string{"OPENAI_API_KEY": "sk-secret-1234567890", "OPENAI_BASE_URL": "https://r/v1", "SHORT": "ab"},
		AgentArgs: []string{"-m", "gpt-fast"}, AgentCommand: "codex exec", SecretsIncluded: true, AgentSecretNames: []string{"OPENAI_API_KEY"},
	}
	c := cfg.forAssignment(as)
	if c.Model != "gpt-fast" || c.AgentCmd != "codex exec" || len(c.ExtraArgs) != 2 {
		t.Fatalf("конфигурация: %+v", c)
	}
	if strings.Join(c.ExtraEnv, ";") != "OPENAI_API_KEY=sk-secret-1234567890;OPENAI_BASE_URL=https://r/v1;SHORT=ab" {
		t.Fatalf("окружение: %v", c.ExtraEnv)
	}
	// Маскируются только переменные, названные секретными.
	if len(c.SecretValues) != 1 {
		t.Fatalf("секреты: %v", c.SecretValues)
	}
	var got string
	sink := maskSecrets(c.SecretValues, func(b []byte) { got += string(b) })
	sink([]byte("key=sk-secret-1234567890 url=https://r/v1 ab"))
	if got != "key=*** url=https://r/v1 ab" {
		t.Fatalf("маскирование: %q", got)
	}
	// Секрет, разрезанный границей чанков, маскируется целиком.
	got = ""
	m := newSecretMasker(c.SecretValues, func(b []byte) { got += string(b) })
	m.feed([]byte("token sk-secret-12"))
	m.feed([]byte("34567890 done"))
	m.flush()
	if got != "token *** done" {
		t.Fatalf("маскирование через границу: %q", got)
	}
	if m.maskString("x sk-secret-1234567890 y") != "x *** y" {
		t.Fatal("maskString")
	}
	// Нативный адаптер команду обёртки не берёт; без секретов маскировать нечего.
	as.SecretsIncluded, as.AgentSecretNames = false, nil
	c2 := Config{Adapter: AdapterClaudeCode, AgentCmd: "x"}.forAssignment(as)
	if c2.AgentCmd != "x" || len(c2.SecretValues) != 0 {
		t.Fatalf("нативный адаптер: %+v", c2)
	}
	if strings.Join(c2.ExtraArgs, " ") != "-m gpt-fast" {
		t.Fatalf("аргументы: %v", c2.ExtraArgs)
	}
	// Обёртка без назначения профиля не меняется.
	c3 := cfg.forAssignment(&pb.Assignment{Model: "m"})
	if c3.AgentCmd != "old-cmd" || len(c3.ExtraEnv) != 0 {
		t.Fatalf("без профиля: %+v", c3)
	}
}
