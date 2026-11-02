package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitT — git в каталоге теста с явной identity (на CI глобального config нет).
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Разрешение конфликта с базовой веткой (change add-merge-conflict-recovery)
// может дать дерево, равное HEAD ветки задачи. Runner всё равно должен
// закоммитить незавершённый merge: иначе базовая ветка не станет предком
// ветки задачи и конфликт на хостинге останется. Без MERGE_HEAD и без
// изменений пустой коммит не создаётся.
func TestGitCommitPushCompletesPendingMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	gitT(t, tmp, "init", "-q", "--bare", "-b", "main", origin)

	seed := filepath.Join(tmp, "seed")
	gitT(t, tmp, "clone", "-q", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-q", "-m", "base")
	gitT(t, seed, "push", "-q", "origin", "main")

	ws := filepath.Join(tmp, "ws")
	gitT(t, tmp, "clone", "-q", origin, ws)
	gitT(t, ws, "checkout", "-q", "-b", "agent/task-1")
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitPush(ctx, ws, "agent/task-1", nil, "task-1: реализация"); err != nil {
		t.Fatal(err)
	}

	// Базовая ветка ушла вперёд (смержена другая задача).
	if err := os.WriteFile(filepath.Join(seed, "c.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-q", "-m", "other task")
	gitT(t, seed, "push", "-q", "origin", "main")

	// Агент влил базовую ветку без коммита и оставил дерево задачи как есть.
	gitT(t, ws, "fetch", "-q", "origin")
	gitT(t, ws, "merge", "--no-commit", "--no-ff", "-s", "ours", "origin/main")
	if err := gitCommitPush(ctx, ws, "agent/task-1", nil, "task-1: разрешение конфликта"); err != nil {
		t.Fatal(err)
	}
	gitT(t, ws, "fetch", "-q", "origin")
	cmd := exec.Command("git", "merge-base", "--is-ancestor", "origin/main", "origin/agent/task-1")
	cmd.Dir = ws
	if err := cmd.Run(); err != nil {
		t.Fatalf("базовая ветка должна стать предком ветки задачи: %v", err)
	}

	// Повтор без изменений и без merge не создаёт пустого коммита.
	head := gitT(t, ws, "rev-parse", "HEAD")
	if err := gitCommitPush(ctx, ws, "agent/task-1", nil, "task-1: повтор"); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, ws, "rev-parse", "HEAD"); got != head {
		t.Fatalf("пустой коммит без изменений: %s → %s", head, got)
	}
}
