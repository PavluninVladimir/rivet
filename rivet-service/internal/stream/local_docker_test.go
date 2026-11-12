package stream

import (
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"testing"
)

func TestLocalDockerDeliveryRegistration(t *testing.T) {
	for _, tt := range []struct {
		caps, features []string
		want           bool
	}{
		{[]string{"deploy", "docker"}, []string{"local_docker_v1"}, true},
		{[]string{"deploy", "docker"}, nil, false},
		{[]string{"deploy", "docker", "coding"}, []string{"local_docker_v1"}, false},
		{[]string{"docker"}, []string{"local_docker_v1"}, false},
	} {
		_, ok := agentAllowed(&pb.RegisterRequest{Capabilities: tt.caps, Features: tt.features})
		if ok != tt.want {
			t.Fatalf("%v %v: %v", tt.caps, tt.features, ok)
		}
	}
}
