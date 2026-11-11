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

// lifecycleFixture — rivetd с одним runner'ом r1 через bufconn.
type lifecycleFixture struct {
	st     *store.Store
	e      *orchestrator.Engine
	reg    *Registry
	client pb.RunnerServiceClient
	authed context.Context
	admin  domain.User
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
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
	return &lifecycleFixture{st: st, e: e, reg: reg, client: pb.NewRunnerServiceClient(conn),
		authed: metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret), admin: admin}
}

func (f *lifecycleFixture) register(t *testing.T, boot string, pending []string) {
	t.Helper()
	if resp, err := f.client.Register(f.authed, &pb.RegisterRequest{RunnerId: "r1", Agent: "rivet", Host: "h",
		Capabilities: []string{"coding"}, ProtocolVersion: protocolVersion, Adapter: "wrap", Depth: "minimal",
		BootId: boot, PendingSessions: pending, Features: []string{"native"}}); err != nil || !resp.Accepted {
		t.Fatalf("register: %v %+v", err, resp)
	}
}

func (f *lifecycleFixture) open(t *testing.T) pb.RunnerService_ChannelClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.authed, 15*time.Second)
	t.Cleanup(cancel)
	s, err := f.client.Channel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&pb.RunnerMsg{MsgId: "hello-" + time.Now().String(), Kind: &pb.RunnerMsg_Hello{Hello: &pb.Hello{RunnerId: "r1"}}}); err != nil {
		t.Fatal(err)
	}
	// Registry может ещё указывать на старый канал. Ack нового heartbeat
	// подтверждает готовность именно этого соединения и RunnerAttached в БД.
	readyID := "ready-" + time.Now().String()
	if err := s.Send(&pb.RunnerMsg{MsgId: readyID, Kind: &pb.RunnerMsg_Heartbeat{Heartbeat: &pb.Heartbeat{}}}); err != nil {
		t.Fatal(err)
	}
	recvAck(t, s, readyID)
	return s
}

// task — задача Epic'а в работе.
func (f *lifecycleFixture) task(t *testing.T) domain.Task {
	t.Helper()
	ctx := context.Background()
	p, _ := f.st.CreateProject(ctx, "p", "o/r", nil, f.admin.ID)
	ep, _ := f.st.CreateEpic(ctx, p.ID, "e", "")
	task, err := f.st.CreateTask(ctx, ep.ID, store.NewTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.TransitionEpic(ctx, ep.ID, domain.EpicRunning, store.EventInput{ActorKind: domain.ActorUser, ActorID: "t", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecomputeEpic(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	return task
}

// recvAssign — следующее назначение в канале (ack'и пропускаются).
func recvAssign(t *testing.T, s pb.RunnerService_ChannelClient) *pb.Assignment {
	t.Helper()
	for {
		msg, err := s.Recv()
		if err != nil {
			t.Fatalf("канал: %v", err)
		}
		if a := msg.GetAssign(); a != nil {
			return a
		}
	}
}

// recvAck ждёт ack на сообщение msgID (назначения и прочее пропускаются).
func recvAck(t *testing.T, s pb.RunnerService_ChannelClient, msgID string) {
	t.Helper()
	for {
		msg, err := s.Recv()
		if err != nil {
			t.Fatalf("канал: %v", err)
		}
		if a := msg.GetAck(); a != nil && a.AckedMsgId == msgID {
			return
		}
	}
}

// SIGTERM посреди CODING: прерванная стадия итога не шлёт, новый процесс
// регистрируется без неё в pending_sessions, и стадия назначается заново
// без расхода попытки (ревью add-agent-roles, круг 4, P2-1).
func TestGracefulRestartReassignsWithoutAttempt(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	f.register(t, "boot-1", nil)
	s1 := f.open(t)
	task := f.task(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	first := recvAssign(t, s1)
	before, _ := f.st.GetTask(ctx, task.ID)
	_ = s1.CloseSend()
	// Новый процесс: журнал пуст, прерванная стадия итога не записала.
	f.register(t, "boot-2", nil)
	s2 := f.open(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	again := recvAssign(t, s2)
	if again.TaskId != task.ID || again.SessionId == first.SessionId {
		t.Fatalf("стадия назначена заново новой сессией: %+v", again)
	}
	after, _ := f.st.GetTask(ctx, task.ID)
	if after.AttemptUsed != before.AttemptUsed || after.Status != domain.TaskRunning {
		t.Fatalf("попытка не израсходована: %d → %d, статус %s", before.AttemptUsed, after.AttemptUsed, after.Status)
	}
}

// Heartbeat от runner'а, потерянного по таймауту: отметку offline он не
// снимает, канал закрывается, а новый канал после регистрации возвращает
// runner в работу (круг 4, P2-2).
func TestStaleRunnerHeartbeatClosesChannel(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	f.register(t, "boot-1", nil)
	s1 := f.open(t)
	// Registry видит канал раньше, чем RunnerAttached снимает отметку offline
	// в базе. Ack на первый heartbeat гарантирует, что RunnerAttached уже
	// прошёл: сервер обрабатывает сообщения канала по очереди. Иначе он мог
	// вернуть runner в строй после MarkStaleRunnersOffline.
	if err := s1.Send(&pb.RunnerMsg{MsgId: "hb-0", Kind: &pb.RunnerMsg_Heartbeat{Heartbeat: &pb.Heartbeat{}}}); err != nil {
		t.Fatal(err)
	}
	recvAck(t, s1, "hb-0")
	if _, _, err := f.st.MarkStaleRunnersOffline(ctx, -60); err != nil {
		t.Fatal(err)
	}
	if err := s1.Send(&pb.RunnerMsg{MsgId: "hb-1", Kind: &pb.RunnerMsg_Heartbeat{Heartbeat: &pb.Heartbeat{}}}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		for {
			if _, err := s1.Recv(); err != nil {
				closed <- err
				return
			}
		}
	}()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("канал потерянного runner'а закрыт без ошибки")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("канал потерянного runner'а не закрылся за 5 с")
	}
	if r, _ := f.st.GetRunner(ctx, "r1"); r.Status != domain.RunnerOffline {
		t.Fatalf("heartbeat не снимает offline после таймаута: %s", r.Status)
	}
	f.register(t, "boot-2", nil)
	_ = f.open(t)
	if r, _ := f.st.GetRunner(ctx, "r1"); r.Status != domain.RunnerIdle {
		t.Fatalf("новый канал возвращает runner: %s", r.Status)
	}
}
