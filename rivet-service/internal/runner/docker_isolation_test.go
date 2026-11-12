package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"google.golang.org/protobuf/proto"
)

func TestDockerDriverRejectsHostAndMonitorScripts(t *testing.T) {
	a := &agent{cfg: Config{Capabilities: []string{"deploy", "docker"}, Workdir: t.TempDir()}}
	features := a.cfg.features()
	if !slices.Contains(features, "local_docker_v1") || slices.Contains(features, featureHostJobs) || slices.Contains(features, featureMonitor) {
		t.Fatalf("driver advertised shell operations: %v", features)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	script := "touch " + strconv.Quote(marker)
	if _, err := a.runHostJob(context.Background(), &pb.HostJob{Script: script}, func([]byte) {}); err == nil {
		t.Fatal("host script accepted")
	}
	if _, err := a.runMonitorCmd(context.Background(), &pb.MonitorJob{}, script, time.Second); err == nil {
		t.Fatal("monitor script accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("script was executed", err)
	}
}

func TestReleasedDockerJobWireCompatibility(t *testing.T) {
	// Выпущенный v12: bytes local_docker_job=15, string local_docker_action=16.
	wire := []byte{0x7a, 2, '{', '}', 0x82, 1, 7, 'p', 'u', 'b', 'l', 'i', 's', 'h'}
	var job pb.DeployJob
	if err := proto.Unmarshal(wire, &job); err != nil {
		t.Fatal(err)
	}
	if string(job.LocalDockerJob) != "{}" || job.LocalDockerAction != "publish" || job.HostKey != "" {
		t.Fatal("released Docker message was reinterpreted")
	}
}
