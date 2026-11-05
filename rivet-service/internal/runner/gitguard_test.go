package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// gitRemote — голый репозиторий base/repo.git с веткой main и клон ws.
func gitRemote(t *testing.T) (base, ws string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git не установлен")
	}
	t.Setenv("HOME", t.TempDir()) // глобальный конфиг разработчика не участвует
	base = t.TempDir()
	gitT(t, base, "init", "-q", "--bare", "-b", "main", "repo.git")
	seed := filepath.Join(base, "seed")
	gitT(t, base, "clone", "-q", filepath.Join(base, "repo.git"), seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-q", "-m", "init")
	gitT(t, seed, "push", "-q", "origin", "HEAD:main")
	ws = filepath.Join(base, "ws")
	gitT(t, base, "clone", "-q", filepath.Join(base, "repo.git"), ws)
	return base, ws
}

// markerHook — безобидный хук: создаёт файл-маркер во временном каталоге
// теста.
func markerHook(t *testing.T, path, marker string) {
	t.Helper()
	if err := writeMarkerHook(path, marker); err != nil {
		t.Fatal(err)
	}
}

func writeMarkerHook(path, marker string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755)
}

func noMarkers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("git runner'а исполнил код рабочей копии: появился маркер %s", e.Name())
	}
}

// git runner'а (commit и push) не исполняет хуки и fsmonitor из
// конфигурации рабочей копии.
func TestRunnerGitIgnoresWorkspaceHooks(t *testing.T) {
	base, ws := gitRemote(t)
	gitT(t, ws, "checkout", "-q", "-b", "feature/x")
	markers := t.TempDir()
	for _, h := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push", "reference-transaction", "post-checkout"} {
		markerHook(t, filepath.Join(ws, ".git", "hooks", h), filepath.Join(markers, h))
	}
	fsmon := filepath.Join(t.TempDir(), "fsmonitor.sh")
	markerHook(t, fsmon, filepath.Join(markers, "fsmonitor"))
	gitT(t, ws, "config", "core.fsmonitor", fsmon)

	if err := os.WriteFile(filepath.Join(ws, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitPush(context.Background(), ws, "feature/x", nil, "task-1: x"); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, base, "--git-dir", filepath.Join(base, "repo.git"), "log", "-1", "--format=%s", "feature/x"); got != "task-1: x" {
		t.Fatalf("push не дошёл: %q", got)
	}
	noMarkers(t, markers)
}

// Окружение git runner'а собрано из белого списка: секретов runner'а нет,
// есть безопасные настройки и askpass назначения.
func TestGitEnvWhitelist(t *testing.T) {
	for _, k := range []string{"RIVET_RUNNER_TOKEN", "RIVET_PROVIDER_KEY_DEMO", "RIVET_NATIVE_KEY", "AWS_SECRET_ACCESS_KEY", "GH_TOKEN"} {
		t.Setenv(k, "test-value")
	}
	env := gitEnv([]string{"GIT_ASKPASS=/tmp/askpass.sh", "RIVET_GIT_USER=rivet", "RIVET_GIT_TOKEN=test-value"})
	extra := []string{
		"GIT_TERMINAL_PROMPT", "GIT_CONFIG_COUNT", "GIT_ASKPASS", "RIVET_GIT_USER", "RIVET_GIT_TOKEN",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_NO_REPLACE_OBJECTS",
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(gitEnvKeep, name) && !slices.Contains(extra, name) &&
			!strings.HasPrefix(name, "GIT_CONFIG_KEY_") && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			t.Errorf("переменная %q не из белого списка", name)
		}
	}
	for _, want := range []string{
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=credential.helper", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_NO_REPLACE_OBJECTS=1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("нет %q", want)
		}
	}
	for _, kv := range scrubEnv(os.Environ()) {
		if name, _, _ := strings.Cut(kv, "="); IsSecretEnv(name) {
			t.Errorf("scrubEnv оставил %q", name)
		}
	}
}

