package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/PavluninVladimir/rivet/internal/loadtest"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

type loadControl struct {
	runID  string
	cancel context.CancelFunc
	stage  string
	grant  chan struct{}
}

func loadOnly(caps []string) bool { return len(caps) == 1 && caps[0] == "load" }
func loadMessageAllowed(msg *pb.PlaneMsg) bool {
	switch msg.Kind.(type) {
	case *pb.PlaneMsg_Ack, *pb.PlaneMsg_LoadTest, *pb.PlaneMsg_LoadTestCancel, *pb.PlaneMsg_LoadTestContinue, *pb.PlaneMsg_Pause:
		return true
	}
	return false
}
func (a *agent) pruneLoadsLocked() {
	if a.loadActive == nil {
		a.loadActive = map[string]loadControl{}
	}
	if a.loadStopped == nil {
		a.loadStopped = map[string]time.Time{}
	}
	for id, deadline := range a.loadStopped {
		if !deadline.After(time.Now()) {
			delete(a.loadStopped, id)
		}
	}
}
func (a *agent) startLoad(ctx context.Context, msg *pb.LoadTestJob, bootID string, emit func(*pb.RunnerMsg)) {
	// A new process cannot acknowledge a previous boot's orphan as stopped.
	if msg == nil || msg.BootId != bootID || len(msg.Payload) > 32<<10 {
		return
	}
	var job loadtest.Job
	dec := json.NewDecoder(bytes.NewReader(msg.Payload))
	dec.DisallowUnknownFields()
	if dec.Decode(&job) != nil || dec.Decode(new(any)) != io.EOF || job.RunID != msg.RunId || job.AssignmentID != msg.AssignmentId || job.RunID == "" || job.AssignmentID == "" {
		return
	}
	send := func(stage string, result loadtest.Result) {
		raw, err := json.Marshal(result)
		if err != nil {
			return
		}
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_LoadTestResult{LoadTestResult: &pb.LoadTestResult{RunId: job.RunID, AssignmentId: job.AssignmentID, Stage: stage, Payload: raw}}})
	}
	fail := func(code string) {
		send("", loadtest.Result{State: "config_needed", Reason: &loadtest.Failure{Code: code, Message: "выделенный runner не может выполнить назначение"}, Stages: []loadtest.Evidence{}})
	}
	if job.Target.Host != hostname() || !job.Deadline.After(time.Now()) || job.Profile.Validate(job.Limits) != nil {
		fail("invalid_assignment")
		return
	}
	a.mu.Lock()
	a.pruneLoadsLocked()
	if _, ok := a.loadActive[job.AssignmentID]; ok {
		a.mu.Unlock()
		return
	}
	if _, ok := a.loadStopped[job.AssignmentID]; ok {
		a.mu.Unlock()
		send("", loadtest.Result{State: "interrupted", Stages: []loadtest.Evidence{}, Reason: &loadtest.Failure{Code: "cancelled", Message: "назначение уже остановлено"}})
		return
	}
	if len(a.loadActive) > 0 || len(a.loadStopped) >= 10000 {
		a.mu.Unlock()
		fail("runner_busy")
		return
	}
	runCtx, cancel := context.WithDeadline(ctx, job.Deadline)
	a.loadActive[job.AssignmentID] = loadControl{runID: job.RunID, cancel: cancel}
	a.mu.Unlock()
	a.goStage(stageInfo{Session: job.RunID, Stage: "LOAD_TEST"}, func() {
		defer cancel()
		defer func() {
			a.mu.Lock()
			delete(a.loadActive, job.AssignmentID)
			a.loadStopped[job.AssignmentID] = job.Deadline
			a.mu.Unlock()
		}()
		runtime := loadtest.Runtime{K6Bin: a.cfg.K6Bin, Progress: func(stage string, r loadtest.Result) error {
			grant := make(chan struct{}, 1)
			a.mu.Lock()
			c := a.loadActive[job.AssignmentID]
			c.stage = stage
			c.grant = grant
			a.loadActive[job.AssignmentID] = c
			a.mu.Unlock()
			send(stage, r)
			select {
			case <-grant:
				return runCtx.Err()
			case <-runCtx.Done():
				return runCtx.Err()
			}
		}}
		result := runtime.Execute(runCtx, job)
		// Execute has killed/waited for native k6 and removed the secret workspace.
		send("", result)
	})
}
func (a *agent) cancelLoad(msg *pb.LoadTestCancel) {
	if msg == nil || msg.RunId == "" || msg.AssignmentId == "" {
		return
	}
	deadline := time.UnixMilli(msg.DeadlineUnixMs)
	if !deadline.After(time.Now()) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLoadsLocked()
	if c, ok := a.loadActive[msg.AssignmentId]; ok && c.runID == msg.RunId {
		c.cancel()
	}
	if len(a.loadStopped) < 10000 {
		a.loadStopped[msg.AssignmentId] = deadline
	}
	// Unknown cancellation is not proof that an old boot's process stopped.
}

// A generic transport ACK is insufficient: this grant follows committed evidence.
func (a *agent) continueLoad(msg *pb.LoadTestContinue) {
	if msg == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.loadActive[msg.AssignmentId]
	if !ok || c.runID != msg.RunId || c.stage != msg.Stage || c.grant == nil {
		return
	}
	select {
	case c.grant <- struct{}{}:
	default:
	}
	c.grant = nil
	a.loadActive[msg.AssignmentId] = c
}
