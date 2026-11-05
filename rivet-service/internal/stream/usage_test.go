package stream

import (
	"context"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Usage агента Rivet доходит до записи вместе с кешированным входом и
// записью в кеш (add-native-agent, задача 1.9.1); отрицательные значения
// считаются несообщёнными.
func TestUsageCachedTokensRecorded(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	owner, err := st.CreateUser(ctx, "alice", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	task, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A"})
	session, err := st.CreateSession(ctx, domain.Session{TaskID: task.ID, Attempt: 1, DriverKind: "scheduler",
		Agent: store.AgentRivet, Depth: domain.DepthFull, Scope: "CODING"})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{St: st, Engine: orchestrator.New(st, nil, (*blob.Store)(nil), sinkSender{}, 0), Hub: NewHub()}
	in, out, cached, write, bad := int64(1000), int64(200), int64(800), int64(100), int64(-5)
	for i, u := range []*pb.Usage{
		{TaskId: task.ID, SessionId: session, Model: "m", TokensIn: &in, TokensOut: &out, TokensCached: &cached, TokensCacheWrite: &write},
		{TaskId: task.ID, SessionId: session, Model: "m", TokensIn: &in, TokensCached: &bad},
	} {
		if err := srv.handle(ctx, "r1", &pb.RunnerMsg{MsgId: "u" + string(rune('0'+i)), Kind: &pb.RunnerMsg_Usage{Usage: u}}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := st.EpicUsage(ctx, epic.ID)
	if err != nil || len(rows) != 1 || total == nil {
		t.Fatalf("usage Epic'а: %+v %v", rows, err)
	}
	if *total.TokensIn != 2000 || total.TokensCached == nil || *total.TokensCached != 800 || total.TokensCacheWrite == nil || *total.TokensCacheWrite != 100 {
		t.Fatalf("кешированные токены: in=%v cached=%v write=%v", *total.TokensIn, total.TokensCached, total.TokensCacheWrite)
	}
}
