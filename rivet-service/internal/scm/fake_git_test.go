package scm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitT — git в каталоге теста с явной identity (на CI глобального
// config нет).
func fakeGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitStand — каталог bare-репозиториев как у e2e-стенда
// (<repos>/own/proj.git) и рабочая копия для коммитов.
func gitStand(t *testing.T) (repos, work string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	tmp := t.TempDir()
	repos = filepath.Join(tmp, "repos")
	bare := filepath.Join(repos, "own", "proj.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGitT(t, tmp, "init", "-q", "--bare", "-b", "main", bare)
	work = filepath.Join(tmp, "work")
	fakeGitT(t, tmp, "clone", "-q", bare, work)
	writeT(t, work, "docs/architecture/architecture.yaml", "version: v1\n")
	writeT(t, work, "shared.txt", "base\n")
	fakeGitT(t, work, "add", "-A")
	fakeGitT(t, work, "commit", "-q", "-m", "base")
	fakeGitT(t, work, "push", "-q", "origin", "main")
	return repos, work
}

// branchT — ветка от текущего main с одним изменённым файлом.
func branchT(t *testing.T, work, branch, file, content string) {
	t.Helper()
	fakeGitT(t, work, "checkout", "-q", "-B", branch, "origin/main")
	writeT(t, work, file, content)
	fakeGitT(t, work, "add", "-A")
	fakeGitT(t, work, "commit", "-q", "-m", branch)
	fakeGitT(t, work, "push", "-q", "-f", "origin", branch)
}

func remoteSHA(t *testing.T, work, ref string) string {
	t.Helper()
	fakeGitT(t, work, "fetch", "-q", "origin")
	return fakeGitT(t, work, "rev-parse", "origin/"+ref)
}

func TestFakeGitHeadAndRead(t *testing.T) {
	repos, work := gitStand(t)
	f := NewFakeGit(repos, "own/proj")
	ctx := context.Background()

	head, err := f.HeadSHA(ctx, "own/proj", "main")
	if err != nil || head != remoteSHA(t, work, "main") {
		t.Fatalf("HeadSHA: %q %v", head, err)
	}
	if def, err := f.HeadSHA(ctx, "own/proj", ""); err != nil || def != head {
		t.Fatalf("HeadSHA без ветки: %q %v", def, err)
	}
	if _, err := f.HeadSHA(ctx, "own/proj", "nope"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("нет ветки: %v", err)
	}
	// Репозиторий вне списка и вне каталога стенда (проект e2e без
	// настоящего репозитория): поведение прежнее, без ошибки.
	if sha, err := f.HeadSHA(ctx, "own/absent", "main"); err != nil || sha != "main" {
		t.Fatalf("репозиторий вне стенда: %q %v", sha, err)
	}
	if _, err := f.ReadFile(ctx, "own/absent", "main", "x"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("файл репозитория вне стенда: %v", err)
	}
	pr, _ := f.CreatePR(ctx, "own/absent", "agent/x", "main", "x", "")
	if sha, err := f.Merge(ctx, "own/absent", pr.Number); err != nil || sha != "main" {
		t.Fatalf("merge вне стенда: %q %v", sha, err)
	}

	fc, err := f.ReadFile(ctx, "own/proj", head, "docs/architecture/architecture.yaml")
	if err != nil || fc.Content != "version: v1\n" || fc.FileID == "" {
		t.Fatalf("ReadFile: %+v %v", fc, err)
	}
	if _, err := f.ReadFile(ctx, "own/proj", head, "docs/architecture/model/x.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("нет файла: %v", err)
	}
	if _, err := f.ReadFile(ctx, "own/proj", head, "docs"); err == nil || errors.Is(err, ErrFileNotFound) {
		t.Fatalf("каталог: %v", err)
	}
	for _, bad := range []string{"--output=x", "a..b", "main:x"} {
		if _, err := f.ReadFile(ctx, "own/proj", bad, "shared.txt"); err == nil || errors.Is(err, ErrFileNotFound) {
			t.Fatalf("версия %q: %v", bad, err)
		}
	}
	// Нет версии — не то же, что нет файла (как у настоящих хостингов).
	if _, err := f.ReadFile(ctx, "own/proj", "nope", "shared.txt"); !errors.Is(err, ErrRefNotFound) || errors.Is(err, ErrFileNotFound) {
		t.Fatalf("нет версии: %v", err)
	}
	// Имя, негодное для пути на диске, даже из списка: поведение прежнее,
	// диск не трогается.
	odd := NewFakeGit(repos, "../etc", "own/proj")
	if _, err := odd.ReadFile(ctx, "../etc", "main", "passwd"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("путь репозитория с ..: %v", err)
	}

	// RepoFS поверх git-режима: снимок читается по SHA.
	fsys := NewRepoFS(ctx, f, "own/proj", head, "docs/architecture", FSLimits{})
	if raw, err := fs.ReadFile(fsys, "architecture.yaml"); err != nil || string(raw) != "version: v1\n" {
		t.Fatalf("RepoFS: %q %v", raw, err)
	}

	// Файлы, записанные WriteFile (публикация GitOps), читаются из памяти,
	// как без git-режима.
	if _, err := f.WriteFile(ctx, "own/proj", "main", "deploy/app.yaml", "", "kind: x\n", "m"); err != nil {
		t.Fatal(err)
	}
	if fc, err := f.ReadFile(ctx, "own/proj", "main", "deploy/app.yaml"); err != nil || fc.Content != "kind: x\n" {
		t.Fatalf("файл из памяти: %+v %v", fc, err)
	}
}

// ReadFile и WriteFile согласованы: файл из git перезаписывается по sha
// его блоба, без него запись отклоняется как устаревшая. Новый файл в
// существующей ветке создаётся, а ветки, которой нет в git, запись не
// создаёт, как и на хостингах.
func TestFakeGitWriteAfterRead(t *testing.T) {
	repos, _ := gitStand(t)
	f := NewFakeGit(repos, "own/proj")
	ctx := context.Background()

	if _, err := f.WriteFile(ctx, "own/proj", "main", "shared.txt", "", "x\n", "m"); err == nil {
		t.Fatal("запись поверх файла из git без версии должна отклоняться")
	}
	fc, err := f.ReadFile(ctx, "own/proj", "main", "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteFile(ctx, "own/proj", "main", "shared.txt", "чужая", "x\n", "m"); err == nil {
		t.Fatal("запись с неверной версией должна отклоняться")
	}
	if _, err := f.WriteFile(ctx, "own/proj", "main", "shared.txt", fc.FileID, "x\n", "m"); err != nil {
		t.Fatalf("запись по sha блоба: %v", err)
	}
	if got, _ := f.ReadFile(ctx, "own/proj", "main", "shared.txt"); got.Content != "x\n" {
		t.Fatalf("после записи: %+v", got)
	}
	if _, err := f.WriteFile(ctx, "own/proj", "main", "deploy/new.yaml", "", "kind: x\n", "m"); err != nil {
		t.Fatalf("новый файл в существующей ветке: %v", err)
	}
	if _, err := f.WriteFile(ctx, "own/proj", "gitops/new", "app.yaml", "", "kind: x\n", "m"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("запись в ветку, которой нет в git: %v", err)
	}
}

// Пустой bare-репозиторий: ветки нет, а не сбой.
func TestFakeGitHeadEmptyRepo(t *testing.T) {
	repos, _ := gitStand(t)
	empty := filepath.Join(repos, "own", "empty.git")
	fakeGitT(t, repos, "init", "-q", "--bare", "-b", "main", empty)
	f := NewFakeGit(repos, "own/empty")
	for _, branch := range []string{"main", ""} {
		if _, err := f.HeadSHA(context.Background(), "own/empty", branch); !errors.Is(err, ErrBranchNotFound) {
			t.Fatalf("пустой репозиторий, ветка %q: %v", branch, err)
		}
	}
}

// Запись PR не перезаписывает занятый номер (второй rivetd на том же
// каталоге): PR уходит под следующий свободный.
func TestFakeGitSavePRNoOverwrite(t *testing.T) {
	repos, _ := gitStand(t)
	dir := filepath.Join(repos, "own", "proj.git")
	ctx := context.Background()
	if n, err := savePR(ctx, dir, 1, fakePR{branch: "a", base: "main"}); err != nil || n != 1 {
		t.Fatalf("первый PR: %d %v", n, err)
	}
	n, err := savePR(ctx, dir, 1, fakePR{branch: "b", base: "main"})
	if err != nil || n != 2 {
		t.Fatalf("занятый номер: %d %v", n, err)
	}
	if pr, ok, err := loadPR(ctx, dir, 1); err != nil || !ok || pr.branch != "a" {
		t.Fatalf("PR 1 перезаписан: %+v %v %v", pr, ok, err)
	}
	if pr, ok, err := loadPR(ctx, dir, 2); err != nil || !ok || pr.branch != "b" {
		t.Fatalf("PR 2: %+v %v %v", pr, ok, err)
	}
}

func TestFakeGitMergeFastForward(t *testing.T) {
	repos, work := gitStand(t)
	f := NewFakeGit(repos, "own/proj")
	ctx := context.Background()
	branchT(t, work, "agent/task-1", "docs/architecture/model/business.yaml", "elements: []\n")

	pr, err := f.CreatePR(ctx, "own/proj", "agent/task-1", "main", "task-1", "")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := f.Merge(ctx, "own/proj", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if main := remoteSHA(t, work, "main"); sha != main || main != remoteSHA(t, work, "agent/task-1") {
		t.Fatalf("fast-forward: merge %s, main %s", sha, main)
	}
	if fc, err := f.ReadFile(ctx, "own/proj", sha, "docs/architecture/model/business.yaml"); err != nil || fc.Content != "elements: []\n" {
		t.Fatalf("файл после merge: %+v %v", fc, err)
	}
	// Повторный merge того же PR ничего не двигает.
	if again, err := f.Merge(ctx, "own/proj", pr.Number); err != nil || again != sha {
		t.Fatalf("повтор: %q %v", again, err)
	}
}

func TestFakeGitMergeDivergedAndConflict(t *testing.T) {
	repos, work := gitStand(t)
	f := NewFakeGit(repos, "own/proj")
	ctx := context.Background()
	branchT(t, work, "agent/task-1", "a.txt", "one\n")
	branchT(t, work, "agent/task-2", "b.txt", "two\n")
	branchT(t, work, "agent/task-3", "shared.txt", "three\n")
	branchT(t, work, "agent/task-4", "shared.txt", "four\n")

	merge := func(branch string) (string, error) {
		pr, err := f.CreatePR(ctx, "own/proj", branch, "main", branch, "")
		if err != nil {
			t.Fatal(err)
		}
		return f.Merge(ctx, "own/proj", pr.Number)
	}
	if _, err := merge("agent/task-1"); err != nil {
		t.Fatal(err)
	}
	// Ветка от старого main без пересечений: merge-коммит с двумя родителями.
	sha, err := merge("agent/task-2")
	if err != nil {
		t.Fatal(err)
	}
	if main := remoteSHA(t, work, "main"); main != sha {
		t.Fatalf("main %s, merge %s", main, sha)
	}
	if parents := strings.Fields(fakeGitT(t, work, "rev-list", "--parents", "-n", "1", sha)); len(parents) != 3 {
		t.Fatalf("merge-коммит с двумя родителями: %v", parents)
	}
	for _, file := range []string{"a.txt", "b.txt"} {
		if _, err := f.ReadFile(ctx, "own/proj", sha, file); err != nil {
			t.Fatalf("%s после merge: %v", file, err)
		}
	}

	if _, err := merge("agent/task-3"); err != nil {
		t.Fatal(err)
	}
	before := remoteSHA(t, work, "main")
	// Та же строка изменена по-разному: конфликт как у хостинга, main не тронут.
	if _, err := merge("agent/task-4"); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("конфликт: %v", err)
	}
	if after := remoteSHA(t, work, "main"); after != before {
		t.Fatalf("main сдвинулся при конфликте: %s → %s", before, after)
	}
}

// Метка FakeConflictMarker работает и в git-режиме: первый merge
// отклоняется, второй проходит по-настоящему.
func TestFakeGitConflictMarker(t *testing.T) {
	repos, work := gitStand(t)
	f := NewFakeGit(repos, "own/proj")
	ctx := context.Background()
	branchT(t, work, "agent/task-9", "c.txt", "c\n")
	pr, _ := f.CreatePR(ctx, "own/proj", "agent/task-9", "main", "Задача "+FakeConflictMarker, "")
	if _, err := f.Merge(ctx, "own/proj", pr.Number); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("первый merge: %v", err)
	}
	sha, err := f.Merge(ctx, "own/proj", pr.Number)
	if err != nil || sha != remoteSHA(t, work, "main") {
		t.Fatalf("второй merge: %q %v", sha, err)
	}
}

// Без каталога и для PR, неизвестного адаптеру (рестарт rivetd), fake
// ведёт себя по-старому.
func TestFakeWithoutGitUnchanged(t *testing.T) {
	t.Setenv(FakeGitDirEnv, "")
	f := NewFake()
	ctx := context.Background()
	if sha, err := f.Merge(ctx, "own/proj", 42); err != nil || sha != "main" {
		t.Fatalf("merge: %q %v", sha, err)
	}
	if sha, err := f.HeadSHA(ctx, "own/proj", "feature"); err != nil || sha != "feature" {
		t.Fatalf("HeadSHA: %q %v", sha, err)
	}
	if _, err := f.ReadFile(ctx, "own/proj", "main", "x"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("ReadFile: %v", err)
	}

	repos, _ := gitStand(t)
	t.Setenv(FakeGitDirEnv, repos)
	// Каталог без списка: git-режим не действует ни для кого.
	if sha, err := NewFake().HeadSHA(ctx, "own/proj", "main"); err != nil || sha != "main" {
		t.Fatalf("каталог без списка: %q %v", sha, err)
	}
	t.Setenv(FakeGitReposEnv, " e2e/arch , own/proj")
	g := NewFake()
	if sha, err := g.HeadSHA(ctx, "own/proj", "main"); err != nil || len(sha) != 40 {
		t.Fatalf("флаг из окружения: %q %v", sha, err)
	}
	// Репозиторий стенда вне списка (общий e2e/demo): по-старому.
	if sha, err := g.HeadSHA(ctx, "e2e/demo", "main"); err != nil || sha != "main" {
		t.Fatalf("репозиторий вне списка: %q %v", sha, err)
	}
	// Неизвестный PR в git-режиме: ошибка, а не ложный успех merge.
	if sha, err := g.Merge(ctx, "own/proj", 42); err == nil {
		t.Fatalf("неизвестный PR в git-режиме: %q", sha)
	}
}

// PR git-режима переживает рестарт rivetd: новый адаптер мержит PR
// прошлого запуска и выдаёт номера больше прежних.
func TestFakeGitPRSurvivesRestart(t *testing.T) {
	repos, work := gitStand(t)
	ctx := context.Background()
	branchT(t, work, "agent/task-1", "a.txt", "one\n")
	branchT(t, work, "agent/task-2", "b.txt", "two\n")

	before := NewFakeGit(repos, "own/proj")
	pr1, err := before.CreatePR(ctx, "own/proj", "agent/task-1", "main", "task-1", "")
	if err != nil {
		t.Fatal(err)
	}
	pr2, err := before.CreatePR(ctx, "own/proj", "agent/task-2", "main", "task-2", "")
	if err != nil || pr2.Number <= pr1.Number {
		t.Fatalf("номера: %d, %d %v", pr1.Number, pr2.Number, err)
	}

	after := NewFakeGit(repos, "own/proj")
	sha, err := after.Merge(ctx, "own/proj", pr1.Number)
	if err != nil || sha != remoteSHA(t, work, "main") || sha != remoteSHA(t, work, "agent/task-1") {
		t.Fatalf("merge PR прошлого запуска: %q %v", sha, err)
	}
	pr3, err := after.CreatePR(ctx, "own/proj", "agent/task-2", "main", "task-2 again", "")
	if err != nil || pr3.Number <= pr2.Number {
		t.Fatalf("номер после рестарта %d не больше прежнего %d: %v", pr3.Number, pr2.Number, err)
	}
	if _, err := after.Merge(ctx, "own/proj", pr2.Number); err != nil {
		t.Fatalf("второй PR прошлого запуска: %v", err)
	}
}
