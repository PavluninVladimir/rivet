package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// providerScript — OpenAI-совместимый провайдер со сценарием вызовов:
// n-й запрос получает n-й вызов инструмента. Запоминает тела запросов.
type providerScript struct {
	mu     sync.Mutex
	calls  []map[string]any // {"name":..., "args":...}
	bodies [][]byte
}

func (p *providerScript) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies = append(p.bodies, raw)
		i := len(p.bodies) - 1
		c := p.calls[min(i, len(p.calls)-1)]
		p.mu.Unlock()
		args, _ := json.Marshal(c["args"])
		resp := map[string]any{
			"model": "deepseek-v4-flash",
			"choices": []map[string]any{{"finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []map[string]any{{"id": fmt.Sprintf("c%d", i), "type": "function",
					"function": map[string]any{"name": c["name"], "arguments": string(args)}}},
			}}},
			"usage": map[string]any{"prompt_tokens": 2000, "completion_tokens": 100},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	dir := t.TempDir()
	nativeGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# CRM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nativeGit(t, dir, "add", "-A")
	nativeGit(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

// nativeGit — git в каталоге теста с явной identity (на CI глобального config нет).
func nativeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func nativeSink() (runSink, *[]*pb.AgentEvent, *strings.Builder) {
	var events []*pb.AgentEvent
	var mu sync.Mutex
	tr := &strings.Builder{}
	return runSink{
		transcript: func(b []byte) { mu.Lock(); tr.Write(b); mu.Unlock() },
		step:       func(ev *pb.AgentEvent) { mu.Lock(); events = append(events, ev); mu.Unlock() },
		session:    "s1",
		contexts:   newContextHub(),
		stage:      pb.StageResult_CODING,
	}, &events, tr
}

// Агент rivet на настоящем git-репозитории: инструменты правят рабочую
// копию, команда видит git, .git не тронут, шаги и usage доходят до стадии,
// итог разбирается прежними маркерами. Внешних программ агента нет.
func TestNativeAdapterOnGitRepo(t *testing.T) {
	dir := gitRepo(t)
	p := &providerScript{calls: []map[string]any{
		{"name": "list_dir", "args": map[string]any{"path": "."}},
		{"name": "write_file", "args": map[string]any{"path": "src/companies.js", "content": "export const companies = []\n"}},
		{"name": "write_file", "args": map[string]any{"path": ".git/hooks/pre-commit", "content": "exit 1"}},
		{"name": "run", "args": map[string]any{"command": "git status --porcelain"}},
		{"name": "finish", "args": map[string]any{"verdict": "ok", "summary": "Добавлен модуль компаний"}},
	}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	t.Setenv("RIVET_NATIVE_BASE_URL", srv.URL)
	t.Setenv("RIVET_NATIVE_CONNECTION", "deepseek")
	t.Setenv("RIVET_NATIVE_PRICE_IN", "0.27")
	t.Setenv("RIVET_NATIVE_PRICE_OUT", "1.1")

	// Ключ подключения — из секретов runner'а, не из окружения процесса.
	// -unsandboxed: на macOS песочницы нет, а команда run здесь нужна.
	cfg := Config{Agent: AgentRivet, Model: "deepseek-v4-flash", Workdir: t.TempDir(), Unsandboxed: true,
		Secrets: map[string]string{"RIVET_PROVIDER_KEY_DEEPSEEK": "sk-test"}}
	cfg.Adapter = AdapterNative
	if cfg.Adapter != AdapterNative || cfg.depth() != "full" || !cfg.contextChannel() {
		t.Fatalf("агент rivet: адаптер %s, глубина %s", cfg.Adapter, cfg.depth())
	}
	sink, events, tr := nativeSink()
	run, err := newAdapter(cfg).Run(context.Background(), dir, "# Задача\nДобавь компании", sink)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(run.FinalText, "VERDICT: APPROVED") || run.Model != "deepseek-v4-flash" {
		t.Fatalf("итог: %+v", run)
	}
	if q, blocked := parseBlocked(run.FinalText); blocked {
		t.Fatalf("ложный BLOCKED: %q", q)
	}
	if run.Usage.TokensIn == nil || *run.Usage.TokensIn != 10000 || run.Usage.CostUSD == nil {
		t.Fatalf("usage: %+v", run.Usage)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "src/companies.js")); !strings.Contains(string(b), "companies") {
		t.Fatal("файл не записан")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git/hooks/pre-commit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("хук в .git не должен появиться")
	}
	// Результат git status вернулся модели.
	var last struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(p.bodies[4], &last)
	if m := last.Messages[len(last.Messages)-1]; m.Role != "tool" || !strings.Contains(m.Content, "?? src/") {
		t.Fatalf("вывод git status: %+v", m)
	}
	tools := map[string]int{}
	files := ""
	for _, ev := range *events {
		if ev.Kind == "tool" {
			tools[ev.Tool]++
			files += strings.Join(ev.Files, ",")
		}
	}
	if tools["write_file"] != 2 || tools["run"] != 1 || tools["finish"] != 1 || files != "src/companies.js" {
		t.Fatalf("шаги: %v files=%q", tools, files)
	}
	if !strings.Contains(tr.String(), "▶ run git status") {
		t.Fatalf("транскрипт: %q", tr.String())
	}
}

func TestNativeAdapterKeyAndVerdicts(t *testing.T) {
	t.Setenv("RIVET_NATIVE_BASE_URL", "https://api.deepseek.com")
	t.Setenv("RIVET_NATIVE_KEY", "")
	cfg := Config{Adapter: AdapterNative, Model: "deepseek-v4-flash", Workdir: t.TempDir()}
	sink, _, _ := nativeSink()
	if _, err := newAdapter(cfg).Run(context.Background(), t.TempDir(), "x", sink); !errors.Is(err, ErrNoProviderKey) || !isEndpointError(err) {
		t.Fatalf("без ключа — ошибка подключения: %v", err)
	}
	p := &providerScript{calls: []map[string]any{{"name": "finish", "args": map[string]any{"verdict": "fail", "summary": "API хостинга недоступен"}}}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	t.Setenv("RIVET_NATIVE_BASE_URL", srv.URL)
	t.Setenv("RIVET_NATIVE_KEY", "sk-test")
	dir := t.TempDir()
	// Реализация: fail — неуспех запуска.
	run, err := newAdapter(cfg).Run(context.Background(), dir, "x", sink)
	if err == nil || !run.isError || !strings.Contains(err.Error(), "выполнить нельзя") || isEndpointError(err) {
		t.Fatalf("fail у реализации: %v", err)
	}
	// Review: fail — маркер, стадия разберёт его сама.
	sink.stage = pb.StageResult_REVIEW
	run, err = newAdapter(cfg).Run(context.Background(), dir, "x", sink)
	if err != nil {
		t.Fatal(err)
	}
	if approved, _ := parseVerdict(run.FinalText); approved {
		t.Fatal("fail ревьюера не одобрение")
	}
	if v, _ := parsePromptVerdict(run.FinalText); v != "fail" {
		t.Fatalf("маркер для шага prompt: %q", v)
	}
	// Локальный сервер работает без ключа.
	t.Setenv("RIVET_NATIVE_KEY", "")
	if _, err := (&nativeAdapter{cfg: Config{Model: "m", ExtraEnv: []string{"RIVET_NATIVE_BASE_URL=http://localhost:11434/v1"}}}).endpoint(); err != nil {
		t.Fatalf("Ollama без ключа: %v", err)
	}
}

// Модель стадии приходит в Assignment.endpoint: адрес, модель, лимиты и
// ключ из назначения; окружение runner'а не нужно, ключ маскируется.
func TestNativeAdapterUsesAssignmentEndpoint(t *testing.T) {
	dir := gitRepo(t)
	p := &providerScript{calls: []map[string]any{
		{"name": "list_dir", "args": map[string]any{"path": "."}},
		{"name": "finish", "args": map[string]any{"verdict": "ok", "summary": "готово"}},
	}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	t.Setenv("RIVET_NATIVE_BASE_URL", "") // окружение не используется

	as := &pb.Assignment{Model: "deepseek-v4-pro", Endpoint: &pb.ModelEndpoint{
		Api: "openai", BaseUrl: srv.URL, Model: "deepseek-v4-pro", Key: "sk-test",
		ContextWindow: 128000, Limits: &pb.SessionLimits{MaxTurns: 5},
	}}
	cfg := Config{Agent: AgentRivet, Adapter: AdapterNative, Model: "default-model"}.forAssignment(as)
	if len(cfg.SecretValues) == 0 || cfg.SecretValues[len(cfg.SecretValues)-1] != "sk-test" {
		t.Fatalf("ключ из назначения должен маскироваться: %v", cfg.SecretValues)
	}
	sink, _, _ := nativeSink()
	run, err := newAdapter(cfg).Run(context.Background(), dir, "# Задача\nx", sink)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(run.FinalText, "VERDICT: APPROVED") {
		t.Fatalf("итог: %q", run.FinalText)
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(p.bodies[0], &body)
	if body.Model != "deepseek-v4-pro" {
		t.Fatalf("модель из endpoint: %q", body.Model)
	}
}

// Без ключа в назначении и в секретах runner'а — понятная ошибка, а
// для локального адреса ключ не нужен.
func TestEndpointFromAssignmentKey(t *testing.T) {
	if _, err := endpointFromAssignment(&pb.ModelEndpoint{Api: "openai", BaseUrl: "https://api.deepseek.com", ConnectionId: "deepseek"}, nil); !errors.Is(err, ErrNoProviderKey) {
		t.Fatalf("без ключа: %v", err)
	}
	ep, err := endpointFromAssignment(&pb.ModelEndpoint{Api: "openai", BaseUrl: "https://api.deepseek.com", ConnectionId: "deepseek"},
		map[string]string{"RIVET_PROVIDER_KEY_DEEPSEEK": "sk-runner"})
	if err != nil || ep.client.Key != "sk-runner" {
		t.Fatalf("ключ из секретов runner'а: %v %+v", err, ep.client)
	}
	if _, err := endpointFromAssignment(&pb.ModelEndpoint{Api: "openai", BaseUrl: "http://localhost:11434/v1"}, nil); err != nil {
		t.Fatalf("локальный адрес без ключа: %v", err)
	}
}

// Тестировщик на агенте Rivet: запрет push доходит до команд агента
// (их окружение собирается из белого списка заново), push из run не
// проходит.
func TestNativeTesterPushBlocked(t *testing.T) {
	base, ws := gitRemote(t)
	p := &providerScript{calls: []map[string]any{
		{"name": "run", "args": map[string]any{"command": "git push origin HEAD:refs/heads/tester-push"}},
		{"name": "finish", "args": map[string]any{"verdict": "ok", "summary": "тесты готовы"}},
	}}
	srv := httptest.NewServer(p.handler(t))
	defer srv.Close()
	as := &pb.Assignment{Endpoint: &pb.ModelEndpoint{Api: "openai", BaseUrl: srv.URL, Model: "m", Key: "sk-test"}}
	cfg := Config{Agent: AgentRivet, Adapter: AdapterNative, Unsandboxed: true}.forAssignment(as)
	cfg.CommandEnv = pushBlockEnv(nil)
	sink, _, _ := nativeSink()
	if _, err := newAdapter(cfg).Run(context.Background(), ws, "# Задача\nтесты", sink); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir", filepath.Join(base, "repo.git"), "rev-parse", "--verify", "-q", "tester-push")
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("push тестировщика должен быть заблокирован: %s", out)
	}
}
