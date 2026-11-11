package runner

import (
	"context"
	"encoding/json"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"io"
	"slices"
)

type localLog func([]byte)

func (f localLog) Write(b []byte) (int, error) { f(b); return len(b), nil }

var _ io.Writer = localLog(nil)

func (a *agent) executeLocalDocker(ctx context.Context, dep *pb.DeployJob, emit func(*pb.RunnerMsg)) {
	ctx, cancel := context.WithTimeout(ctx, defaultDeployTimeout)
	defer cancel()
	send := func(v localdocker.Result) {
		b, _ := json.Marshal(v)
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_DeployResult{DeployResult: &pb.DeployResult{DeploymentId: dep.DeploymentId, Stage: pb.DeployResult_VERIFY, Ok: v.Status == "running" || v.Status == "stopped", LocalDockerResult: b}}})
	}
	if !slices.Contains(a.cfg.Capabilities, "docker") {
		send(localdocker.Result{Status: "failed", Reason: &localdocker.Failure{Code: "config_invalid", Message: "runner не поддерживает Docker"}})
		return
	}
	var job localdocker.Job
	if e := json.Unmarshal(dep.LocalDockerJob, &job); e != nil {
		send(localdocker.Result{Status: "failed", Reason: &localdocker.Failure{Code: "config_invalid", Message: "повреждённое задание публикации"}})
		return
	}
	log := localLog(func(b []byte) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Transcript{Transcript: &pb.TranscriptChunk{DeployId: dep.DeploymentId, Data: b}}})
	})
	rt := localdocker.Runtime{Log: log, Progress: func(status string) error { send(localdocker.Result{Status: status}); return ctx.Err() }}
	if dep.LocalDockerAction == "prepare" {
		send(rt.PrepareHost(ctx, job))
		return
	}
	if dep.LocalDockerAction == "publish" {
		dir, e := a.deployWorkspace(ctx, dep, func(b []byte) { _, _ = log.Write(b) })
		if e != nil {
			send(localdocker.Result{Status: "failed", Reason: &localdocker.Failure{Code: "build_failed", Message: "не удалось подготовить рабочую копию: " + e.Error()}})
			return
		}
		res := rt.Publish(ctx, dir, job)
		if ctx.Err() != nil {
			res.Status = "failed"
			res.Reason = &localdocker.Failure{Code: "timeout", Message: "истёк дедлайн публикации"}
		}
		if res.Status == "unknown" {
			res.Status = "failed"
		}
		send(res)
		return
	}
	// Контейнерные логи не смешиваются с логами builder/inspect.
	if dep.LocalDockerAction == "logs" {
		rt.Log = nil
	}
	rt.GarbageCollect(ctx, job)
	if dep.LocalDockerAction == "cleanup" {
		send(localdocker.Result{Status: "stopped"})
		return
	}
	res := rt.Operate(ctx, dep.LocalDockerAction, job)
	if res.Status == "running" || res.Status == "unhealthy" {
		logRt := localdocker.Runtime{}
		logs := logRt.Operate(ctx, "logs", job)
		res.Logs = logs.Logs
	}
	send(res)
}
