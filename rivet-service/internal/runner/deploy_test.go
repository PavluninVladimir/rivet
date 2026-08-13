package runner

import (
	"context"
	"strings"
	"sync"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// runDeploy — executeDeploy с локальным исполнением, собирает сообщения.
func runDeploy(t *testing.T, job *pb.DeployJob) (results []*pb.DeployResult, transcript string) {
	t.Helper()
	a := &agent{cfg: Config{Workdir: t.TempDir()}}
	var mu sync.Mutex
	var buf strings.Builder
	emit := func(m *pb.RunnerMsg) {
		mu.Lock()
		defer mu.Unlock()
		if dr := m.GetDeployResult(); dr != nil {
			results = append(results, dr)
		}
		if tr := m.GetTranscript(); tr != nil {
			if tr.DeployId != job.DeploymentId {
				t.Errorf("чанк с чужим deploy_id: %q", tr.DeployId)
			}
			buf.Write(tr.Data)
		}
	}
	a.executeDeploy(context.Background(), job, emit)
	return results, buf.String()
}

// Успешная джоба: env-переменные RIVET_* доступны командам, оба этапа ok.
func TestExecuteDeploySuccess(t *testing.T) {
	results, transcript := runDeploy(t, &pb.DeployJob{
		DeploymentId: "dep-1", EnvName: "staging", Repo: "o/r",
		Version: "sha-2", PrevVersion: "sha-1",
		DeployCmd: `echo "deploy $RIVET_ENV -> $RIVET_VERSION (prev $RIVET_PREV_VERSION) repo $RIVET_REPO"`,
		VerifyCmd: "true", TimeoutS: 30,
	})
	if len(results) != 2 || !results[0].Ok || results[0].Stage != pb.DeployResult_DEPLOY ||
		!results[1].Ok || results[1].Stage != pb.DeployResult_VERIFY {
		t.Fatalf("ожидались DEPLOY ok + VERIFY ok: %+v", results)
	}
	if !strings.Contains(transcript, "deploy staging -> sha-2 (prev sha-1) repo o/r") {
		t.Fatalf("env-переменные не дошли до команды: %q", transcript)
	}
}

// Провал доставки: VERIFY не запускается, detail несёт вывод.
func TestExecuteDeployFail(t *testing.T) {
	results, _ := runDeploy(t, &pb.DeployJob{
		DeploymentId: "dep-2", EnvName: "staging",
		DeployCmd: "echo сломалось; exit 3", VerifyCmd: "true", TimeoutS: 30,
	})
	if len(results) != 1 || results[0].Ok || results[0].Stage != pb.DeployResult_DEPLOY {
		t.Fatalf("ожидался единственный DEPLOY !ok: %+v", results)
	}
	if !strings.Contains(results[0].Detail, "сломалось") {
		t.Fatalf("detail без вывода команды: %q", results[0].Detail)
	}
}

// Провал Verify-команды после успешной доставки.
func TestExecuteDeployVerifyFail(t *testing.T) {
	results, _ := runDeploy(t, &pb.DeployJob{
		DeploymentId: "dep-3", EnvName: "staging",
		DeployCmd: "true", VerifyCmd: "exit 1", TimeoutS: 30,
	})
	if len(results) != 2 || !results[0].Ok || results[1].Ok || results[1].Stage != pb.DeployResult_VERIFY {
		t.Fatalf("ожидались DEPLOY ok + VERIFY !ok: %+v", results)
	}
}

// Дедлайн джобы: зависшая команда обрывается, этап проваливается.
func TestExecuteDeployTimeout(t *testing.T) {
	results, _ := runDeploy(t, &pb.DeployJob{
		DeploymentId: "dep-4", EnvName: "staging",
		DeployCmd: "sleep 30", VerifyCmd: "true", TimeoutS: 1,
	})
	if len(results) != 1 || results[0].Ok {
		t.Fatalf("зависшая команда должна провалить DEPLOY: %+v", results)
	}
}
