package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"

	"github.com/PavluninVladimir/rivet/internal/loadtest"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func (e *Engine) tickLoadTests(ctx context.Context) {
	if err := e.TickLoadTests(ctx); err != nil {
		slog.Error("нагрузочное тестирование", "err", err)
	}
}

// TickLoadTests never creates load runs: only an explicit API request can queue one.
func (e *Engine) TickLoadTests(ctx context.Context) error {
	if err := e.St.ReconcileLoadPlans(ctx); err != nil {
		return err
	}
	if _, err := e.St.ReconcileLoadRuns(ctx); err != nil {
		return err
	}
	if err := e.FlushLoadCancels(ctx); err != nil {
		return err
	}
	if e.Out == nil {
		return nil
	}
	for range 32 {
		r, ok, err := e.St.ClaimLoadTest(ctx, e.LoadLimits)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		auth, err := e.St.LoadAuthValue(ctx, r, e.Box)
		if err != nil {
			_, _, err = e.St.AcceptLoadResult(ctx, r.RunnerID, r.ID, r.AssignmentID, "", loadtest.Result{State: "config_needed", Reason: &loadtest.Failure{Code: "auth_required", Message: "явно выбранный тестовый секрет недоступен"}})
			if err != nil {
				return err
			}
			continue
		}
		active, err := e.St.LoadAssignmentActive(ctx, r)
		if err != nil {
			return err
		}
		if !active {
			continue
		}
		job := loadtest.Job{RunID: r.ID, AssignmentID: r.AssignmentID, Target: r.Target, Profile: r.Profile, Limits: r.Limits, Deadline: *r.Deadline, AuthValue: auth}
		payload, err := json.Marshal(job)
		if err != nil {
			return err
		}
		msg := &pb.PlaneMsg{Kind: &pb.PlaneMsg_LoadTest{LoadTest: &pb.LoadTestJob{RunId: r.ID, AssignmentId: r.AssignmentID, BootId: r.BootID, Payload: payload}}}
		delivered := false
		if r.Profile.Auth != nil {
			sender, ok := e.Out.(interface {
				SendSecure(string, *pb.PlaneMsg) bool
			})
			delivered = ok && sender.SendSecure(r.RunnerID, msg)
		} else {
			delivered = e.Out.Send(r.RunnerID, msg)
		}
		if !delivered {
			// Registry false proves nothing was enqueued. Later channel loss is
			// uncertain and ReconcileLoadRuns interrupts without retry.
			if err = e.St.RequeueUndeliveredLoad(ctx, r); err != nil {
				return err
			}
			return nil
		}
	}
	return nil
}
func (e *Engine) FlushLoadCancels(ctx context.Context) error {
	runs, err := e.St.PendingLoadCancels(ctx)
	if err != nil {
		return err
	}
	if e.Out == nil {
		return nil
	}
	for _, r := range runs {
		if r.Deadline != nil {
			e.Out.Send(r.RunnerID, &pb.PlaneMsg{Kind: &pb.PlaneMsg_LoadTestCancel{LoadTestCancel: &pb.LoadTestCancel{RunId: r.ID, AssignmentId: r.AssignmentID, DeadlineUnixMs: r.Deadline.UnixMilli()}}})
		}
	}
	return nil
}
func (e *Engine) OnLoadTestResult(ctx context.Context, runnerID string, msg *pb.LoadTestResult) error {
	if msg == nil || len(msg.Payload) > 4*loadtest.SummaryLimit+(64<<10) {
		return nil
	}
	var result loadtest.Result
	dec := json.NewDecoder(bytes.NewReader(msg.Payload))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || dec.Decode(new(any)) != io.EOF {
		return nil
	}
	run, accepted, err := e.St.AcceptLoadResult(ctx, runnerID, msg.RunId, msg.AssignmentId, msg.Stage, result)
	if err != nil {
		return err
	}
	if accepted && run.State == "running" && result.State == "running" && e.Out != nil {
		active, err := e.St.LoadAssignmentActive(ctx, run)
		if err != nil {
			return err
		}
		if active {
			e.Out.Send(runnerID, &pb.PlaneMsg{Kind: &pb.PlaneMsg_LoadTestContinue{LoadTestContinue: &pb.LoadTestContinue{RunId: run.ID, AssignmentId: run.AssignmentID, Stage: msg.Stage}}})
		}
	}
	return e.FlushLoadCancels(ctx)
}
