package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func (e *Engine) dispatchLocalDocker(a store.DeployAssignment) {
	ctx := context.Background()
	p, err := e.St.LocalPublicationByDeployment(ctx, a.Deployment.ID)
	if err != nil {
		if ok, _ := e.St.IsLocalPreparation(ctx, a.Deployment.ID); ok {
			e.dispatchLocalPreparation(a)
			return
		}
		_ = e.failDeployInfra(ctx, a.Deployment.ID, a.RunnerID, "нет записи локальной публикации")
		return
	}
	job := p.Job(a.ProjectID)
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
	e.Out.Send(a.RunnerID, &pb.PlaneMsg{MsgId: fmt.Sprintf("local-%s", a.Deployment.ID), Kind: &pb.PlaneMsg_Deploy{Deploy: &pb.DeployJob{DeploymentId: a.Deployment.ID, EnvName: a.Env.Name, Repo: a.Repo, Version: p.Version, TimeoutS: int32(deployTimeout.Seconds()), Checkout: p.Operation == "publish", RepoUrl: repoURL, GitToken: token, LocalDockerJob: data, LocalDockerAction: p.Operation}}})
}
func (e *Engine) onLocalDockerResult(ctx context.Context, runnerID string, dr *pb.DeployResult) error {
	var res localdocker.Result
	if err := json.Unmarshal(dr.LocalDockerResult, &res); err != nil {
		return err
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

func (e *Engine) dispatchLocalPreparation(a store.DeployAssignment) {
	e.mu.Lock()
	e.deployOwner[a.Deployment.ID] = a.RunnerID
	e.mu.Unlock()
	config := a.Env.Config.LocalDocker
	if config == nil {
		return
	}
	job := localdocker.Job{ProjectID: a.ProjectID, EnvironmentID: a.Env.ID, PublicationID: a.Deployment.ID, Generation: 1, Config: *config}
	data, _ := json.Marshal(job)
	e.Out.Send(a.RunnerID, &pb.PlaneMsg{MsgId: "prepare-" + a.Deployment.ID, Kind: &pb.PlaneMsg_Deploy{Deploy: &pb.DeployJob{DeploymentId: a.Deployment.ID, EnvName: a.Env.Name, LocalDockerJob: data, LocalDockerAction: "prepare"}}})
}
