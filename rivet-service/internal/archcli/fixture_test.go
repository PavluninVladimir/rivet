package archcli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/architecture"
)

// TestMain: с ARCHCLI_AS_RUNNER=1 тестовый бинарь играет «rivet-runner
// architecture …», чтобы скрипты фикстур звали подкоманду без сборки runner'а.
func TestMain(m *testing.M) {
	if os.Getenv("ARCHCLI_AS_RUNNER") == "1" {
		args := os.Args[1:]
		if len(args) > 0 && args[0] == "architecture" {
			args = args[1:]
		}
		os.Exit(Main(args, os.Stdout))
	}
	os.Exit(m.Run())
}

// runnerWrapper — исполняемый «rivet-runner» поверх тестового бинаря.
func runnerWrapper(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "rivet-runner")
	wrapper := "#!/bin/sh\nARCHCLI_AS_RUNNER=1 exec '" + self + "' \"$@\"\n"
	if err := os.WriteFile(bin, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// taskRepo — репозиторий с коммитом на main и веткой задачи.
func taskRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	repo := t.TempDir()
	gitT(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("crm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "seed")
	gitT(t, repo, "checkout", "-q", "-b", "agent/task-1")
	return repo
}

// Регрессия e2e-стенда (ревью Opus, круг 1): worker'ы стенда работают на
// fake-claude. По маркеру задания describe он кладёт описание, не трогает
// файлы вне docs/architecture и печатает корректный stream-json.
func TestFakeClaudeDescribe(t *testing.T) {
	bin := runnerWrapper(t)
	repo := taskRepo(t)
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "fake-claude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Dir = repo
	cmd.Stdin = bytes.NewBufferString(architecture.DescribePrompt())
	cmd.Env = append(os.Environ(), "RIVET_RUNNER_BIN="+bin, "RIVET_HOOK_CMD=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake-claude: %v\n%s", err, out)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("stream-json сломан строкой: %s", line)
		}
	}
	if !bytes.Contains(out, []byte(`"subtype":"success"`)) {
		t.Fatalf("нет успешного результата: %s", out)
	}
	var gate bytes.Buffer
	if code := Gate(&gate, repo, architecture.DefaultDir, "main", true, true); code != ExitOK {
		t.Fatalf("гейт задачи описания после fake-claude: %d\n%s", code, gate.String())
	}
}

// Фикстура fake-агента для задачи описания (add-architecture-c4 1.20,
// add-architecture-erd-sequence-adr 1.13): scripts/fake-describe.sh кладёт
// описание с C4, ERD, сценарием и ADR, оно проходит гейт задачи описания и
// даёт полный снимок.
func TestFakeDescribeFixture(t *testing.T) {
	bin := runnerWrapper(t)
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "fake-describe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	repo := taskRepo(t)

	cmd := exec.Command("sh", script)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "RIVET_RUNNER_BIN="+bin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fake-describe.sh: %v\n%s", err, out)
	}

	var out bytes.Buffer
	if code := Gate(&out, repo, architecture.DefaultDir, "main", true, true); code != ExitOK {
		t.Fatalf("гейт задачи описания: %d\n%s", code, out.String())
	}
	snap, err := architecture.BuildSnapshot(os.DirFS(filepath.Join(repo, architecture.DefaultDir)),
		architecture.SnapshotOptions{Dir: architecture.DefaultDir})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != architecture.SnapshotOK || snap.Data == nil {
		t.Fatalf("снимок фикстуры: %s %v", snap.Status, snap.Defects)
	}
	d := snap.Data
	if len(d.Elements) == 0 || d.HLD == "" || len(d.ERD.Entities) == 0 || len(d.Scenarios) == 0 || len(d.Decisions) == 0 {
		t.Fatalf("фикстура без C4, ERD, сценариев или ADR: элементов %d, сущностей %d, сценариев %d, решений %d",
			len(d.Elements), len(d.ERD.Entities), len(d.Scenarios), len(d.Decisions))
	}
}
