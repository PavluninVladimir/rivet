package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Публикация через GitOps (change add-gitops-delivery, спека deployment
// «Дирижирование внешними системами доставки»): этап Deploy — коммит
// публикуемой версии в репозиторий конфигурации, этап Verify — ожидание,
// пока окружение не начнёт отвечать этой версией. Выкат делает контроллер
// кластера, Rivet ему не мешает и ничего не гадает про сроки.

// gitopsCommit вносит версию в файл конфигурации. Повторный коммит той же
// версии не создаётся: история репозитория не должна забиваться пустыми
// изменениями.
func (e *Engine) gitopsCommit(ctx context.Context, a store.DeployAssignment, version string) (scm.Commit, bool, error) {
	adapter, repo, ref, err := e.gitopsTarget(ctx, a)
	if err != nil {
		return scm.Commit{}, false, err
	}
	cfg := a.Env.Config
	cur, err := adapter.ReadFile(ctx, repo, ref, cfg.File)
	if err != nil && !errors.Is(err, scm.ErrFileNotFound) {
		return scm.Commit{}, false, err
	}
	next, err := applyVersion(cur.Content, cfg.Key, version)
	if err != nil {
		return scm.Commit{}, false, err
	}
	if err == nil && cur.Content == next {
		return scm.Commit{}, false, nil // версия уже в файле
	}
	msg := fmt.Sprintf("%s: версия %s (публикация Rivet)", a.Env.Name, version)
	commit, err := adapter.WriteFile(ctx, repo, ref, cfg.File, cur.FileID, next, msg)
	if err != nil {
		return scm.Commit{}, false, err
	}
	return commit, true, nil
}

// gitopsTarget — адаптер хостинга, репозиторий конфигурации и ветка
// коммита. Пустой репозиторий в конфигурации означает репозиторий проекта.
func (e *Engine) gitopsTarget(ctx context.Context, a store.DeployAssignment) (scm.Adapter, string, string, error) {
	p, err := e.St.GetProject(ctx, a.ProjectID)
	if err != nil {
		return nil, "", "", err
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return nil, "", "", err
	}
	repo := a.Env.Config.Repo
	if repo == "" {
		repo = p.Repo()
	}
	ref := a.Env.Config.Ref
	if ref == "" {
		ref = p.DefaultBranch
	}
	if ref == "" {
		ref = e.BaseBranch
	}
	return adapter, repo, ref, nil
}

// applyVersion подставляет версию: без ключа — файл целиком, с ключом —
// значение этого ключа YAML. Отступ вложенности берётся из самого файла
// (2 или 4 пробела — как принято в проекте), инлайн-комментарий и
// остальные строки сохраняются.
func applyVersion(content, key, version string) (string, error) {
	if key == "" {
		return version + "\n", nil
	}
	lines := strings.Split(content, "\n")
	segments := strings.Split(key, ".")
	// parent — отступ родительского ключа; ищем строго глубже него.
	parent, idx, last := -1, 0, len(lines)
	for si, seg := range segments {
		found := -1
		for i := idx; i < last; i++ {
			line := lines[i]
			trimmed := strings.TrimLeft(line, " \t")
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			indent := len(line) - len(trimmed)
			if si > 0 && indent <= parent {
				break // ветка родителя закончилась
			}
			if !strings.HasPrefix(trimmed, seg+":") {
				continue
			}
			if si == 0 && indent != 0 {
				continue // ключ верхнего уровня ищем только на нулевом отступе
			}
			found = i
			break
		}
		if found < 0 {
			return "", fmt.Errorf("ключ %q не найден в файле конфигурации", key)
		}
		if si == len(segments)-1 {
			lines[found] = replaceValue(lines[found], seg, version)
			return strings.Join(lines, "\n"), nil
		}
		line := lines[found]
		parent = len(line) - len(strings.TrimLeft(line, " \t"))
		idx = found + 1
	}
	return "", fmt.Errorf("ключ %q не найден в файле конфигурации", key)
}

// valueLineRe — строка «ключ: значение» с необязательным инлайн-комментарием.
var valueLineRe = regexp.MustCompile(`^(\s*[^:]+:[ \t]*)([^#]*?)([ \t]*#.*)?$`)

// replaceValue меняет только значение ключа, сохраняя отступ и комментарий.
func replaceValue(line, key, version string) string {
	m := valueLineRe.FindStringSubmatch(line)
	if m == nil {
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		return indent + key + ": " + version
	}
	return m[1] + version + m[3]
}

// gitopsSynced — окружение отвечает публикуемой версией: факт, а не
// таймер. Проверяется тем же адресом, что и остальные Verify без runner'а,
// но 2xx недостаточно: старая версия отвечает не хуже новой.
func gitopsSynced(ctx context.Context, rawURL, version string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := verifyClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, nil
	}
	return strings.Contains(string(body), version), nil
}

