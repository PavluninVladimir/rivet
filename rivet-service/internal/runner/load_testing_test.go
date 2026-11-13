package runner

import (
	"context"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/loadtest"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func TestLoadOnlyMessageBoundaryAndFeatures(t *testing.T) {
	cfg := Config{Capabilities: []string{"load"}}
	if f := cfg.features(); len(f) != 1 || f[0] != loadtest.Feature {
		t.Fatal(f)
	}
	for _, msg := range []*pb.PlaneMsg{{Kind: &pb.PlaneMsg_Assign{}}, {Kind: &pb.PlaneMsg_Deploy{}}, {Kind: &pb.PlaneMsg_Monitor{}}, {Kind: &pb.PlaneMsg_HostJob{}}, {Kind: &pb.PlaneMsg_Context{}}} {
		if loadMessageAllowed(msg) {
			t.Fatal("load runner accepted nonload operation")
		}
	}
	if !loadMessageAllowed(&pb.PlaneMsg{Kind: &pb.PlaneMsg_LoadTestContinue{}}) {
		t.Fatal("phase grant blocked")
	}
}
func TestLoadPhaseGrantFencingAndCancelTombstone(t *testing.T) {
	a := &agent{}
	a.mu.Lock()
	a.pruneLoadsLocked()
	a.mu.Unlock()
	grant := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.loadActive["lease"] = loadControl{runID: "run", cancel: cancel, stage: "capacity_1", grant: grant}
	for _, msg := range []*pb.LoadTestContinue{{RunId: "foreign", AssignmentId: "lease", Stage: "capacity_1"}, {RunId: "run", AssignmentId: "old", Stage: "capacity_1"}, {RunId: "run", AssignmentId: "lease", Stage: "target"}} {
		a.continueLoad(msg)
	}
	select {
	case <-grant:
		t.Fatal("stale grant released phase")
	default:
	}
	a.continueLoad(&pb.LoadTestContinue{RunId: "run", AssignmentId: "lease", Stage: "capacity_1"})
	select {
	case <-grant:
	default:
		t.Fatal("committed exact phase grant ignored")
	}
	a.continueLoad(&pb.LoadTestContinue{RunId: "run", AssignmentId: "lease", Stage: "capacity_1"})
	select {
	case <-grant:
		t.Fatal("duplicate grant reused")
	default:
	}
	a.cancelLoad(&pb.LoadTestCancel{RunId: "run", AssignmentId: "lease", DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli()})
	if ctx.Err() == nil || a.loadStopped["lease"].IsZero() {
		t.Fatal("cancel failed to fence load")
	}
	a.cancelLoad(&pb.LoadTestCancel{RunId: "unknown", AssignmentId: "future", DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli()})
	if a.loadStopped["future"].IsZero() {
		t.Fatal("cancel-before-job lost")
	}
}
