package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Конвейер публикаций Deploy → Verify (спека backend/deployment,
// design implement-deployment): детерминированные реакции, CAS-переходы
// в БД, откат одной попыткой, пауза автопубликаций и эскалация при провале.

const (
	// deployTimeout — дедлайн джобы на runner'е; watchdog оркестратора
	// добавляет запас (результат мог потеряться вместе с runner'ом).
	deployTimeout       = 30 * time.Minute
	deployWatchdogGrace = 5 * time.Minute
)

// FailDeploymentNow — внешняя точка провала публикации без отката
// (reconnect runner'а: его деплой-goroutine мертва, результата не будет).
func (e *Engine) FailDeploymentNow(ctx context.Context, depID, detail string) error {
	return e.failDeployNow(ctx, depID, "", detail)
}

// tickDeployments — планирование публикаций и watchdog (вызывается из Tick).
func (e *Engine) tickDeployments(ctx context.Context) error {
	timedOut, err := e.St.TimedOutDeployments(ctx, deployTimeout+deployWatchdogGrace)
	if err != nil {
		return err
	}
	for _, depID := range timedOut {
		if err := e.failDeployNow(ctx, depID, "", "таймаут публикации: runner не вернул результат"); err != nil {
			slog.Error("deploy watchdog", "deployment", depID, "err", err)
		}
	}
	for {
		a, ok, err := e.St.StartNextDeployment(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e.dispatchDeploy(a, a.Deployment.Version, "", false)
	}
	// Окружения с внешней доставкой идут своим путём: runner им не нужен.
	return e.tickExternalDeployments(ctx)
}

// dispatchDeploy отправляет деплой-джобу runner'у публикации.
// Недоступный runner доведёт watchdog (как heartbeat у задач).
func (e *Engine) dispatchDeploy(a store.DeployAssignment, version, prevVersion string, rollback bool) {
	e.mu.Lock()
	e.deployOwner[a.Deployment.ID] = a.RunnerID
	e.mu.Unlock()
	// У k8s-окружения команды собирает control plane из параметров
	// (спека deployment «Публикация в Kubernetes»); у Linux-хоста они
	// приходят из конфигурации как есть.
	deployCmd, verifyCmd := a.Env.Config.DeployCmd, a.Env.Config.VerifyCmd
	checkout, repoURL, gitToken := false, "", ""
	if a.Env.ExecType == domain.ExecK8s {
		// Манифесты и чарт лежат в репозитории: команды исполняются в
		// рабочей копии на публикуемой версии (протокол v10).
		deployCmd, verifyCmd = k8sJob(a.Env.Config, version)
		checkout = true
		repoURL, gitToken = e.deployRepoAccess(a)
	}
	msg := &pb.PlaneMsg{
		MsgId: fmt.Sprintf("deploy-%s-%v-%d", a.Deployment.ID, rollback, time.Now().UnixNano()),
		Kind: &pb.PlaneMsg_Deploy{Deploy: &pb.DeployJob{
			DeploymentId: a.Deployment.ID, EnvName: a.Env.Name, Repo: a.Repo,
			Version: version, PrevVersion: prevVersion, Rollback: rollback,
			Host: a.Env.Config.Host, DeployCmd: deployCmd,
			VerifyCmd: verifyCmd, VerifyUrl: a.Env.Config.VerifyURL,
			TimeoutS: int32(deployTimeout.Seconds()),
			Checkout: checkout, RepoUrl: repoURL, GitToken: gitToken,
		}},
	}
	if !e.Out.Send(a.RunnerID, msg) {
		slog.Warn("deploy dispatch: runner недоступен, публикацию доведёт watchdog",
			"runner", a.RunnerID, "deployment", a.Deployment.ID)
	}
}

// DeployMatches — принадлежит ли сообщение активной публикации этого
// runner'а. Кеш в памяти; после рестарта rivetd — подъём из БД
// (паттерн SessionMatches).
func (e *Engine) DeployMatches(ctx context.Context, depID, runnerID string) bool {
	if depID == "" || runnerID == "" {
		return false
	}
	e.mu.Lock()
	owner, known := e.deployOwner[depID]
	e.mu.Unlock()
	if known {
		return owner == runnerID
	}
	ok, err := e.St.DeploymentOwned(ctx, depID, runnerID)
	if err != nil || !ok {
		return false
	}
	e.mu.Lock()
	if owner, known := e.deployOwner[depID]; known {
		ok = owner == runnerID
	} else {
		e.deployOwner[depID] = runnerID
	}
	e.mu.Unlock()
	return ok
}

// OnDeployTranscript накапливает замаскированный лог публикации (кап как у
// транскриптов задач); принадлежность проверяет вызывающий (DeployMatches).
func (e *Engine) OnDeployTranscript(depID string, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	buf := e.deployLogs[depID]
	if len(buf) >= transcriptCap {
		return
	}
	if len(buf)+len(data) > transcriptCap {
		keep := transcriptCap - len(truncMarker)
		if len(buf) > keep {
			buf = buf[:keep]
		} else {
			buf = append(buf, data[:keep-len(buf)]...)
		}
		e.deployLogs[depID] = append(buf, truncMarker...)
		return
	}
	e.deployLogs[depID] = append(buf, data...)
}

// OnDeployResult — реакции на итог этапа публикации. Все переходы — CAS
// в БД с проверкой владельца: stale/чужой/повторный результат не проходит.
func (e *Engine) OnDeployResult(ctx context.Context, runnerID string, dr *pb.DeployResult) error {
	if !e.DeployMatches(ctx, dr.DeploymentId, runnerID) {
		slog.Warn("deploy result чужой публикации отброшен",
			"deployment", dr.DeploymentId, "runner", runnerID, "stage", dr.Stage)
		return nil
	}
	dr.Detail = redact.String(dr.Detail)
	// Фаза отката — durable флаг в БД (память не переживает рестарт rivetd:
	// результат отката после рестарта не должен записать done проваленной версии).
	dep, err := e.St.GetDeployment(ctx, dr.DeploymentId)
	if err != nil {
		return err
	}
	// Runner эхом возвращает фазу джобы: повтор исходного провала после
	// перехода в откат (и наоборот) не смешивается с результатами отката.
	if dr.Rollback != dep.Rollback {
		slog.Warn("deploy result чужой фазы отброшен",
			"deployment", dr.DeploymentId, "result_rollback", dr.Rollback, "dep_rollback", dep.Rollback)
		return nil
	}

	switch {
	case dr.Stage == pb.DeployResult_DEPLOY && dr.Ok:
		if dep.Rollback {
			return nil // промежуточный этап отката — ждём его Verify
		}
		ok, err := e.St.DeployStageVerifying(ctx, dr.DeploymentId, runnerID)
		if err != nil || !ok {
			return err
		}
		return e.deployEvent(ctx, dr.DeploymentId, "verifying", "доставка выполнена — проверка окружения")

	case dr.Stage == pb.DeployResult_VERIFY && dr.Ok:
		if dep.Rollback {
			// Откат доставлен и проверен: публикация провалена, окружение
			// вернулось к предыдущей версии. Причина провала уже в detail
			// (записана при переходе в откат), финал только дописывает итог.
			return e.finishDeploy(ctx, dr.DeploymentId, runnerID, "rolled_back",
				"откат к предыдущей версии выполнен")
		}
		return e.finishDeploy(ctx, dr.DeploymentId, runnerID, "done", "")

	default: // провал этапа (deploy или verify)
		if dep.Rollback {
			return e.failDeploy(ctx, dr.DeploymentId, runnerID,
				"откат тоже провалился: "+dr.Detail, false)
		}
		return e.failDeploy(ctx, dr.DeploymentId, runnerID, dr.Detail, true)
	}
}

// finishDeploy — успешный финал (done) или завершение отката (rolled_back):
// лог в blob, затем атомарная транзакция store (финал + событие + runner
// (+ пауза и эскалация для rolled_back)). Ошибка уходит без Ack — повтор
// результата восстановит цепочку целиком.
func (e *Engine) finishDeploy(ctx context.Context, depID, runnerID, status, detail string) error {
	e.flushDeployLog(ctx, depID)
	var done bool
	var err error
	if status == "done" {
		done, err = e.St.CompleteDeployment(ctx, depID, runnerID)
	} else {
		// rolled_back — это провал с успешным откатом: полная цепочка
		// провала (пауза, эскалация), статус rolled_back.
		done, err = e.St.FailDeployment(ctx, depID, runnerID, status, detail)
	}
	if err != nil {
		return err
	}
	e.dropDeploy(depID)
	if !done {
		slog.Warn("финал публикации уже выполнен другим путём", "deployment", depID, "status", status)
	}
	return nil
}

// failDeploy — провал этапа: одна попытка отката (если есть куда), иначе
// полная цепочка провала со статусом failed.
func (e *Engine) failDeploy(ctx context.Context, depID, runnerID, detail string, tryRollback bool) error {
	if tryRollback {
		projectID, envID, _, _, err := e.St.DeploymentRefs(ctx, depID)
		if err != nil {
			return err
		}
		prev, err := e.St.LastSuccessfulVersion(ctx, envID, depID)
		if err != nil {
			return err
		}
		if prev != "" {
			// Durable переход в откат: CAS от владельца, причина — в detail.
			claimed, err := e.St.MarkDeploymentRollingBack(ctx, depID, runnerID, "провал: "+detail)
			if err != nil {
				return err
			}
			dep, err := e.St.GetDeployment(ctx, depID)
			if err != nil {
				return err
			}
			if !claimed {
				// Переход уже был: финализированную публикацию не трогаем,
				// а начатый откат продолжаем — повторная отправка джобы
				// идемпотентна (деплой идемпотентен по версии). Закрывает
				// сбой между записью фазы и отправкой джобы.
				if dep.Ended != nil || !dep.Rollback {
					return nil
				}
			}
			env, err := e.St.GetEnvironment(ctx, envID)
			if err != nil {
				return err
			}
			var repo string
			if p, perr := e.St.GetProject(ctx, projectID); perr == nil {
				repo = p.Repo()
			}
			if err := e.deployEvent(ctx, depID, "rolling_back",
				"этап провалился — откат к версии "+prev); err != nil {
				return err
			}
			e.dispatchDeploy(store.DeployAssignment{
				Deployment: dep, Env: env, ProjectID: projectID, Repo: repo, RunnerID: runnerID,
			}, prev, dep.Version, true)
			return nil
		}
	}
	return e.failDeployNow(ctx, depID, runnerID, detail)
}

// failDeployNow — полная цепочка провала без отката (некуда, watchdog,
// потеря runner'а): лог в blob, финал failed, пауза, эскалация. Ошибка
// уходит вызывающему: недоставленная fail-цепочка результата runner'а не
// должна получить Ack (runner повторит сообщение).
func (e *Engine) failDeployNow(ctx context.Context, depID, runnerID, detail string) error {
	e.flushDeployLog(ctx, depID)
	done, err := e.St.FailDeployment(ctx, depID, runnerID, "failed", detail)
	if err != nil {
		return fmt.Errorf("fail deployment %s: %w", depID, err)
	}
	if !done {
		slog.Warn("публикация уже финализирована", "deployment", depID)
	}
	e.dropDeploy(depID)
	return nil
}

// flushDeployLog уводит накопленный лог публикации в blob (лог уже
// замаскирован построчным редактором на входе потока).
func (e *Engine) flushDeployLog(ctx context.Context, depID string) {
	e.mu.Lock()
	buf := e.deployLogs[depID]
	delete(e.deployLogs, depID)
	e.mu.Unlock()
	if len(buf) == 0 || e.Blob == nil {
		return
	}
	_, _, envName, _, err := e.St.DeploymentRefs(ctx, depID)
	if err != nil {
		slog.Error("deploy log refs", "deployment", depID, "err", err)
		return
	}
	key := fmt.Sprintf("deploys/%s/%s.log", envName, depID)
	ref, err := e.Blob.Put(ctx, key, redact.Bytes(buf))
	if err != nil {
		slog.Error("deploy log flush", "deployment", depID, "err", err)
		return
	}
	if err := e.St.SetDeploymentLog(ctx, depID, ref); err != nil {
		slog.Error("deploy log ref", "deployment", depID, "err", err)
	}
}

// dropDeploy чистит память публикации.
func (e *Engine) dropDeploy(depID string) {
	e.mu.Lock()
	delete(e.deployLogs, depID)
	delete(e.deployOwner, depID)
	e.mu.Unlock()
}

// deployEvent — событие deploy.status с payload для SSE.
func (e *Engine) deployEvent(ctx context.Context, depID, status, text string) error {
	projectID, envID, envName, version, err := e.St.DeploymentRefs(ctx, depID)
	if err != nil {
		return err
	}
	_, err = e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: "deploy.status", ProjectID: projectID,
		Text: fmt.Sprintf("публикация %s: %s", envName, text),
		Payload: map[string]any{"environment_id": envID, "deployment_id": depID,
			"status": status, "version": version},
	})
	return err
}

// deployRepoAccess — адрес клонирования и токен доступа для рабочей копии
// деплоя. Ошибка не валит джобу: без адреса runner склонирует по своему
// RIVET_GIT_BASE (стенды), а приватный репозиторий провалится понятной
// ошибкой git.
func (e *Engine) deployRepoAccess(a store.DeployAssignment) (repoURL, gitToken string) {
	ctx := context.Background()
	p, err := e.St.GetProject(ctx, a.ProjectID)
	if err != nil {
		slog.Error("деплой: проект", "project", a.ProjectID, "err", err)
		return "", ""
	}
	if p.Provider == string(scm.ProviderFake) {
		return "", "" // e2e-стенд клонирует по RIVET_GIT_BASE
	}
	token, err := e.St.ProjectToken(ctx, p.ID, e.Box)
	if err != nil {
		slog.Error("деплой: учётные данные проекта", "project", p.ID, "err", err)
		return p.WebURL(), ""
	}
	return p.WebURL(), token
}
