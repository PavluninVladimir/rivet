package stream

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Регистрация runner'а по токену (спека runners «Токены регистрации
// runner'ов»): без токена, с отозванным — единый отказ; с действующим —
// регистрация и событие установки.

func testStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("RIVET_DATABASE_URL")
	if base == "" {
		base = "postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("postgres недоступен: %v", err)
	}
	name := fmt.Sprintf("rivet_stream_test_%d_%x", time.Now().UnixNano(), rand.Uint32())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(base)
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func dialAuthServer(t *testing.T, st *store.Store) pb.RunnerServiceClient {
	t.Helper()
	rs := &Server{St: st, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second}
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

func TestRunnerRegistrationRequiresToken(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx, "root", "root-secret"); err != nil {
		t.Fatal(err)
	}
	admin, err := st.Authenticate(ctx, "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := st.CreateRunnerToken(ctx, "fleet", nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthServer(t, st)
	req := &pb.RegisterRequest{RunnerId: "r1", Agent: "rivet", Host: "h", Capabilities: []string{"coding"},
		ProtocolVersion: protocolVersion, Adapter: "wrap", Depth: "minimal", Features: []string{"native"}}

	mustUnauth := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s: ожидался Unauthenticated, получено %v", what, err)
		}
	}
	// Без токена и с неизвестным — единый отказ, runner не появляется.
	_, err = client.Register(ctx, req)
	mustUnauth("без токена", err)
	_, err = client.Register(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer rrt_bogus"), req)
	mustUnauth("неизвестный токен", err)
	if runners, _ := st.ListRunners(ctx); len(runners) != 0 {
		t.Fatalf("runner зарегистрирован без токена: %+v", runners)
	}
	// Канал без токена не открывается.
	stream, err := client.Channel(ctx)
	if err == nil {
		_ = stream.Send(&pb.RunnerMsg{MsgId: "1", Kind: &pb.RunnerMsg_Hello{Hello: &pb.Hello{RunnerId: "r1"}}})
		_, err = stream.Recv()
	}
	mustUnauth("канал без токена", err)

	// С действующим токеном: регистрация принята, событие установки с именем токена.
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
	resp, err := client.Register(authed, req)
	if err != nil || !resp.Accepted {
		t.Fatalf("регистрация с токеном: %v %+v", err, resp)
	}
	events, err := st.Events(ctx, store.EventFilter{Installation: true, Type: "runner.registered"})
	if err != nil || len(events) != 1 || events[0].Payload["token_name"] != "fleet" || events[0].ActorID != "r1" {
		t.Fatalf("событие регистрации: %v %+v", err, events)
	}
	if stream, err := client.Channel(authed); err != nil {
		t.Fatalf("канал с токеном: %v", err)
	} else {
		_ = stream.CloseSend()
	}

	// Отзыв закрывает новые регистрации.
	if err := st.RevokeRunnerToken(ctx, tok.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	_, err = client.Register(authed, req)
	mustUnauth("отозванный токен", err)
	if !errors.Is(st.RevokeRunnerToken(ctx, tok.ID, admin.Login), store.ErrRevoked) {
		t.Fatal("повторный отзыв должен давать ErrRevoked")
	}

	// Просроченный токен — тот же отказ.
	past := time.Now().Add(-time.Hour)
	_, expired, err := st.CreateRunnerToken(ctx, "old", &past, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Register(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+expired), req)
	mustUnauth("просроченный токен", err)
}

// Runner прежней версии протокола получает понятный отказ, а не разрыв
// (api-contract add-policy-delivery: v9).
func TestRegisterRejectsOldProtocol(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin, err := st.CreateUser(ctx, "root", "", "pw-root-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateRunnerToken(ctx, "fleet", nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthServer(t, st)
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
	resp, err := client.Register(authed, &pb.RegisterRequest{
		RunnerId: "old", Agent: "rivet", ProtocolVersion: "8", Features: []string{"native"}})
	if err != nil || resp.Accepted {
		t.Fatalf("v8 должен отклоняться: %v %+v", err, resp)
	}
	if !strings.Contains(resp.Message, protocolVersion) {
		t.Fatalf("сообщение должно называть нужную версию: %q", resp.Message)
	}
	// Актуальная версия с адаптером и глубиной регистрируется.
	// Неизвестная глубина — несовместимый runner, а не молчаливый minimal.
	resp, err = client.Register(authed, &pb.RegisterRequest{
		RunnerId: "odd", Agent: "rivet", Capabilities: []string{"coding"}, Features: []string{"native"},
		ProtocolVersion: protocolVersion, Adapter: "native", Depth: "superdeep",
	})
	if err != nil || resp.Accepted || !strings.Contains(resp.Message, "глубина") {
		t.Fatalf("неизвестная глубина должна отклоняться: %v %+v", err, resp)
	}
	resp, err = client.Register(authed, &pb.RegisterRequest{
		RunnerId: "fresh", Agent: "rivet", Capabilities: []string{"coding"},
		ProtocolVersion: protocolVersion, Adapter: "native", Depth: "full",
		ContextChannel: true, Features: []string{"native"}, NetworkIsolated: true})
	if err != nil || !resp.Accepted {
		t.Fatalf("актуальная версия должна регистрироваться: %v %+v", err, resp)
	}
	runners, err := st.ListRunners(ctx)
	if err != nil || len(runners) != 1 || runners[0].Adapter != "native" || runners[0].Depth != domain.DepthFull || !runners[0].NetworkIsolated {
		t.Fatalf("адаптер и глубина в списке: %v %+v", err, runners)
	}
	// Поддержка обратного канала объявляется при регистрации (спека runners).
	if !runners[0].ContextChannel {
		t.Fatalf("обратный канал должен сохраниться: %+v", runners[0])
	}
}

// Регистрация агентом вне каталога (claude-code, внешний CLI) и runner без
// агента не только с deploy — отказ FAILED_PRECONDITION с событием
// runner.rejected; runner выкатки без агента принимается.
func TestRegisterRejectsNonRivetAgents(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin, err := st.CreateUser(ctx, "root", "", "pw-root-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateRunnerToken(ctx, "fleet", nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthServer(t, st)
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
	reasons := map[string]string{
		"cc":      "агентом может быть только агент Rivet: запустите rivet-runner -agent rivet",
		"nofeat":  "runner прежней версии без агентского цикла Rivet: обновите rivet-runner",
		"noagent": "runner без агента допускается только с единственной capability deploy (runner выкатки)",
	}
	// Отказанный runner переподключается снова и снова: событие одно на
	// runner и причину за окно (ревью P2-2), отказ — на каждую попытку.
	for range 3 {
		for _, req := range []*pb.RegisterRequest{
			{RunnerId: "cc", Agent: "claude-code", Capabilities: []string{"coding"}, ProtocolVersion: protocolVersion, Depth: "full"},
			{RunnerId: "nofeat", Agent: "rivet", Capabilities: []string{"coding"}, ProtocolVersion: protocolVersion, Depth: "full"},
			{RunnerId: "noagent", Capabilities: []string{"coding"}, ProtocolVersion: protocolVersion, Depth: "minimal"},
		} {
			_, err := client.Register(authed, req)
			if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != reasons[req.RunnerId] {
				t.Fatalf("%s: ожидается FailedPrecondition «%s», получено %v", req.RunnerId, reasons[req.RunnerId], err)
			}
		}
	}
	evs, err := st.Events(ctx, store.EventFilter{Type: "runner.rejected"})
	if err != nil || len(evs) != 3 {
		t.Fatalf("события runner.rejected: %v %d", err, len(evs))
	}
	for _, e := range evs {
		if e.Payload["reason"] != reasons[e.ActorID] || e.Payload["token_name"] != "fleet" {
			t.Fatalf("событие отказа %s: %+v", e.ActorID, e.Payload)
		}
	}
	resp, err := client.Register(authed, &pb.RegisterRequest{RunnerId: "dep", Capabilities: []string{"deploy"}, ProtocolVersion: protocolVersion, Depth: "minimal"})
	if err != nil || !resp.Accepted {
		t.Fatalf("runner выкатки без агента должен регистрироваться: %v %+v", err, resp)
	}
}

// Окно событий отказа: повтор внутри окна молчит, после окна событие
// пишется снова с числом пропущенных отказов; разные причины и runner'ы
// считаются отдельно.
func TestRejectionEventWindow(t *testing.T) {
	var s Server
	t0 := time.Now()
	if write, n := s.noteRejection("old", "r1", t0); !write || n != 0 {
		t.Fatalf("первый отказ: %v %d", write, n)
	}
	for i := 1; i <= 5; i++ {
		if write, _ := s.noteRejection("old", "r1", t0.Add(time.Duration(i)*3*time.Second)); write {
			t.Fatalf("повтор %d внутри окна записан", i)
		}
	}
	if write, _ := s.noteRejection("old", "r2", t0.Add(time.Minute)); !write {
		t.Fatal("другая причина не записана")
	}
	if write, _ := s.noteRejection("other", "r1", t0.Add(time.Minute)); !write {
		t.Fatal("другой runner не записан")
	}
	if write, n := s.noteRejection("old", "r1", t0.Add(rejectedEventWindow)); !write || n != 5 {
		t.Fatalf("после окна: %v, пропущено %d", write, n)
	}
	// Записи, которые давно не повторялись, не копятся.
	s.noteRejection("late", "r1", t0.Add(3*rejectedEventWindow))
	if len(s.rejSeen) != 1 {
		t.Fatalf("устаревшие записи не очищены: %d", len(s.rejSeen))
	}
}
