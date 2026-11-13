package stream

import (
	"github.com/PavluninVladimir/rivet/internal/loadtest"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestDedicatedLoadRegistration(t *testing.T) {
	req := &pb.RegisterRequest{Capabilities: []string{"load"}, Features: []string{loadtest.Feature}, BootId: "boot"}
	if why, ok := agentAllowed(req); !ok {
		t.Fatal(why)
	}
	for _, mutate := range []func(*pb.RegisterRequest){func(r *pb.RegisterRequest) { r.Capabilities = []string{"load", "coding"} }, func(r *pb.RegisterRequest) { r.Features = nil }, func(r *pb.RegisterRequest) { r.Model = "model" }, func(r *pb.RegisterRequest) { r.Models = []string{"model"} }, func(r *pb.RegisterRequest) { r.BootId = "" }} {
		r := proto.Clone(req).(*pb.RegisterRequest)
		mutate(r)
		if _, ok := agentAllowed(r); ok {
			t.Fatal("unsafe dedicated registration", r.String())
		}
	}
}
