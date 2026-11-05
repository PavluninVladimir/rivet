package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Подготовка workspace убирает состояние .git, которое мог оставить агент
// прошлой стадии: replace-ссылки и подменённый refspec origin (ревью Opus,
// круг 2). Иначе они переживают fetch, reset и clean и меняют историю для
// проверки архитектуры и следующих задач этого репозитория.
func TestWorkspaceDropsTamperedGitState(t *testing.T) {
	tmp := testerRepo(t)
	cfg := Config{Workdir: filepath.Join(tmp, "work"), GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	as := &pb.Assignment{TaskId: "t1", TaskNum: 1, Stage: pb.StageResult_TESTING, Repo: "o/r",
		Branch: "agent/task-1", BaseBranch: "main", SessionId: "s1"}
	dir, err := a.workspace(context.Background(), as, func(string) {})
	if err != nil {
		t.Fatal(err)
	}

	// Агент оставил подмену объекта и refspec, кладущий ветку задачи в origin/main.
	base := strings.TrimSpace(gitOutT(t, dir, "rev-parse", "origin/main"))
	head := strings.TrimSpace(gitOutT(t, dir, "rev-parse", "HEAD"))
	gitT(t, dir, "replace", base, head)
	gitT(t, dir, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/agent/task-1:refs/remotes/origin/main")
	// Финальная проверка: подменённый адрес, его переписывание напрямую и
	// через include, pushurl.
	origin := strings.TrimSpace(gitOutT(t, dir, "config", "remote.origin.url"))
	fake := filepath.Join(tmp, "fake.git")
	gitT(t, tmp, "init", "-q", "--bare", fake)
	gitT(t, dir, "push", "-q", fake, "HEAD:refs/heads/main")
	gitT(t, dir, "config", "remote.origin.url", fake)
	gitT(t, dir, "config", "remote.origin.pushurl", origin)
	gitT(t, dir, "config", "url."+fake+".insteadOf", "file:///nowhere")
	inc := filepath.Join(tmp, "evil.config")
	if err := os.WriteFile(inc, []byte("[url \""+fake+"\"]\n\tinsteadOf = "+origin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "config", "include.path", inc)

	if _, err := a.workspace(context.Background(), as, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if refs := strings.TrimSpace(gitOutT(t, dir, "for-each-ref", "refs/replace/")); refs != "" {
		t.Fatalf("replace-ссылки пережили подготовку: %s", refs)
	}
	if spec := strings.TrimSpace(gitOutT(t, dir, "config", "--get-all", "remote.origin.fetch")); spec != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("refspec origin не восстановлен: %s", spec)
	}
	if got := strings.TrimSpace(gitOutT(t, dir, "rev-parse", "origin/main")); got != base {
		t.Fatalf("origin/main указывает не на основную ветку: %s, ожидался %s", got, base)
	}
	if got := strings.TrimSpace(gitOutT(t, dir, "config", "remote.origin.url")); got != origin {
		t.Fatalf("адрес origin не восстановлен: %s, ожидался %s", got, origin)
	}
	cmd := exec.Command("git", "config", "--local", "--get-regexp", `^(url|include|includeif)\.|^remote\.origin\.pushurl$`)
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("подмены в конфигурации пережили подготовку: %s", out)
	}
}

// revAgent — runner и назначение TESTING на ветку задачи testerRepo.
func revAgent(t *testing.T) (string, *agent, *pb.Assignment) {
	t.Helper()
	tmp := testerRepo(t)
	cfg := Config{Workdir: filepath.Join(tmp, "work"), GitBase: "file://" + tmp + "/repos/", Adapter: AdapterWrap}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	as := &pb.Assignment{TaskId: "t1", TaskNum: 1, Stage: pb.StageResult_TESTING, Repo: "o/r",
		Branch: "agent/task-1", BaseBranch: "main", SessionId: "s1"}
	return tmp, a, as
}

// Репро ревью ограничителя: ветку задачи переписали (rebase и force push).
// Стадия TESTING на runner'е, где ветка уже есть локально, берёт вершину с
// хостинга, а не продолжает старую локальную.
func TestWorkspaceFollowsForcePush(t *testing.T) {
	tmp, a, as := revAgent(t)
	dir, err := a.workspace(context.Background(), as, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(tmp, "seed")
	if err := os.WriteFile(filepath.Join(seed, "main.js"), []byte("rewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "commit", "-q", "-a", "--amend", "-m", "переписано")
	gitT(t, seed, "push", "-q", "-f", "origin", "agent/task-1")
	hosting := strings.TrimSpace(gitOutT(t, seed, "rev-parse", "HEAD"))
	if _, err := a.workspace(context.Background(), as, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitOutT(t, dir, "rev-parse", "HEAD")); got != hosting {
		t.Fatalf("проверяется старая вершина %s, на хостинге %s", got, hosting)
	}
}

// Репро ревью ограничителя: подмена в .git/config.worktree (worktreeConfig)
// снимается подготовкой так же, как в .git/config.
func TestWorkspaceScrubsWorktreeConfig(t *testing.T) {
	tmp, a, as := revAgent(t)
	dir, err := a.workspace(context.Background(), as, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(tmp, "fake.git")
	gitT(t, tmp, "init", "-q", "--bare", fake)
	origin := strings.TrimSpace(gitOutT(t, dir, "config", "remote.origin.url"))
	gitT(t, dir, "config", "extensions.worktreeConfig", "true")
	gitT(t, dir, "config", "--worktree", "url."+fake+".insteadOf", origin)
	gitT(t, dir, "config", "--worktree", "remote.origin.url", fake)
	if _, err := a.workspace(context.Background(), as, func(string) {}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "--includes", "--get-regexp", `^url\.`)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("url.* из config.worktree пережил подготовку: %s", out)
	}
	if got := strings.TrimSpace(gitOutT(t, dir, "config", "remote.origin.url")); got != origin {
		t.Fatalf("адрес origin после подготовки: %s, ожидался %s", got, origin)
	}
}

// noHooksEnv не затирает GIT_CONFIG_* из окружения runner'а.
func TestNoHooksEnvKeepsRunnerConfig(t *testing.T) {
	base := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.sslCAInfo", "GIT_CONFIG_VALUE_0=/etc/ca.pem"}
	got := strings.Join(noHooksEnv(base), " ")
	if got != "GIT_CONFIG_KEY_1=core.hooksPath GIT_CONFIG_VALUE_1=/dev/null GIT_CONFIG_COUNT=2" {
		t.Fatalf("окружение без хуков: %s", got)
	}
}

func gitOutT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