// Снимок служебных файлов: изменения конфигурации, новые хуки и commondir
// видны сверкой и возвращаются к снимку.
func TestGitGuardRestores(t *testing.T) {
	_, ws := gitRemote(t)
	cfgPath := filepath.Join(ws, ".git", "config")
	orig, _ := os.ReadFile(cfgPath)
	snap, err := snapshotGit(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.verify("агент"); err != nil {
		t.Fatalf("без изменений: %v", err)
	}
	gitT(t, ws, "config", "core.fsmonitor", "/bin/true")
	markerHook(t, filepath.Join(ws, ".git", "hooks", "pre-commit"), filepath.Join(t.TempDir(), "m"))
	if err := os.WriteFile(filepath.Join(ws, ".git", "commondir"), []byte("../elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = snap.verify("агент")
	var te *gitTamperError
	if !errors.As(err, &te) || !errors.Is(err, errGitTampered) {
		t.Fatalf("ожидается gitTamperError: %v", err)
	}
	for _, p := range []string{".git/config", ".git/hooks/pre-commit", ".git/commondir"} {
		if !slices.Contains(te.changed, p) {
			t.Errorf("сверка не увидела %s: %v", p, te.changed)
		}
	}
	if te.removed || !strings.Contains(err.Error(), "агент изменил служебные файлы git") {
		t.Fatalf("причина: %v", err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != string(orig) {
		t.Fatalf(".git/config не возвращён:\n%s", b)
	}
	for _, p := range []string{"hooks/pre-commit", "commondir"} {
		if _, err := os.Stat(filepath.Join(ws, ".git", p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf(".git/%s должен быть удалён", p)
		}
	}
	gitT(t, ws, "status", "--short")
}

// Alternates, появившиеся за сессию, видны сверкой и удаляются: снимок
// служебных файлов включает objects/info/alternates и http-alternates,
// хотя каталог objects он не обходит.
func TestGitGuardDetectsAlternates(t *testing.T) {
	_, ws := gitRemote(t)
	snap, err := snapshotGit(ws)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(ws, ".git", "objects", "info", "alternates"), other+"\n")
	mustWriteFile(t, filepath.Join(ws, ".git", "objects", "info", "http-alternates"), "http://127.0.0.1:9/objects\n")
	var te *gitTamperError
	if err := snap.verify("агент"); !errors.As(err, &te) || te.removed {
		t.Fatalf("ожидается gitTamperError с откатом: %v", err)
	}
	for _, p := range []string{".git/" + gitAlternates, ".git/" + gitHTTPAlternates} {
		if !slices.Contains(te.changed, p) {
			t.Errorf("сверка не увидела %s: %v", p, te.changed)
		}
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(p))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s должен быть удалён", p)
		}
	}
	gitT(t, ws, "status", "--short")
}

// .git подменён файлом: вернуть нельзя, клон удаляется целиком.
func TestGitGuardRemovesReplacedGitDir(t *testing.T) {
	_, ws := gitRemote(t)
	snap, err := snapshotGit(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ws, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var te *gitTamperError
	if err := snap.verify("агент"); !errors.As(err, &te) || !te.removed {
		t.Fatalf("ожидается удаление клона: %v", err)
	}
	if _, err := os.Stat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("клон должен быть удалён")
	}
}

// tamperAdapter — агент, который меняет служебные файлы git клона.
type tamperAdapter struct{ markers string }

func (a tamperAdapter) Run(_ context.Context, dir, _ string, _ runSink) (agentRun, error) {
	if err := writeMarkerHook(filepath.Join(dir, ".git", "hooks", "post-checkout"), filepath.Join(a.markers, "post-checkout")); err != nil {
		return agentRun{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return agentRun{}, err
	}
	_, _ = f.WriteString("[core]\n\tfsmonitor = /bin/true\n")
	_ = f.Close()
	_ = os.WriteFile(filepath.Join(dir, "work.txt"), []byte("сделано\n"), 0o644)
	return agentRun{FinalText: "готово"}, nil
}

// Стадия реализации, в которой агент изменил служебные файлы git:
// неуспех с причиной, push не выполняется, файлы возвращены.
func TestStageFailsWhenAgentChangesGitFiles(t *testing.T) {
	base, _ := gitRemote(t)
	markers := t.TempDir()
	a := &agent{
		cfg:      Config{Adapter: AdapterNative, Workdir: t.TempDir(), GitBase: "file://" + base + "/"},
		adapter:  tamperAdapter{markers: markers},
		contexts: newContextHub(),
		cancel:   map[string]*stageCancel{},
	}
	as := &pb.Assignment{TaskId: "t1", SessionId: "s1", Stage: pb.StageResult_CODING, Repo: "repo",
		Branch: "feature/t1", BaseBranch: "main", TaskNum: 1, Title: "x"}
	var mu sync.Mutex
	var res *pb.StageResult
	a.executeStage(context.Background(), as, func(m *pb.RunnerMsg) {
		mu.Lock()
		defer mu.Unlock()
		if r := m.GetStageResult(); r != nil {
			res = r
		}
	})
	if res == nil || res.Ok || !strings.Contains(res.Detail, "агент изменил служебные файлы git") {
		t.Fatalf("итог стадии: %+v", res)
	}
	ws := filepath.Join(a.cfg.Workdir, "repos", workspaceKey(as))
	if b, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); strings.Contains(string(b), "fsmonitor") {
		t.Fatalf(".git/config не возвращён:\n%s", b)
	}
	cmd := exec.Command("git", "--git-dir", filepath.Join(base, "repo.git"), "rev-parse", "--verify", "-q", "feature/t1")
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("push не должен был выполниться: %s", out)
	}
	noMarkers(t, markers)
}

// git runner'а игнорирует глобальный и системный конфиг: безобидный
// clean-фильтр из ~/.gitconfig и .gitattributes не исполняется при коммите.
func TestRunnerGitIgnoresGlobalConfig(t *testing.T) {
	base, ws := gitRemote(t)
	gitT(t, ws, "checkout", "-q", "-b", "feature/x")
	home := os.Getenv("HOME") // gitRemote выставил HOME в t.TempDir()
	marker := filepath.Join(t.TempDir(), "filter-ran")
	// Безобидный фильтр: вместо содержимого пишет маркер во временный файл.
	gitcfg := "[filter \"evil\"]\n\tclean = touch '" + marker + "'; cat\n\tsmudge = cat\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitcfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".gitattributes"), []byte("* filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "new.txt"), []byte("данные\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitPush(context.Background(), ws, "feature/x", nil, "task-1: x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("clean-фильтр из глобального конфига исполнился в git runner'а")
	}
	if got := gitT(t, base, "--git-dir", filepath.Join(base, "repo.git"), "log", "-1", "--format=%s", "feature/x"); got != "task-1: x" {
		t.Fatalf("push не дошёл: %q", got)
	}
}

// git replace из команды агента не влияет на git-операции runner'а, а
// стадия падает с причиной «агент изменил служебные файлы git».
func TestGitGuardDetectsReplaceRefs(t *testing.T) {
	_, ws := gitRemote(t)
	// Второй коммит, чтобы было чем подменить первый.
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "second")
	head := gitT(t, ws, "rev-parse", "HEAD")
	prev := gitT(t, ws, "rev-parse", "HEAD~1")

	snap, err := snapshotGit(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.verify("агент"); err != nil {
		t.Fatalf("без изменений: %v", err)
	}
	// Команда агента подменяет HEAD деревом прошлого коммита.
	gitT(t, ws, "replace", head, prev)
	// git runner'а не применяет подмену (GIT_NO_REPLACE_OBJECTS): его HEAD
	// по-прежнему second.
	out, err := runShellEnv(context.Background(), ws, "git log -1 --format=%s", gitEnv(nil), nil)
	if err != nil || !strings.Contains(out, "second") {
		t.Fatalf("git runner'а должен видеть дерево без подмены: %q %v", out, err)
	}
	// Стадия падает с причиной, refs/replace откатывается.
	terr := &gitTamperError{}
	if err := snap.verify("агент"); !errors.As(err, &terr) || !slices.Contains(terr.changed, "refs/replace/*") {
		t.Fatalf("сверка не увидела refs/replace: %v", err)
	}
	if again, _, err := snap.diff(); err != nil || len(again) != 0 {
		t.Fatalf("refs/replace не откатился: %v %v", again, err)
	}
}

// Мультизначный refspec origin (git config --add из команды агента)
// приводится к одному стандартному значению, подготовка не падает.
func TestPrepareWorkspaceGitMultiValueRefspec(t *testing.T) {
	_, ws := gitRemote(t)
	gitT(t, ws, "config", "--add", "remote.origin.fetch", "+refs/heads/evil:refs/remotes/origin/main")
	origin := gitT(t, ws, "config", "--get", "remote.origin.url")
	if err := prepareWorkspaceGit(context.Background(), ws, origin); err != nil {
		t.Fatalf("подготовка с мультизначным refspec: %v", err)
	}
	out := gitT(t, ws, "config", "--get-all", "remote.origin.fetch")
	if out != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("refspec должен стать одним стандартным: %q", out)
	}
}
