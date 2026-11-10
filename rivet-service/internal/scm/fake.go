package scm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Fake — SCM-провайдер для e2e и локальных стендов (RIVET_SCM=fake).
// PR фиктивный, merge — no-op. Продуктовое поведение не меняет:
// включается только явным флагом окружения.
//
// С каталогом bare-репозиториев стенда (FakeGitDirEnv) и списком
// репозиториев (FakeGitReposEnv) fake работает поверх настоящего git для
// репозиториев из списка: ReadFile читает файлы версии, WriteFile коммитит
// в ветку, HeadSHA отдаёт настоящий SHA, Merge продвигает базовую ветку.
// Остальные репозитории работают по-старому: e2e-проекты делят общий
// e2e/demo и ждут версию «main», настоящий merge в нём ломал бы их
// сценарии.
type Fake struct {
	seq atomic.Int64
	// files — файлы фиктивного репозитория для публикации GitOps вне
	// git-режима.
	mu    sync.Mutex
	files map[string]string
	// conflicts — PR с меткой FakeConflictMarker («repo#номер»), первый
	// merge которых отклоняется как конфликт с базовой веткой.
	conflicts map[string]bool
	// gitDir — каталог bare-репозиториев стенда: <gitDir>/<owner>/<name>.git.
	gitDir string
	// gitRepos — репозитории («owner/name»), для которых действует git-режим.
	gitRepos map[string]bool
	// prs — ветка и база PR («repo#номер») для merge в git-режиме.
	prs map[string]fakePR
	// prFiles — заданный тестом список файлов PR («repo#номер»).
	prFiles map[string][]string
	// gitMu — изменения git-режима по одному (создание PR, merge, запись
	// файла): проверка состояния и сдвиг ref'а должны видеть одно
	// состояние.
	gitMu sync.Mutex
}

type fakePR struct{ branch, base string }

// FakeConflictMarker — метка в заголовке задачи, по которой fake-хостинг
// имитирует конфликт PR с базовой веткой: первая попытка merge получает
// ErrMergeConflict, следующая проходит (e2e конфликта при merge).
const FakeConflictMarker = "[e2e-conflict]"

// FakeGitDirEnv — переменная окружения с каталогом bare-репозиториев
// стенда для git-режима fake (e2e снимка архитектуры).
const FakeGitDirEnv = "RIVET_FAKE_GIT_DIR"

// FakeGitReposEnv — репозитории git-режима через запятую («owner/name,…»).
// Без списка git-режим не действует ни для одного репозитория.
const FakeGitReposEnv = "RIVET_FAKE_GIT_REPOS"

// NewFake — fake-адаптер; git-режим включают переменные FakeGitDirEnv и
// FakeGitReposEnv.
func NewFake() *Fake {
	return NewFakeGit(os.Getenv(FakeGitDirEnv), strings.Split(os.Getenv(FakeGitReposEnv), ",")...)
}

// NewFakeGit — fake-адаптер в git-режиме для репозиториев repos из
// каталога bare-репозиториев dir.
func NewFakeGit(dir string, repos ...string) *Fake {
	f := &Fake{gitDir: dir, gitRepos: map[string]bool{}}
	for _, r := range repos {
		if r = strings.TrimSpace(r); r != "" {
			f.gitRepos[r] = true
		}
	}
	return f
}

func (f *Fake) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	dir, inGit := f.repoDir(repo)
	var n int
	if inGit {
		var err error
		if n, err = f.gitCreatePR(ctx, dir, fakePR{branch: branch, base: base}); err != nil {
			return PR{}, err
		}
	} else {
		n = int(f.seq.Add(1))
	}
	key := fmt.Sprintf("%s#%d", repo, n)
	f.mu.Lock()
	if strings.Contains(title, FakeConflictMarker) {
		if f.conflicts == nil {
			f.conflicts = map[string]bool{}
		}
		f.conflicts[key] = true
	}
	if inGit {
		if f.prs == nil {
			f.prs = map[string]fakePR{}
		}
		f.prs[key] = fakePR{branch: branch, base: base}
	}
	f.mu.Unlock()
	return PR{Number: n, URL: fmt.Sprintf("https://fake.local/%s/pull/%d", repo, n)}, nil
}

// gitCreatePR — номер больше и сохранённых в репозитории (прошлые запуски
// rivetd), и выданных этим адаптером; запись PR в ref'ы репозитория.
func (f *Fake) gitCreatePR(ctx context.Context, dir string, pr fakePR) (int, error) {
	f.gitMu.Lock()
	defer f.gitMu.Unlock()
	stored, err := maxPR(ctx, dir)
	if err != nil {
		return 0, err
	}
	n, err := savePR(ctx, dir, int(max(f.seq.Load(), int64(stored)))+1, pr)
	if err != nil {
		return 0, err
	}
	f.raiseSeq(int64(n))
	return n, nil
}

