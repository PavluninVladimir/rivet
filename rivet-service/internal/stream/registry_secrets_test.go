package stream

import (
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"testing"
)

func TestSecretDeliveryUsesCurrentSecureStream(t *testing.T) {
	r := NewRegistry()
	msg := &pb.PlaneMsg{MsgId: "test-secret-job"}
	insecure := r.Attach("docker")
	if r.SendSecure("docker", msg) {
		t.Fatal("secret sent to insecure stream")
	}
	if len(insecure) != 0 {
		t.Fatal("insecure stream received secret")
	}
	secure := r.AttachSecure("docker", true)
	if !r.SendSecure("docker", msg) {
		t.Fatal("secure delivery failed")
	}
	if got := <-secure; got != msg {
		t.Fatal("wrong message")
	}
	replacement := r.AttachSecure("docker", false)
	if r.SendSecure("docker", msg) || len(replacement) != 0 {
		t.Fatal("reconnect leaked secret")
	}
	r.Detach("docker", secure)
	if !r.Current("docker", replacement) {
		t.Fatal("stale detach removed current stream")
	}
	r.Detach("docker", replacement)
	if r.SendSecure("docker", msg) {
		t.Fatal("disconnected delivery")
	}
}
