package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Назначение агенту Rivet: модель стадии и ключ из endpoint, ключ
// маскируется в транскрипте, в том числе разрезанный границей чанков.
func TestForAssignmentAndMask(t *testing.T) {
	cfg := Config{Adapter: AdapterNative, Model: "default"}
	as := &pb.Assignment{Model: "gpt-fast", SecretsIncluded: true,
		Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: "https://r/v1", Model: "gpt-fast", Key: "sk-secret-1234567890"}}
	c := cfg.forAssignment(as)
	if c.Model != "gpt-fast" || c.Endpoint == nil || len(c.ExtraEnv) != 0 {
		t.Fatalf("конфигурация: %+v", c)
	}
	if len(c.SecretValues) != 1 {
		t.Fatalf("секреты: %v", c.SecretValues)
	}
	var got string
	sink := maskSecrets(c.SecretValues, func(b []byte) { got += string(b) })
	sink([]byte("key=sk-secret-1234567890 url=https://r/v1 ab"))
	if got != "key=*** url=https://r/v1 ab" {
		t.Fatalf("маскирование: %q", got)
	}
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
	// Без endpoint маскировать нечего.
	if c3 := cfg.forAssignment(&pb.Assignment{Model: "m"}); c3.Endpoint != nil || len(c3.SecretValues) != 0 {
		t.Fatalf("без endpoint: %+v", c3)
	}
}

// Заголовки подключения: ключ в заголовке маскируется, короткие и
// заведомо несекретные значения остаются в транскрипте как есть.
func TestHeaderSecretsMasking(t *testing.T) {
	as := &pb.Assignment{Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: "https://r/v1", Model: "m",
		Headers: map[string]string{"X-Api-Key": "hdr-secret-123456", "X-Org": "acme", "X-Flag": "1",
			"HTTP-Referer": "https://rivet.example/console"}}}
	c := Config{Adapter: AdapterNative}.forAssignment(as)
	m := newSecretMasker(c.SecretValues, func([]byte) {})
	got := m.maskString("hdr-secret-123456 org=acme flag=1 ref=https://rivet.example/console")
	if got != "*** org=acme flag=1 ref=https://rivet.example/console" {
		t.Fatalf("маскирование заголовков: %q", got)
	}
}

// Итог стадии (ошибка агента или провайдера) маскируется: эхо ключа в
// тексте ошибки не уходит в detail.
func TestStageDetailMasked(t *testing.T) {
	base, _ := gitRemote(t)
	const key = "sk-echo-1234567890"
	a := &agent{cfg: Config{Adapter: AdapterNative, Workdir: t.TempDir(), GitBase: "file://" + base + "/", SecretValues: []string{key}},
		adapter: errAdapter{err: errors.New("HTTP 401: invalid key " + key)}, contexts: newContextHub(), cancel: map[string]*stageCancel{}}
	as := &pb.Assignment{TaskId: "t1", SessionId: "s1", TaskNum: 1, Stage: pb.StageResult_CODING, Repo: "repo",
		Branch: "feature/t1", BaseBranch: "main", Title: "x"}
	var res *pb.StageResult
	a.executeStage(context.Background(), as, func(m *pb.RunnerMsg) {
		if r := m.GetStageResult(); r != nil {
			res = r
		}
	})
	if res == nil || res.Ok || strings.Contains(res.Detail, key) || !strings.Contains(res.Detail, "invalid key ***") {
		t.Fatalf("итог: %+v", res)
	}
}

// errAdapter — агент, который падает с заданной ошибкой.
type errAdapter struct{ err error }

func (e errAdapter) Run(context.Context, string, string, runSink) (agentRun, error) {
	return agentRun{}, e.err
}
