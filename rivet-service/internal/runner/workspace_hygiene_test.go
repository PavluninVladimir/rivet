package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// Риск P3-5 add-architecture-c4: прошлая сессия оставила в .git код, а
// сверка его не поймала (runner упал между сессией агента и сверкой). Это
// хуки, fsmonitor, фильтр через info/attributes, помощник учётных данных,
// core.worktree на чужой каталог. Подготовка следующей стадии идёт с
// токеном назначения у fetch и ничего из этого не исполняет, а
// конфигурация после неё как у свежего клона.
func TestWorkspacePrepIgnoresLeftoverGitCode(t *testing.T) {
	tmp, a, as := revAgent(t)
	as.GitToken = "benign-test-token"
	dir, err := a.workspace(context.Background(), as, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	markers := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(t.TempDir(), name+".sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch '"+filepath.Join(markers, name)+"'\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, h := range []string{"post-checkout", "reference-transaction", "post-merge", "pre-auto-gc"} {
		markerHook(t, filepath.Join(dir, ".git", "hooks", h), filepath.Join(markers, h))
	}
	gitT(t, dir, "config", "core.fsmonitor", script("fsmonitor", ""))
	gitT(t, dir, "config", "credential.helper", "!"+script("credential", ""))
	gitT(t, dir, "config", "filter.leftover.clean", script("clean", "cat\n"))
	gitT(t, dir, "config", "filter.leftover.smudge", script("smudge", "cat\n"))
	gitT(t, dir, "config", "filter.leftover.required", "true")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("* filter=leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	mustWriteFile(t, filepath.Join(elsewhere, "keep.txt"), "чужой каталог\n")
	gitT(t, dir, "config", "core.hooksPath", filepath.Join(dir, ".git", "hooks"))
	gitT(t, dir, "config", "core.worktree", elsewhere)

	// Ветка задачи сдвинулась на хостинге: checkout обновит файлы, и
	// smudge-фильтр сработал бы, переживи он подготовку.
	seed := filepath.Join(tmp, "seed")
	mustWriteFile(t, filepath.Join(seed, "main.js"), "next\n")
	gitT(t, seed, "commit", "-q", "-a", "-m", "следующий коммит")
	gitT(t, seed, "push", "-q", "origin", "agent/task-1")

	if _, err := a.workspace(context.Background(), as, func(string) {}); err != nil {
		t.Fatal(err)
	}
	noMarkers(t, markers)
	if b, err := os.ReadFile(filepath.Join(elsewhere, "keep.txt")); err != nil || string(b) != "чужой каталог\n" {
		t.Fatalf("подготовка тронула каталог из core.worktree: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.js")); string(b) != "next\n" {
		t.Fatalf("checkout не взял вершину ветки: %q", b)
	}
	for _, key := range strings.Fields(gitOutT(t, dir, "config", "--file", filepath.Join(dir, ".git", "config"), "--name-only", "--list")) {
		if !keepGitConfig(key, strings.TrimSpace(gitOutT(t, dir, "config", "--file", filepath.Join(dir, ".git", "config"), "--get", key))) &&
			key != "remote.origin.url" && key != "remote.origin.fetch" {
			t.Errorf("ключ %s пережил подготовку", key)
		}
	}
}

// .git в неизвестном состоянии (commondir на другой репозиторий, shallow,
// alternates, .git файлом): подготовка не правит такой клон, а клонирует
// заново.
func TestWorkspaceReclonesUnsafeGitDir(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"alternates": func(t *testing.T, dir string) {
			// Безобидное стороннее хранилище: пустой каталог объектов.
			other := filepath.Join(t.TempDir(), "objects")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(dir, ".git", "objects", "info", "alternates"), other+"\n")
		},
		"http-alternates": func(t *testing.T, dir string) {
			mustWriteFile(t, filepath.Join(dir, ".git", "objects", "info", "http-alternates"), "http://127.0.0.1:9/objects\n")
		},
		"config ссылкой": func(t *testing.T, dir string) {
			// Безобидная цель: копия той же конфигурации вне клона.
			cfg := filepath.Join(dir, ".git", "config")
			b, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(t.TempDir(), "config")
			mustWriteFile(t, copyPath, string(b))
			if err := os.Remove(cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copyPath, cfg); err != nil {
				t.Fatal(err)
			}
		},
		"commondir": func(t *testing.T, dir string) {
			mustWriteFile(t, filepath.Join(dir, ".git", "commondir"), "../elsewhere\n")
		},
		"shallow": func(t *testing.T, dir string) {
			mustWriteFile(t, filepath.Join(dir, ".git", "shallow"), strings.TrimSpace(gitOutT(t, dir, "rev-parse", "HEAD"))+"\n")
		},
		".git файлом": func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(dir, ".git"), "gitdir: /nowhere\n")
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			_, a, as := revAgent(t)
			dir, err := a.workspace(context.Background(), as, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			spoil(t, dir)
			var steps []string
			if _, err := a.workspace(context.Background(), as, func(s string) { steps = append(steps, s) }); err != nil {
				t.Fatalf("подготовка после порчи: %v", err)
			}
			if !slices.ContainsFunc(steps, func(s string) bool {
				return strings.HasPrefix(s, "рабочая копия клонируется заново")
			}) {
				t.Fatalf("клон не пересоздан: %v", steps)
			}
			for _, f := range []string{"commondir", "shallow", gitAlternates, gitHTTPAlternates} {
				if _, err := os.Lstat(filepath.Join(dir, ".git", filepath.FromSlash(f))); !errors.Is(err, os.ErrNotExist) {
					t.Errorf(".git/%s пережил пересоздание", f)
				}
			}
			if st, err := os.Lstat(filepath.Join(dir, ".git", "config")); err != nil || !st.Mode().IsRegular() {
				t.Errorf(".git/config после пересоздания не обычный файл: %v", err)
			}
			if got := strings.TrimSpace(gitOutT(t, dir, "rev-parse", "--abbrev-ref", "HEAD")); got != "agent/task-1" {
				t.Fatalf("ветка после пересоздания: %s", got)
			}
		})
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