// startGitOps — этап Deploy: коммит версии и событие со ссылкой на него.
func (e *Engine) startGitOps(ctx context.Context, a store.DeployAssignment) {
	claimed, err := e.St.ClaimExternalTrigger(ctx, a.Deployment.ID)
	if err != nil {
		slog.Error("публикация GitOps: захват коммита", "deployment", a.Deployment.ID, "err", err)
		return
	}
	if !claimed {
		return
	}
	version := a.Deployment.Version
	if a.Deployment.Rollback {
		prev, err := e.St.LastSuccessfulVersion(ctx, a.Env.ID, a.Deployment.ID)
		if err != nil {
			slog.Error("публикация GitOps: предыдущая версия", "deployment", a.Deployment.ID, "err", err)
			return
		}
		if prev == "" {
			if err := e.failDeployNow(ctx, a.Deployment.ID, "", "откат невозможен: успешных версий не было"); err != nil {
				slog.Error("публикация GitOps: провал отката", "deployment", a.Deployment.ID, "err", err)
			}
			return
		}
		version = prev
	}
	commit, committed, err := e.gitopsCommit(ctx, a, version)
	if err != nil {
		e.failExternal(ctx, a, "коммит версии: "+err.Error())
		return
	}
	text := "версия " + version + " уже в конфигурации — ожидание синхронизации"
	if committed {
		text = "версия " + version + " закоммичена" + urlSuffix(commit.URL)
	}
	runID := commit.SHA
	if runID == "" {
		runID = store.ExternalRunPending
	}
	if _, err := e.St.SetDeploymentExternalRun(ctx, a.Deployment.ID,
		store.ExternalRunPending, runID, commit.URL); err != nil {
		slog.Error("публикация GitOps: запись коммита", "deployment", a.Deployment.ID, "err", err)
		return
	}
	if err := e.deployEventWithRun(ctx, a, "deploying", text, commit.URL); err != nil {
		slog.Error("публикация GitOps: событие коммита", "deployment", a.Deployment.ID, "err", err)
	}
}

// pollGitOps — этап Verify: ждём, пока окружение начнёт отвечать версией.
// Провал по дедлайну доводит общий watchdog внешних публикаций.
func (e *Engine) pollGitOps(ctx context.Context, a store.DeployAssignment) error {
	version := a.Deployment.Version
	if a.Deployment.Rollback {
		prev, err := e.St.LastSuccessfulVersion(ctx, a.Env.ID, a.Deployment.ID)
		if err != nil {
			return err
		}
		version = prev
	}
	// Публикация помечена «коммит захвачен», но ссылки на него нет: rivetd
	// мог упасть между захватом и записью файла. Коммит идемпотентен по
	// содержимому, поэтому повторяем его, а не ждём таймаута впустую.
	if a.Deployment.ExternalRunID == store.ExternalRunPending && a.Deployment.ExternalURL == "" {
		commit, committed, err := e.gitopsCommit(ctx, a, version)
		if err != nil {
			e.failExternal(ctx, a, "коммит версии: "+err.Error())
			return nil
		}
		if committed {
			if _, err := e.St.SetDeploymentExternalRun(ctx, a.Deployment.ID,
				store.ExternalRunPending, commit.SHA, commit.URL); err != nil {
				return err
			}
			if err := e.deployEventWithRun(ctx, a, "deploying",
				"версия "+version+" закоммичена"+urlSuffix(commit.URL), commit.URL); err != nil {
				return err
			}
		}
	}
	synced, err := gitopsSynced(ctx, a.Env.Config.VerifyURL, version)
	if err != nil {
		// Разовая ошибка опроса — не провал: окружение может ещё
		// перезапускаться, а дедлайн держит watchdog.
		slog.Debug("публикация GitOps: опрос окружения", "deployment", a.Deployment.ID, "err", err)
		return nil
	}
	if !synced {
		return nil
	}
	if a.Deployment.Rollback {
		return e.finishDeploy(ctx, a.Deployment.ID, "", "rolled_back",
			"откат к предыдущей версии выполнен")
	}
	ok, err := e.St.DeployStageVerifying(ctx, a.Deployment.ID, "")
	if err != nil {
		return err
	}
	if ok {
		if err := e.deployEventWithRun(ctx, a, "verifying",
			"окружение синхронизировалось", a.Deployment.ExternalURL); err != nil {
			return err
		}
	}
	return e.finishDeploy(ctx, a.Deployment.ID, "", "done", "")
}

// gitopsEnv — окружение публикуется коммитом в конфигурацию.
func gitopsEnv(env domain.Environment) bool { return env.ExecType == domain.ExecGitOps }
