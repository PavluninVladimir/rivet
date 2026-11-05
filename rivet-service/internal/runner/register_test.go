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
	"google.golang.org/grpc/status"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// rejectPlane — control plane, который отклоняет регистрацию так же, как
// rivetd стороннему агенту: FAILED_PRECONDITION с причиной.
type rejectPlane struct {
	pb.UnimplementedRunnerServiceServer
	calls atomic.Int32
}

func (p *rejectPlane) Register(context.Context, *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	p.calls.Add(1)
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
