package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PRFileList — файлы PR и вершина ветки PR, для которой они получены.
type PRFileList struct {
	Files   []string
	HeadSHA string // пусто, если хостинг вершину не отдал (fake без git-режима)
}

// PRFileLister — список файлов PR по данным хостинга. Граница задачи
// описания архитектуры («только docs/architecture») проверяется по нему на
// control plane: гейт в рабочей копии runner'а агент может обмануть
// состоянием .git (add-architecture-c4). Пути от корня репозитория, у
// переименования оба пути, список отсортирован. Список, который хостинг
// отдал не целиком, — ErrPRFilesTruncated: граница по нему не проверяется.
type PRFileLister interface {
	PRFiles(ctx context.Context, repo string, number int) (PRFileList, error)
}

// SHAMerger — merge только той вершины ветки PR, которую проверили.
// Сдвинулась — ErrHeadMoved, merge не выполняется.
type SHAMerger interface {
	MergeAt(ctx context.Context, repo string, number int, headSHA string) (string, error)
}

// ErrPRFilesTruncated — хостинг отдал не весь список файлов PR.
var ErrPRFilesTruncated = errors.New("список файлов PR неполный")

// ErrHeadMoved — вершина ветки PR сдвинулась после проверки.
var ErrHeadMoved = errors.New("ветка PR сдвинулась после проверки")

// Счётчик файлов PR и список разошлись (push между запросами) или счётчик
// ещё не посчитан: один повтор, затем неполный список или временный сбой
// (ревью Opus, финальная проверка).
var (
	errCountMismatch = errors.New("число файлов PR не сходится со счётчиком")
	errCountPending  = errors.New("счётчик файлов MR ещё не посчитан")
)

// prFilesRetryPause — пауза перед повтором чтения списка. Переменная для тестов.
var prFilesRetryPause = 500 * time.Millisecond

// retryPRFiles — одно чтение и один повтор при расхождении счётчика.
// Повторное расхождение — неполный список, повторно непосчитанный счётчик —
// временный сбой хостинга (guard повторит и эскалирует).
func retryPRFiles(ctx context.Context, read func() (PRFileList, error)) (PRFileList, error) {
	l, err := read()
	if !errors.Is(err, errCountMismatch) && !errors.Is(err, errCountPending) {
		return l, err
	}
	select {
	case <-ctx.Done():
		return PRFileList{}, ctx.Err()
	case <-time.After(prFilesRetryPause):
	}
	l, err = read()
	if errors.Is(err, errCountMismatch) {
		return PRFileList{}, fmt.Errorf("%w: %v", ErrPRFilesTruncated, err)
	}
	return l, err
}

// PRFiles — файлы PR GitHub с одним повтором при расхождении счётчика.
func (g *GitHub) PRFiles(ctx context.Context, repo string, number int) (PRFileList, error) {
	return retryPRFiles(ctx, func() (PRFileList, error) { return g.prFilesOnce(ctx, repo, number) })
}

// PRFiles — файлы MR GitLab с одним повтором при расхождении счётчика.
func (g *GitLab) PRFiles(ctx context.Context, repo string, number int) (PRFileList, error) {
	return retryPRFiles(ctx, func() (PRFileList, error) { return g.prFilesOnce(ctx, repo, number) })
}

// prFilesMaxPages — GitHub отдаёт не больше 3000 файлов PR (30 страниц по
// 100), GitLab ограничен теми же порядками. Дальше список неполный.
const prFilesMaxPages = 30

