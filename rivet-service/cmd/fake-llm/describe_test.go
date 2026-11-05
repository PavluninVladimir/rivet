package main

import (
	"bytes"
	"context"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/archcli"
	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/runner/native"
)

// TestMain: тестовый бинарь играет «rivet-runner architecture …» для
// команды run сценария Архитектора (RIVET_RUNNER_BIN).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "architecture" {
		os.Exit(archcli.Main(os.Args[2:], os.Stdout))
	}
	os.Exit(m.Run())
}

func describeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Задача описания архитектуры (add-architecture-c4) на агенте Rivet: шаг
// describe идёт настоящим агентским циклом по сценарию fake-llm, описание
// пишется инструментами, представления строит подкоманда runner'а через
// RIVET_RUNNER_BIN. Итог проходит гейт задачи описания (-force -only-docs)
// и даёт полный снимок: C4, ERD, сценарии и ADR. Это замена fake-describe.sh
// сторонних fake-агентов.
func TestNativeArchitectureDescribe(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	repo := t.TempDir()
	describeGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("crm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	describeGit(t, repo, "add", "-A")
	describeGit(t, repo, "commit", "-q", "-m", "seed")
	describeGit(t, repo, "checkout", "-q", "-b", "agent/task-1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	var events []native.Event
	res, err := native.Run(context.Background(), native.Config{
		Client: llm.Client{API: llm.APIOpenAI, BaseURL: srv.URL + "/v1", Key: "fake-key"},
		Model:  "fake-small", Dir: repo,
		Exec: native.ExecConfig{KillGrace: 200 * time.Millisecond,
			ReadOnlyCaches: []native.CacheDir{{Env: "RIVET_RUNNER_BIN", Dir: self}}},
		OnStep: func(ev native.Event) { events = append(events, ev) },
	}, "Твоя роль в команде Rivet: Архитектор.\n# Задача\n"+architecture.DescribePrompt())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != "ok" {
		t.Fatalf("итог Архитектора: %+v", res)
	}
	ran := false
	for _, ev := range events {
		if ev.Kind == "tool" && ev.Tool == "run" {
			if !ev.OK {
				t.Fatalf("views и validate упали: %+v", ev)
			}
			ran = true
		}
	}
	if !ran {
		t.Fatal("Архитектор не запускал подкоманду runner'а")
	}
	for _, f := range strings.Fields(describeGit(t, repo, "status", "--porcelain", "--untracked-files=all")) {
		if f != "??" && !strings.HasPrefix(f, architecture.DefaultDir+"/") {
			t.Errorf("Архитектор тронул файл вне описания: %s", f)
		}
	}

	var gate bytes.Buffer
	if code := archcli.Gate(&gate, repo, architecture.DefaultDir, "main", true, true); code != archcli.ExitOK {
		t.Fatalf("гейт задачи описания: %d\n%s", code, gate.String())
	}
	snap, err := architecture.BuildSnapshot(os.DirFS(filepath.Join(repo, architecture.DefaultDir)),
		architecture.SnapshotOptions{Dir: architecture.DefaultDir})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != architecture.SnapshotOK || snap.Data == nil {
		t.Fatalf("снимок описания: %s %v", snap.Status, snap.Defects)
	}
	d := snap.Data
	if len(d.Elements) == 0 || d.HLD == "" || len(d.ERD.Entities) == 0 || len(d.Scenarios) == 0 || len(d.Decisions) == 0 {
		t.Fatalf("описание без C4, ERD, сценариев или ADR: элементов %d, сущностей %d, сценариев %d, решений %d",
			len(d.Elements), len(d.ERD.Entities), len(d.Scenarios), len(d.Decisions))
	}
}

// Копия фикстуры в fake-llm совпадает с эталоном демо-CRM в
// internal/architecture/testdata/crm (без представлений): описание на
// стенде и в тестах архитектуры одно и то же.
func TestDescribeFixtureMatchesArchitectureTestdata(t *testing.T) {
	ref := filepath.Join("..", "..", "internal", "architecture", "testdata", "crm")
	want := map[string]bool{}
	for _, part := range []string{"architecture.yaml", "model", "scenarios", "adr"} {
		err := filepath.WalkDir(filepath.Join(ref, part), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(ref, p)
			want[filepath.ToSlash(rel)] = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	err := fs.WalkDir(describeFixture, "describe", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "describe/")
		got[rel] = true
		b, _ := describeFixture.ReadFile(p)
		orig, err := os.ReadFile(filepath.Join(ref, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(b, orig) {
			t.Errorf("describe/%s расходится с эталоном: %v", rel, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range want {
		if !got[rel] {
			t.Errorf("в фикстуре fake-llm нет %s из эталона", rel)
		}
	}
}
