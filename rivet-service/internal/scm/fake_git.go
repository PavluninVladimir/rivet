package scm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// git-режим fake-адаптера (FakeGitDirEnv): e2e-стенду нужно, чтобы merge
// двигал основную ветку bare-репозитория, а снимок архитектуры читал
// файлы настоящей версии. Репозиторий «owner/name» лежит в
// <gitDir>/owner/name.git, как его клонируют runner'ы по RIVET_GIT_BASE.

// fakeRepoRe — путь репозитория из сегментов без «..» и служебных символов:
// он становится путём на диске.
var fakeRepoRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$`)

// repoDir — bare-репозиторий проекта на диске. ok=false — git-режим для
// этого репозитория не действует: его нет в списке FakeGitReposEnv, имя
// не годится для пути на диске или репозитория нет в каталоге стенда. Тогда
// fake ведёт себя по-старому.
func (f *Fake) repoDir(repo string) (string, bool) {
	if f.gitDir == "" || !f.gitRepos[repo] {
		return "", false
	}
	if !fakeRepoRe.MatchString(repo) || strings.Contains(repo, "..") {
		return "", false
	}
	dir := filepath.Join(f.gitDir, filepath.FromSlash(repo)+".git")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// fakeGit — команда git над bare-репозиторием. Ошибка несёт код выхода и
// stderr: по коду различаются «нет объекта» и «конфликт».
func fakeGit(ctx context.Context, dir string, args ...string) (string, error) {
	return fakeGitIn(ctx, dir, "", args...)
}

// fakeGitIn — fakeGit с данными на stdin. При отмене контекста git
// получает SIGTERM, а не SIGKILL: по SIGTERM он сам убирает lock-файлы
// ref'ов, а убитый процесс оставил бы их и заблокировал репозиторий стенда.
func fakeGitIn(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", dir}, args...)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &gitError{args: args, err: err, stderr: strings.TrimSpace(errb.String())}
	}
	return out.String(), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("fake git %s: %v: %s", strings.Join(e.args, " "), e.err, clip([]byte(e.stderr)))
}

func (e *gitError) Unwrap() error { return e.err }

// exitCode — код выхода git или -1, если процесс не дошёл до выхода.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// revParse — SHA объекта или ok=false, если его нет.
func revParse(ctx context.Context, dir, rev string) (string, bool, error) {
	out, err := fakeGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
	if err != nil {
		if exitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// PR git-режима живут в ref'ах bare-репозитория: refs/rivet-fake/pulls/<номер>
// указывает на блоб «ветка\nбаза\n». Так PR переживает рестарт rivetd, а
// номера новых PR не совпадают с номерами прошлых запусков.
const fakePRRefs = "refs/rivet-fake/pulls/"

// savePR записывает PR под номером n, только если такого ref'а ещё нет:
// пустое прежнее значение в update-ref запрещает перезапись. Занятый
// номер (второй rivetd на том же каталоге) — повтор со следующим.
// Возвращает номер, под которым PR записан.
func savePR(ctx context.Context, dir string, n int, pr fakePR) (int, error) {
	blob, err := fakeGitIn(ctx, dir, pr.branch+"\n"+pr.base+"\n", "hash-object", "-w", "--stdin")
	if err != nil {
		return 0, err
	}
	// Запись ref'а не прерывается отменой запроса: недописанный ref хуже
	// лишней секунды ожидания.
	wctx := context.WithoutCancel(ctx)
	for range 50 {
		if _, err = fakeGit(wctx, dir, "update-ref", fakePRRefs+strconv.Itoa(n), strings.TrimSpace(blob), ""); err == nil {
			return n, nil
		}
		if _, exists, rerr := revParse(wctx, dir, fakePRRefs+strconv.Itoa(n)); rerr != nil || !exists {
			return 0, err
		}
		n++
	}
	return 0, fmt.Errorf("fake git: не удалось занять номер PR: %w", err)
}

func loadPR(ctx context.Context, dir string, n int) (fakePR, bool, error) {
	sha, ok, err := revParse(ctx, dir, fakePRRefs+strconv.Itoa(n))
	if err != nil || !ok {
		return fakePR{}, false, err
	}
	body, err := fakeGit(ctx, dir, "cat-file", "blob", sha)
	if err != nil {
		return fakePR{}, false, err
	}
	lines := strings.Split(body, "\n")
	if len(lines) < 2 || lines[0] == "" {
		return fakePR{}, false, fmt.Errorf("fake git: повреждённая запись PR #%d", n)
	}
	return fakePR{branch: lines[0], base: lines[1]}, true, nil
}

// maxPR — наибольший номер PR, сохранённый в репозитории.
func maxPR(ctx context.Context, dir string) (int, error) {
	out, err := fakeGit(ctx, dir, "for-each-ref", "--format=%(refname)", fakePRRefs)
	if err != nil {
		return 0, err
	}
	m := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if n, err := strconv.Atoi(strings.TrimPrefix(line, fakePRRefs)); err == nil && n > m {
			m = n
		}
	}
	return m, nil
}

// safeRef — ветка или SHA, который git не примет за опцию и не прочтёт
// как диапазон: значение приходит из данных проекта.
func safeRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\r\n:^~?*[\\") || strings.Contains(ref, "..") {
		return fmt.Errorf("fake git: некорректная версия %q", ref)
	}
	return nil
}

func (f *Fake) gitHead(ctx context.Context, dir, branch string) (string, error) {
	rev := "HEAD"
	if branch != "" {
		if err := safeRef(branch); err != nil {
			return "", err
		}
		rev = "refs/heads/" + branch
	}
	sha, ok, err := revParse(ctx, dir, rev+"^{commit}")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("fake git branch %s: %w", branch, ErrBranchNotFound)
	}
	return sha, nil
}

func (f *Fake) gitRead(ctx context.Context, dir, repo, ref, path string) (FileContent, error) {
	if ref == "" {
		ref = "HEAD"
	} else if err := safeRef(ref); err != nil {
		return FileContent{}, err
	}
	commit, ok, err := revParse(ctx, dir, ref+"^{commit}")
	if err != nil {
		return FileContent{}, err
	}
	if !ok {
		// Нет версии — не то же, что нет файла: как и настоящие хостинги.
		return FileContent{}, fmt.Errorf("fake git %s@%s: %w", repo, ref, ErrRefNotFound)
	}
	path = strings.TrimPrefix(path, "/")
	obj, ok, err := revParse(ctx, dir, commit+":"+path)
	if err != nil {
		return FileContent{}, err
	}
	if !ok {
		return FileContent{}, ErrFileNotFound
	}
	kind, err := fakeGit(ctx, dir, "cat-file", "-t", obj)
	if err != nil {
		return FileContent{}, err
	}
	if k := strings.TrimSpace(kind); k != "blob" {
		return FileContent{}, fmt.Errorf("fake git: %s: не файл (%s)", path, k)
	}
	size, err := fakeGit(ctx, dir, "cat-file", "-s", obj)
	if err != nil {
		return FileContent{}, err
	}
	if n, _ := strconv.ParseInt(strings.TrimSpace(size), 10, 64); n > MaxResponseBytes {
		return FileContent{}, fmt.Errorf("fake git: %s: %d байт, больше лимита чтения", path, n)
	}
	content, err := fakeGit(ctx, dir, "cat-file", "blob", obj)
	if err != nil {
		return FileContent{}, err
	}
	return FileContent{Content: content, FileID: obj}, nil
}

// gitMerge продвигает базовую ветку PR: fast-forward, если ветка PR её
// потомок, иначе merge-коммит через `git merge-tree --write-tree`.
// Конфликт содержимого — ErrMergeConflict, как у настоящего хостинга.
// Возвращает SHA новой вершины базовой ветки.
func (f *Fake) gitMerge(ctx context.Context, dir string, number int, pr fakePR) (string, error) {
	f.gitMu.Lock()
	defer f.gitMu.Unlock()
	base := pr.base
	if base == "" {
		base = "main"
	}
	if err := safeRef(base); err != nil {
		return "", err
	}
	if err := safeRef(pr.branch); err != nil {
		return "", err
	}
	baseSHA, ok, err := revParse(ctx, dir, "refs/heads/"+base+"^{commit}")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("fake git merge: базовая ветка %s: %w", base, ErrBranchNotFound)
	}
	tipSHA, ok, err := revParse(ctx, dir, "refs/heads/"+pr.branch+"^{commit}")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("fake git merge: ветка PR %s: %w", pr.branch, ErrBranchNotFound)
	}
	isAncestor := func(a, b string) (bool, error) {
		_, err := fakeGit(ctx, dir, "merge-base", "--is-ancestor", a, b)
		if err == nil {
			return true, nil
		}
		if exitCode(err) == 1 {
			return false, nil
		}
		return false, err
	}
	// Ветка PR уже в базе: сдвигать нечего.
	if in, err := isAncestor(tipSHA, baseSHA); err != nil {
		return "", err
	} else if in {
		return baseSHA, nil
	}
	next := tipSHA
	ff, err := isAncestor(baseSHA, tipSHA)
	if err != nil {
		return "", err
	}
	if !ff {
		out, err := fakeGit(ctx, dir, "merge-tree", "--write-tree", baseSHA, tipSHA)
		if err != nil {
			if exitCode(err) == 1 {
				return "", fmt.Errorf("fake git merge #%d: %w", number, ErrMergeConflict)
			}
			return "", err
		}
		tree := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		msg := fmt.Sprintf("Merge pull request #%d from %s", number, pr.branch)
		commit, err := fakeGit(ctx, dir, "-c", "user.name=rivet-fake", "-c", "user.email=fake@rivet.local",
			"commit-tree", tree, "-p", baseSHA, "-p", tipSHA, "-m", msg)
		if err != nil {
			return "", err
		}
		next = strings.TrimSpace(commit)
	}
	// Сдвиг с проверкой прежнего значения: если ветку подвинул кто-то ещё
	// (push на стенде), merge не затирает его коммиты. Отмена запроса сдвиг
	// не прерывает: ветка либо сдвинута, либо нет, без lock-файла.
	if _, err := fakeGit(context.WithoutCancel(ctx), dir, "update-ref", "refs/heads/"+base, next, baseSHA); err != nil {
		return "", err
	}
	return next, nil
}