func sortedUnique(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// PRFiles — файлы PR GitHub. Число записей сверяется с changed_files PR:
// постраничный список, который хостинг отдал не целиком, — отказ.
func (g *GitHub) prFilesOnce(ctx context.Context, repo string, number int) (PRFileList, error) {
	raw, code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, "")
	if err != nil {
		return PRFileList{}, err
	}
	if code != http.StatusOK {
		return PRFileList{}, fmt.Errorf("github PR: %d: %s", code, tailMsg(raw))
	}
	var pr struct {
		ChangedFiles int `json:"changed_files"`
		Head         struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return PRFileList{}, err
	}
	var paths []string
	entries := 0
	for page := 1; page <= prFilesMaxPages; page++ {
		raw, code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page), nil, "")
		if err != nil {
			return PRFileList{}, err
		}
		if code != http.StatusOK {
			return PRFileList{}, fmt.Errorf("github PR files: %d: %s", code, tailMsg(raw))
		}
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := json.Unmarshal(raw, &files); err != nil {
			return PRFileList{}, err
		}
		entries += len(files)
		for _, f := range files {
			paths = append(paths, f.Filename, f.PreviousFilename)
		}
		if len(files) < 100 {
			if entries != pr.ChangedFiles {
				return PRFileList{}, fmt.Errorf("github PR #%d: %w: файлов %d из %d", number, errCountMismatch, entries, pr.ChangedFiles)
			}
			return PRFileList{Files: sortedUnique(paths), HeadSHA: pr.Head.SHA}, nil
		}
	}
	return PRFileList{}, fmt.Errorf("github PR #%d: %w", number, ErrPRFilesTruncated)
}

// PRFiles — файлы MR GitLab. /diffs молча урезается diff limits (1000
// файлов или 50 000 строк по умолчанию) без признака overflow, поэтому
// число записей сверяется с changes_count MR: «1000+» или расхождение —
// неполный список (ревью Opus, круг 3).
func (g *GitLab) prFilesOnce(ctx context.Context, repo string, number int) (PRFileList, error) {
	raw, code, err := g.do(ctx, "GET", fmt.Sprintf("/projects/%s/merge_requests/%d", projectID(repo), number), nil)
	if err != nil {
		return PRFileList{}, err
	}
	if code != http.StatusOK {
		return PRFileList{}, fmt.Errorf("gitlab MR: %d: %s", code, tailMsg(raw))
	}
	var mr struct {
		SHA          string `json:"sha"`
		ChangesCount string `json:"changes_count"`
	}
	if err := json.Unmarshal(raw, &mr); err != nil {
		return PRFileList{}, err
	}
	count := strings.TrimSpace(mr.ChangesCount)
	if strings.HasSuffix(count, "+") {
		// «1000+»: GitLab сам говорит, что список неполный.
		return PRFileList{}, fmt.Errorf("gitlab MR !%d: %w: changes_count %q", number, ErrPRFilesTruncated, mr.ChangesCount)
	}
	want, err := strconv.Atoi(count)
	if err != nil {
		// Пустой счётчик у свежего MR заполняется асинхронно: это не
		// урезанный список, а временное состояние.
		return PRFileList{}, fmt.Errorf("gitlab MR !%d: %w: %q", number, errCountPending, mr.ChangesCount)
	}
	var paths []string
	entries := 0
	for page := 1; page <= prFilesMaxPages; page++ {
		raw, code, err := g.do(ctx, "GET",
			fmt.Sprintf("/projects/%s/merge_requests/%d/diffs?per_page=100&page=%d", projectID(repo), number, page), nil)
		if err != nil {
			return PRFileList{}, err
		}
		if code == http.StatusNotFound && page == 1 {
			files, err := g.prFilesChanges(ctx, repo, number, want)
			return PRFileList{Files: files, HeadSHA: mr.SHA}, err
		}
		if code != http.StatusOK {
			return PRFileList{}, fmt.Errorf("gitlab MR diffs: %d: %s", code, tailMsg(raw))
		}
		var files []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		}
		if err := json.Unmarshal(raw, &files); err != nil {
			return PRFileList{}, err
		}
		entries += len(files)
		for _, f := range files {
			paths = append(paths, f.OldPath, f.NewPath)
		}
		if len(files) < 100 {
			if entries != want {
				return PRFileList{}, fmt.Errorf("gitlab MR !%d: %w: файлов %d из %d", number, errCountMismatch, entries, want)
			}
			return PRFileList{Files: sortedUnique(paths), HeadSHA: mr.SHA}, nil
		}
	}
	return PRFileList{}, fmt.Errorf("gitlab MR !%d: %w", number, ErrPRFilesTruncated)
}

