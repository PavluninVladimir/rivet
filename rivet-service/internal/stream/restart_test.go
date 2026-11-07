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
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

type nopSender struct{}

func (nopSender) Send(string, *pb.PlaneMsg) bool { return true }

// Перезапуск runner'а (новый boot id): открытые запуски снимаются и агенты
// команды свободны; переподключение того же процесса их не трогает
// (add-agent-roles, ревью: агент, зависший занятым).
func TestRunnerRestartReleasesRuns(t *testing.T) {
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
	client := dialServerWithEngine(t, st, orchestrator.New(st, nil, nil, nopSender{}, 90*time.Second))
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
	register := func(boot string) {
		t.Helper()
		resp, err := client.Register(authed, &pb.RegisterRequest{RunnerId: "r1", Agent: "rivet", Host: "h",
			Capabilities: []string{"coding"}, ProtocolVersion: protocolVersion, Adapter: "wrap", Depth: "minimal", BootId: boot, Features: []string{"native"}})
		if err != nil || !resp.Accepted {
			t.Fatalf("регистрация: %v %+v", err, resp)
		}
	}
	register("boot-1")

	p, _ := st.CreateProject(ctx, "p", "o/r", nil, admin.ID)
	ep, _ := st.CreateEpic(ctx, p.ID, "e", "")
	task, err := st.CreateTask(ctx, ep.ID, store.NewTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionEpic(ctx, ep.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, ActorID: "t", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeEpic(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	proc := policy.Defaults().EffectiveProcess()
	code, _ := proc.Step("code")
	dev, _ := st.GetRole(ctx, "developer")
	if _, err := st.EnterStep(ctx, store.EnterStep{TaskID: task.ID, Step: code, Entry: "ok", Process: &proc, ProcessHash: "h",
		Roles: map[string]store.RoleBinding{"p1": store.ResolveRole(dev, true, nil, true)}}); err != nil {
		t.Fatal(err)
	}
	a, ok, err := st.AssignRun(ctx, nil, nil)
	if err != nil || !ok || a.Run.AgentID == "" {
		t.Fatalf("назначение: %v %v", ok, err)
	}
	// Переподключение того же процесса: запуск на месте.
	register("boot-1")
	if ag, _ := st.GetTeamAgent(ctx, a.Run.AgentID); ag.TaskID != task.ID {
		t.Fatalf("переподключение не снимает запуск: %+v", ag)
	}
	// Перезапуск: запуск снят, агент свободен.
	register("boot-2")
	if ag, _ := st.GetTeamAgent(ctx, a.Run.AgentID); ag.TaskID != "" {
		t.Fatalf("перезапуск освобождает агента: %+v", ag)
	}
}

// dialServerWithEngine — сервер протокола с движком (регистрация снимает
// запуски перезапущенного runner'а через движок).
func dialServerWithEngine(t *testing.T, st *store.Store, e *orchestrator.Engine) pb.RunnerServiceClient {
	t.Helper()
	rs := &Server{St: st, Engine: e, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second}
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
	return pb.NewRunnerServiceClient(conn)
}
