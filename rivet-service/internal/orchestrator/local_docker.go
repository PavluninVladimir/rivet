package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func (e *Engine) dispatchLocalDocker(a store.DeployAssignment) bool {
	ctx := context.Background()
	p, err := e.St.LocalPublicationByDeployment(ctx, a.Deployment.ID)
	if err != nil {
		if ok, _ := e.St.IsLocalPreparation(ctx, a.Deployment.ID); ok {
			return e.dispatchLocalPreparation(a)
		}
		_ = e.failDeployInfra(ctx, a.Deployment.ID, a.RunnerID, "нет записи локальной публикации")
		return true
	}
	job := p.Job(a.ProjectID)
	secrets, sensitive, err := e.St.LocalSecretsForRunner(ctx, a.Deployment.ID, a.RunnerID, e.Box, p.Operation == "publish")
	if err != nil {
		_ = e.failDeployInfra(ctx, a.Deployment.ID, a.RunnerID, "не удалось безопасно передать секреты приложения")
		return true
	}
	job.Sensitive = sensitive
	if p.Operation == "publish" {
		job.Secrets = secrets
	}
	job.GarbageCollectionOnly = p.Operation == "cleanup"
	job.KeepPublications, _ = e.St.LocalKeepPublications(ctx, a.Env.ID)
	if p.Operation == "remove" {
		job.RemoveData, _ = e.St.LastLocalRemoval(ctx, p.EnvironmentID, p.ID)
	}
	data, _ := json.Marshal(job)
	e.mu.Lock()
	e.deployOwner[a.Deployment.ID] = a.RunnerID
	e.mu.Unlock()
	repoURL, token := "", ""
	if p.Operation == "publish" {
		repoURL, token = e.deployRepoAccess(a)
	}
	return e.sendLocalDocker(a, &pb.PlaneMsg{MsgId: fmt.Sprintf("local-%s", a.Deployment.ID), Kind: &pb.PlaneMsg_Deploy{Deploy: &pb.DeployJob{DeploymentId: a.Deployment.ID, EnvName: a.Env.Name, Repo: a.Repo, Version: p.Version, TimeoutS: int32(deployTimeout.Seconds()), Checkout: p.Operation == "publish", RepoUrl: repoURL, GitToken: token, LocalDockerJob: data, LocalDockerAction: p.Operation}}}, len(job.Secrets) > 0)
}
func (e *Engine) onLocalDockerResult(ctx context.Context, runnerID string, dr *pb.DeployResult) error {
	var res localdocker.Result
	if err := json.Unmarshal(dr.LocalDockerResult, &res); err != nil {
		return err
	}
	sensitive, err := e.St.DeploymentHasApplicationSecrets(ctx, dr.DeploymentId)
	if err != nil {
		return err
	}
	if sensitive {
		res = localdocker.ProtectResult(res)
	}
	if preparing, _ := e.St.IsLocalPreparation(ctx, dr.DeploymentId); preparing {
		status, reason := "waiting_artifact", ""
		if res.Status != "waiting_artifact" {
			status = "failed"
			if res.Reason != nil {
				reason = res.Reason.Message
			}
		}
		if err := e.St.SetLocalPreparation(ctx, dr.DeploymentId, runnerID, status, reason); err != nil {
			return err
		}
		if status == "failed" {
			return e.failDeployInfra(ctx, dr.DeploymentId, runnerID, reason)
		}
		if err := e.St.FinishLocalOperation(ctx, dr.DeploymentId, runnerID); err != nil {
			return err
		}
		e.dropDeploy(dr.DeploymentId)
		return nil
	}
	p, accepted, err := e.St.AcceptLocalResult(ctx, dr.DeploymentId, runnerID, res)
	if err != nil || !accepted {
		return err
	}
	switch res.Status {
	case "building", "starting", "verifying":
		return nil
	}
	if p.Operation == "publish" {
		log := e.flushDeployLog(ctx, dr.DeploymentId)
		if err = e.St.SaveLocalBuilderLog(ctx, dr.DeploymentId, log); err != nil {
			return err
		}
		if res.Status == "running" {
			return e.finishDeploy(ctx, dr.DeploymentId, runnerID, "done", "")
		}
		reason := "локальная публикация не выполнена"
		if res.Reason != nil {
			reason = res.Reason.Code + ": " + res.Reason.Message
		}
		return e.failDeploy(ctx, dr.DeploymentId, runnerID, reason, false)
	}
	if p.Operation == "reconcile" && res.Status == "failed" {
		detail := "контейнер публикации не восстановился"
		if res.Reason != nil {
			detail = res.Reason.Code + ": " + res.Reason.Message
		}
		return e.failDeployInfra(ctx, dr.DeploymentId, runnerID, detail)
	}
	// Технические сверки не создают релизы или инциденты кода.
	if err = e.St.FinishLocalOperation(ctx, dr.DeploymentId, runnerID); err != nil {
		return err
	}
	e.dropDeploy(dr.DeploymentId)
	return nil
}
func (e *Engine) tickLocalDocker(ctx context.Context) error {
	if err := e.St.ProcessLocalDeletions(ctx); err != nil {
		return err
	}
	if err := e.St.FlushPendingLocalPublications(ctx); err != nil {
		return err
	}
	ids, err := e.St.DueLocalPublications(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		action := "reconcile"
		if p, err := e.St.CurrentLocalPublication(ctx, id); err == nil && p.ContainerID == "" {
			action = "cleanup"
		}
		_, _ = e.St.QueueLocalOperation(ctx, id, action, "platform")
	}
	retired, err := e.St.RetiredLocalPublications(ctx)
	if err != nil {
		return err
	}
	for _, p := range retired {
		_, _ = e.St.QueueRetiredLocalStop(ctx, p.ID)
	}
	return nil
}