// raiseSeq — счётчик адаптера не отстаёт от занятого номера PR: его делят
// PR, пайплайны и коммиты fake.
func (f *Fake) raiseSeq(n int64) {
	for {
		cur := f.seq.Load()
		if cur >= n || f.seq.CompareAndSwap(cur, n) {
			return
		}
	}
}

func (f *Fake) Diff(ctx context.Context, repo string, number int) (string, error) {
	return "diff --git a/e2e b/e2e\n+e2e", nil
}

// Merge — без git-режима no-op: настоящего merge-коммита нет, поэтому
// версией служит базовая ветка. Она существует в репозитории стенда, и
// доставка, которой нужна рабочая копия версии (Kubernetes), на стенде
// работает. В git-режиме базовая ветка продвигается по-настоящему (gitMerge).
func (f *Fake) Merge(ctx context.Context, repo string, number int) (string, error) {
	key := fmt.Sprintf("%s#%d", repo, number)
	f.mu.Lock()
	conflict := f.conflicts[key]
	delete(f.conflicts, key)
	pr, known := f.prs[key]
	f.mu.Unlock()
	if conflict {
		return "", fmt.Errorf("fake merge: %w", ErrMergeConflict)
	}
	if dir, inGit := f.repoDir(repo); inGit {
		// PR прошлого запуска rivetd в памяти нет: он читается из ref'ов
		// репозитория. Неизвестный PR — ошибка, иначе задача ушла бы в done
		// без merge, а публикация выкатила бы старый код.
		if !known {
			var err error
			if pr, known, err = loadPR(ctx, dir, number); err != nil {
				return "", err
			}
			if !known {
				return "", fmt.Errorf("fake git: PR #%d в %s неизвестен", number, repo)
			}
		}
		return f.gitMerge(ctx, dir, number, pr)
	}
	return "main", nil
}

// HeadSHA — без git-режима «вершина» ветки: для стенда это сама ветка
// (см. Merge). В git-режиме настоящий SHA, пустая ветка — HEAD репозитория.
func (f *Fake) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	if dir, ok := f.repoDir(repo); ok {
		return f.gitHead(ctx, dir, branch)
	}
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
// адаптера, чего достаточно для сквозной проверки публикации GitOps. В
// git-режиме файл читается из версии ref bare-репозитория стенда.
func (f *Fake) ReadFile(ctx context.Context, repo, ref, path string) (FileContent, error) {
	if dir, inGit := f.repoDir(repo); inGit {
		return f.gitRead(ctx, dir, repo, ref, path)
	}
	f.mu.Lock()
	content, ok := f.files[repo+"@"+ref+":"+path]
	f.mu.Unlock()
	if ok {
		return FileContent{Content: content, FileID: "fake-file"}, nil
	}
	return FileContent{}, ErrFileNotFound
}

// WriteFile — «коммит» в фиктивный репозиторий: как и настоящие хостинги,
// проверяет идентификатор прочитанной версии файла. Записанное живёт в
// памяти. В git-режиме запись — настоящий коммит в ветку bare-репозитория
// (gitWrite): окружение GitOps e2e-стенда читает версию из git, как
// GitOps-контроллер.
func (f *Fake) WriteFile(ctx context.Context, repo, ref, path, prevID, content, message string) (Commit, error) {
	if dir, inGit := f.repoDir(repo); inGit {
		return f.gitWrite(ctx, dir, repo, ref, path, prevID, content, message)
	}
	key := repo + "@" + ref + ":" + path
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string]string{}
	}
	// Новый файл пишется без версии, существующий — с версией из ReadFile.
	_, exists := f.files[key]
	if exists == (prevID == "") {
		return Commit{}, errFakeStale
	}
	f.files[key] = content
	sha := fmt.Sprintf("fake-commit-%04d", f.seq.Add(1))
	return Commit{SHA: sha, URL: fakeCommitURL(repo, sha)}, nil
}

// errFakeStale — запись поверх версии файла, которую не читали.
var errFakeStale = errors.New("fake: файл изменился с момента чтения")

func fakeCommitURL(repo, sha string) string {
	return fmt.Sprintf("https://fake.local/%s/commit/%s", repo, sha)
}

// BranchProtected — на стенде ветка считается защищённой: настоящих
// правил хостинга у fake нет, а сценарии проверяют логику Rivet.
func (f *Fake) BranchProtected(ctx context.Context, repo, branch string) (bool, error) {
	return true, nil
}
