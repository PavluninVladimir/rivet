package stream

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Runner зарегистрировался, но канал ещё не открыт, и тик в этом окне
// назначает ему стадию: недоставка ставит offline, а открытие канала его
// снимает, и стадия уходит этому runner'у (ревью add-agent-roles, круг 3,
// P2-2).
func TestOfflineAfterReconnectRace(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	admin, err := st.Authenticate(ctx, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateRunnerToken(ctx, "fleet", nil, []string{"r1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	e := orchestrator.New(st, nil, nil, reg, 90*time.Second)
	rs := &Server{St: st, Engine: e, Reg: reg, Hub: NewHub(), HeartbeatInterval: 30 * time.Second}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnaryInterceptor(rs.UnaryAuth()), grpc.StreamInterceptor(rs.StreamAuth()))
	pb.RegisterRunnerServiceServer(srv, rs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewRunnerServiceClient(conn)
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)

	if resp, err := client.Register(authed, &pb.RegisterRequest{RunnerId: "r1", Agent: "rivet", Host: "h",
		Capabilities: []string{"coding"}, ProtocolVersion: protocolVersion, Adapter: "wrap", Depth: "minimal", BootId: "b1", Features: []string{"native"}}); err != nil || !resp.Accepted {
		t.Fatalf("register: %v %+v", err, resp)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, admin.ID)
	ep, _ := st.CreateEpic(ctx, p.ID, "e", "")
	task, _ := st.CreateTask(ctx, ep.ID, store.NewTask{Title: "t"})
	if err := st.TransitionEpic(ctx, ep.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, ActorID: "t", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeEpic(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	// Тик в окне между Register и Hello: недоставка.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetRunner(ctx, "r1"); r.Status != domain.RunnerOffline {
		t.Fatalf("недоставка ставит offline: %s", r.Status)
	}
	stream, err := client.Channel(authed)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.RunnerMsg{MsgId: "h1", Kind: &pb.RunnerMsg_Hello{Hello: &pb.Hello{RunnerId: "r1"}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, _ := st.GetRunner(ctx, "r1"); reg.Connected("r1") && r.Status == domain.RunnerIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("открытый канал снимает отметку offline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	tk, _ := st.GetTask(ctx, task.ID)
	if r, _ := st.GetRunner(ctx, "r1"); r.TaskID != task.ID || tk.RunnerID != "r1" {
		t.Fatalf("стадия ушла переподключившемуся runner'у: runner=%+v task runner=%q", r, tk.RunnerID)
	}
}