// prFilesChanges — старые инстансы без /diffs: /changes с признаком overflow.
func (g *GitLab) prFilesChanges(ctx context.Context, repo string, number, want int) ([]string, error) {
	raw, code, err := g.do(ctx, "GET",
		fmt.Sprintf("/projects/%s/merge_requests/%d/changes", projectID(repo), number), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("gitlab MR changes: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		Changes []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		} `json:"changes"`
		Overflow bool `json:"overflow"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Overflow {
		return nil, fmt.Errorf("gitlab MR !%d: %w", number, ErrPRFilesTruncated)
	}
	if len(out.Changes) != want {
		return nil, fmt.Errorf("gitlab MR !%d: %w: файлов %d из %d", number, errCountMismatch, len(out.Changes), want)
	}
	var paths []string
	for _, c := range out.Changes {
		paths = append(paths, c.OldPath, c.NewPath)
	}
	return sortedUnique(paths), nil
}

// PRFiles — в git-режиме настоящие файлы ветки PR относительно общего
// предка с базой и вершина ветки. Без git-режима репозитория нет: список
// задаётся SetPRFiles (тесты), иначе пуст.
func (f *Fake) PRFiles(ctx context.Context, repo string, number int) (PRFileList, error) {
	key := fmt.Sprintf("%s#%d", repo, number)
	f.mu.Lock()
	set, ok := f.prFiles[key]
	pr, known := f.prs[key]
	f.mu.Unlock()
	if ok {
		return PRFileList{Files: sortedUnique(set)}, nil
	}
	dir, inGit := f.repoDir(repo)
	if !inGit {
		return PRFileList{}, nil
	}
	pr, err := f.gitPR(ctx, dir, repo, number, pr, known)
	if err != nil {
		return PRFileList{}, err
	}
	base := pr.base
	if base == "" {
		base = "main"
	}
	if err := safeRef(base); err != nil {
		return PRFileList{}, err
	}
	if err := safeRef(pr.branch); err != nil {
		return PRFileList{}, err
	}
	head, ok, err := revParse(ctx, dir, "refs/heads/"+pr.branch+"^{commit}")
	if err != nil {
		return PRFileList{}, err
	}
	if !ok {
		return PRFileList{}, fmt.Errorf("fake git: ветка PR %s: %w", pr.branch, ErrBranchNotFound)
	}
	out, err := fakeGit(ctx, dir, "-c", "core.quotePath=false", "diff", "--no-renames", "--name-only", "-z",
		"refs/heads/"+base+"..."+head)
	if err != nil {
		return PRFileList{}, err
	}
	return PRFileList{Files: sortedUnique(strings.Split(out, "\x00")), HeadSHA: head}, nil
}

// gitPR — запись PR git-режима: из памяти или из ref'ов репозитория.
func (f *Fake) gitPR(ctx context.Context, dir, repo string, number int, pr fakePR, known bool) (fakePR, error) {
	if known {
		return pr, nil
	}
	pr, known, err := loadPR(ctx, dir, number)
	if err != nil {
		return fakePR{}, err
	}
	if !known {
		return fakePR{}, fmt.Errorf("fake git: PR #%d в %s неизвестен", number, repo)
	}
	return pr, nil
}

// MergeAt — merge в git-режиме, только если вершина ветки PR та же.
func (f *Fake) MergeAt(ctx context.Context, repo string, number int, headSHA string) (string, error) {
	if dir, inGit := f.repoDir(repo); inGit && headSHA != "" {
		key := fmt.Sprintf("%s#%d", repo, number)
		f.mu.Lock()
		pr, known := f.prs[key]
		f.mu.Unlock()
		pr, err := f.gitPR(ctx, dir, repo, number, pr, known)
		if err != nil {
			return "", err
		}
		if err := safeRef(pr.branch); err != nil {
			return "", err
		}
		tip, ok, err := revParse(ctx, dir, "refs/heads/"+pr.branch+"^{commit}")
		if err != nil {
			return "", err
		}
		if !ok || tip != headSHA {
			return "", fmt.Errorf("fake git merge: %w", ErrHeadMoved)
		}
	}
	return f.Merge(ctx, repo, number)
}

// SetPRFiles задаёт список файлов PR fake-адаптера (тесты без git-режима).
func (f *Fake) SetPRFiles(repo string, number int, files []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prFiles == nil {
		f.prFiles = map[string][]string{}
	}
	f.prFiles[fmt.Sprintf("%s#%d", repo, number)] = files
}