func (e *Engine) dispatchLocalPreparation(a store.DeployAssignment) bool {
	e.mu.Lock()
	e.deployOwner[a.Deployment.ID] = a.RunnerID
	e.mu.Unlock()
	config := a.Env.Config.LocalDocker
	if config == nil {
		return true
	}
	job := localdocker.Job{ProjectID: a.ProjectID, EnvironmentID: a.Env.ID, PublicationID: a.Deployment.ID, Generation: 1, Config: *config}
	data, _ := json.Marshal(job)
	return e.sendLocalDocker(a, &pb.PlaneMsg{MsgId: "prepare-" + a.Deployment.ID, Kind: &pb.PlaneMsg_Deploy{Deploy: &pb.DeployJob{DeploymentId: a.Deployment.ID, EnvName: a.Env.Name, LocalDockerJob: data, LocalDockerAction: "prepare"}}}, false)
}

// Недоставленная джоба не расходует поколение и не ждёт watchdog.
func (e *Engine) sendLocalDocker(a store.DeployAssignment, msg *pb.PlaneMsg, requireSecure bool) bool {
	ctx := context.Background()
	seen := time.Time{}
	if runner, err := e.St.GetRunner(ctx, a.RunnerID); err == nil {
		seen = runner.LastSeen
	}
	delivered := false
	if requireSecure {
		sender, ok := e.Out.(interface {
			SendSecure(string, *pb.PlaneMsg) bool
		})
		delivered = ok && sender.SendSecure(a.RunnerID, msg)
	} else {
		delivered = e.Out.Send(a.RunnerID, msg)
	}
	if delivered {
		return true
	}
	requeued, err := e.St.RequeueUndeliveredLocal(ctx, a.Deployment.ID, a.RunnerID, seen)
	if err != nil {
		slog.Error("local docker: requeue undelivered job", "deployment", a.Deployment.ID, "err", err)
	}
	if requeued {
		e.dropDeploy(a.Deployment.ID)
	}
	return false
}
