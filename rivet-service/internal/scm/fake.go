package scm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Fake — SCM-провайдер для e2e и локальных стендов (RIVET_SCM=fake).
// PR фиктивный, merge — no-op. Продуктовое поведение не меняет:
// включается только явным флагом окружения.
type Fake struct {
	seq atomic.Int64
	// files — файлы фиктивного репозитория для публикации GitOps.
	mu    sync.Mutex
	files map[string]string
}

func NewFake() *Fake { return &Fake{} }

func (f *Fake) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	n := int(f.seq.Add(1))
	return PR{Number: n, URL: fmt.Sprintf("https://fake.local/%s/pull/%d", repo, n)}, nil
}

func (f *Fake) Diff(ctx context.Context, repo string, number int) (string, error) {
	return "diff --git a/e2e b/e2e\n+e2e", nil
}

// Merge — no-op: настоящего merge-коммита нет, поэтому версией служит
// базовая ветка. Она существует в репозитории стенда, и доставка, которой
// нужна рабочая копия версии (Kubernetes), на стенде работает.
func (f *Fake) Merge(ctx context.Context, repo string, number int) (string, error) {
	return "main", nil
}

// HeadSHA — «вершина» ветки: для стенда это сама ветка (см. Merge).
func (f *Fake) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	if branch == "" {
		return "main", nil
	}
	return branch, nil
}

// Probe в fake-режиме всегда успешен: стенд не ходит на хостинг.
func (f *Fake) Probe(ctx context.Context, repo string) ProbeResult {
	if repo == "" {
		return ProbeResult{OK: true, TokenOwner: "e2e-bot"}
	}
	return ProbeResult{
		OK: true, TokenOwner: "e2e-bot", RepoPath: repo, DefaultBranch: "main",
		CanPush: true, CanMergeRequest: true,
	}
}

// CreateRepo возвращает репозиторий так, будто он создан на хостинге:
// физический bare-репозиторий стенда готовит e2e-скрипт.
func (f *Fake) CreateRepo(ctx context.Context, in NewRepo) (RepoInfo, error) {
	owner := in.Owner
	if owner == "" {
		owner = "e2e"
	}
	path := owner + "/" + in.Name
	return RepoInfo{Path: path, WebURL: "https://fake.local/" + path, DefaultBranch: "main"}, nil
}

// RegisterWebhook в fake-режиме подписки не делает: стенд шлёт события сам.
func (f *Fake) RegisterWebhook(ctx context.Context, repo, url, secret string) (bool, error) {
	return false, nil
}

// TriggerPipeline — фиктивный прогон пайплайна: сразу известен и сразу
// успешен (e2e-стенд и отладка внешней доставки без хостинга).
func (f *Fake) TriggerPipeline(ctx context.Context, repo, pipeline, ref string, vars map[string]string) (PipelineRun, error) {
	n := f.seq.Add(1)
	return PipelineRun{
		RunID: fmt.Sprintf("%d", n),
		URL:   fmt.Sprintf("https://fake.local/%s/pipelines/%d", repo, n),
		State: PipelineRunning,
	}, nil
}

// PipelineRun — фиктивный прогон завершается успехом при первом же опросе.
func (f *Fake) PipelineRun(ctx context.Context, repo, pipeline, ref, runID string, _ time.Time) (PipelineRun, error) {
	return PipelineRun{
		RunID: runID,
		URL:   fmt.Sprintf("https://fake.local/%s/pipelines/%s", repo, runID),
		State: PipelineSuccess,
	}, nil
}

// ─── файлы репозитория (e2e-стенд) ───────────────────────────────────────

// ReadFile — файл фиктивного репозитория: содержимое живёт в памяти
// адаптера, чего достаточно для сквозной проверки публикации GitOps.
func (f *Fake) ReadFile(ctx context.Context, repo, ref, path string) (FileContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[repo+"@"+ref+":"+path]
	if !ok {
		return FileContent{}, ErrFileNotFound
	}
	return FileContent{Content: content, FileID: "fake-file"}, nil
}

// WriteFile — «коммит» в фиктивный репозиторий: как и настоящие хостинги,
// проверяет идентификатор прочитанной версии файла.
func (f *Fake) WriteFile(ctx context.Context, repo, ref, path, prevID, content, message string) (Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string]string{}
	}
	key := repo + "@" + ref + ":" + path
	_, exists := f.files[key]
	if exists != (prevID != "") {
		return Commit{}, fmt.Errorf("fake: файл изменился с момента чтения")
	}
	f.files[key] = content
	sha := fmt.Sprintf("fake-commit-%04d", f.seq.Add(1))
	return Commit{SHA: sha, URL: fmt.Sprintf("https://fake.local/%s/commit/%s", repo, sha)}, nil
}

// BranchProtected — на стенде ветка считается защищённой: настоящих
// правил хостинга у fake нет, а сценарии проверяют логику Rivet.
func (f *Fake) BranchProtected(ctx context.Context, repo, branch string) (bool, error) {
	return true, nil
}
