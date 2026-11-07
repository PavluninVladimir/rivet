package runner

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/PavluninVladimir/rivet/internal/runnertls"
	"github.com/PavluninVladimir/rivet/internal/runnertls/tlstest"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// rejectPlane — control plane, который отклоняет регистрацию так же, как
// rivetd стороннему агенту: FAILED_PRECONDITION с причиной.
type rejectPlane struct {
	pb.UnimplementedRunnerServiceServer
	calls atomic.Int32
	// code и msg: отказ, без них отказ по агенту.
	code codes.Code
	msg  string
	// cert: CN клиентского сертификата последней регистрации (mTLS).
	cert atomic.Value
}

func (p *rejectPlane) Register(ctx context.Context, _ *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	p.calls.Add(1)
	if c := runnertls.PeerCertificate(ctx); c != nil {
		p.cert.Store(c.Subject.CommonName)
	}
	if p.code != codes.OK {
		return nil, status.Error(p.code, p.msg)
	}
	return nil, status.Error(codes.FailedPrecondition, "агентом может быть только агент Rivet: запустите rivet-runner -agent rivet")
}

// Отказ регистрации повтором не лечится: runner не переподключается раз в
// 3 секунды, а завершается с текстом причины (ревью интеграции, P2-2).
func TestRegisterRejectionIsFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	plane := &rejectPlane{}
	pb.RegisterRunnerServiceServer(srv, plane)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	err = Run(ctx, Config{PlaneAddr: ln.Addr().String(), Token: "rrt_test", ID: "r1", Agent: AgentRivet,
		Adapter: AdapterNative, Workdir: t.TempDir()})
	if !errors.Is(err, ErrRegistrationRejected) || !strings.Contains(err.Error(), "запустите rivet-runner -agent rivet") {
		t.Fatalf("ждём фатальный отказ с причиной, получено: %v", err)
	}
	if ctx.Err() != nil || time.Since(started) > 10*time.Second {
		t.Fatalf("runner ждал вместо выхода: %v", time.Since(started))
	}
	if n := plane.calls.Load(); n != 1 {
		t.Fatalf("регистрация повторялась %d раз, ждём одну попытку", n)
	}
}

// Отказ по идентичности (add-runner-identity): токен или сертификат выданы
// для другого runner'а, тоже фатален. Runner предъявляет клиентский
// сертификат установке с mTLS.
func TestIdentityRejectionIsFatalOverMutualTLS(t *testing.T) {
	ca := tlstest.NewCA(t, "runners")
	srvCert, srvKey := ca.Server(t)
	cfg, err := runnertls.ServerConfig(srvCert, srvKey, ca.File)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	plane := &rejectPlane{code: codes.PermissionDenied, msg: "токен регистрации не выпущен для runner'а r1"}
	pb.RegisterRunnerServiceServer(srv, plane)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	certFile, keyFile := ca.Issue(t, "r1", tlstest.Leaf{CN: "r1"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = Run(ctx, Config{PlaneAddr: ln.Addr().String(), Token: "rrt_test", ID: "r1", Agent: AgentRivet,
		Adapter: AdapterNative, Workdir: t.TempDir(), TLS: true, TLSCA: ca.File, TLSCert: certFile, TLSKey: keyFile})
	if !errors.Is(err, ErrRegistrationRejected) || !strings.Contains(err.Error(), "не выпущен для runner'а r1") {
		t.Fatalf("ждём фатальный отказ по идентичности, получено: %v", err)
	}
	if n := plane.calls.Load(); n != 1 {
		t.Fatalf("регистрация повторялась %d раз", n)
	}
	if cn, _ := plane.cert.Load().(string); cn != "r1" {
		t.Fatalf("сервер не увидел клиентский сертификат runner'а: %q", cn)
	}
	// Сертификат без TLS даёт ошибку запуска, а не подключение без него.
	err = Run(ctx, Config{PlaneAddr: ln.Addr().String(), Token: "rrt_test", ID: "r1", Agent: AgentRivet,
		Adapter: AdapterNative, Workdir: t.TempDir(), TLSCert: certFile, TLSKey: keyFile})
	if err == nil || !strings.Contains(err.Error(), "требует TLS") {
		t.Fatalf("сертификат без TLS: %v", err)
	}
}

// Недопустимый идентификатор (INVALID_ARGUMENT) повтором тоже не лечится:
// runner завершается с причиной.
func TestInvalidIDRejectionIsFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	plane := &rejectPlane{code: codes.InvalidArgument, msg: "идентификатор runner'а длиннее 128 символов"}
	pb.RegisterRunnerServiceServer(srv, plane)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = Run(ctx, Config{PlaneAddr: ln.Addr().String(), Token: "rrt_test", ID: "r1", Agent: AgentRivet,
		Adapter: AdapterNative, Workdir: t.TempDir()})
	if !errors.Is(err, ErrRegistrationRejected) || !strings.Contains(err.Error(), "длиннее 128") {
		t.Fatalf("ждём фатальный отказ, получено: %v", err)
	}
	if n := plane.calls.Load(); n != 1 {
		t.Fatalf("регистрация повторялась %d раз", n)
	}
}
