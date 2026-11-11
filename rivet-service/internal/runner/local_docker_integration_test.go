package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/api"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/PavluninVladimir/rivet/internal/stream"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"github.com/jackc/pgx/v5"
)

type dockerSender struct{ jobs chan *pb.DeployJob }

func (s dockerSender) Send(_ string, m *pb.PlaneMsg) bool {
	if job := m.GetDeploy(); job != nil {
		s.jobs <- job
		return true
	}
	return false
}
func TestLocalDockerAPIToContainer(t *testing.T) {
	if os.Getenv("RIVET_DOCKER_TEST") != "1" {
		t.Skip("explicit isolated Docker e2e fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	base := os.Getenv("RIVET_DATABASE_URL")
	if base == "" {
		base = "postgres://rivet:rivet@127.0.0.1:5432/rivet?sslmode=disable"
	}
	admin, e := pgx.Connect(ctx, base)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(context.Background())
	name := fmt.Sprintf("rivet_local_e2e_%d", time.Now().UnixNano())
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+name); e != nil {
		t.Fatal(e)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") }()
	cfg, parseErr := pgx.ParseConfig(base)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	testURL := &url.URL{Scheme: "postgres", User: url.UserPassword(cfg.User, cfg.Password), Host: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), Path: "/" + name, RawQuery: "sslmode=disable"}
	dbURL := testURL.String()
	guard, e := pgx.Connect(ctx, dbURL)
	if e != nil {
		t.Fatal(e)
	}
	var actualDB string
	e = guard.QueryRow(ctx, "SELECT current_database()").Scan(&actualDB)
	_ = guard.Close(ctx)
	if e != nil || actualDB != name {
		t.Fatal("refuse to migrate non-fixture database", actualDB, e)
	}
	if e = store.Migrate(ctx, dbURL); e != nil {
		t.Fatal(e)
	}
	st, e := store.New(ctx, dbURL)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	root := t.TempDir()
	repos := filepath.Join(root, "repos")
	bare := filepath.Join(repos, "fixture", "app.git")
	seed := filepath.Join(root, "seed")
	if e = os.MkdirAll(filepath.Dir(bare), 0755); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(seed, 0755); e != nil {
		t.Fatal(e)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_EMAIL=fixture@example.test")
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "--bare", "-b", "main", bare)
	git(seed, "init", "-b", "main")
	app := `package main
import("net/http";"os")
func main(){http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){b,_:=os.ReadFile("/data/marker");if len(b)==0{b=[]byte("durable");_ = os.WriteFile("/data/marker",b,0600)};_,_=w.Write(b)});_ = http.ListenAndServe(":"+os.Getenv("PORT"),nil)}`
	if e = os.WriteFile(filepath.Join(seed, "go.mod"), []byte("module fixture\ngo 1.26.11\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(seed, "main.go"), []byte(app), 0644); e != nil {
		t.Fatal(e)
	}
	git(seed, "add", ".")
	git(seed, "commit", "-m", "first tested artifact")
	git(seed, "push", bare, "main")
	sha := git(seed, "rev-parse", "HEAD")
	out := dockerSender{make(chan *pb.DeployJob, 20)}
	engine := orchestrator.New(st, scm.NewFakeGit(repos, "fixture/app"), nil, out, 90*time.Second)
	if e = st.Bootstrap(ctx, "fixture", "fixture-test-password"); e != nil {
		t.Fatal(e)
	}
	owner, e := st.Authenticate(ctx, "fixture", "fixture-test-password")
	if e != nil {
		t.Fatal(e)
	}
	p, e := st.CreateProject(ctx, "Docker fixture", "fixture/app", nil, owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.Pool.Exec(ctx, `UPDATE projects SET provider='fake' WHERE id=$1`, p.ID); e != nil {
		t.Fatal(e)
	}
	if e = st.UpsertRunner(ctx, domain.Runner{ID: "docker-fixture", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1"}}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer((&api.Server{St: st, Engine: engine, Hub: stream.NewHub()}).Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		var b io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			b = bytes.NewReader(data)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+"/api/v1"+path, b)
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, res.StatusCode, want, data)
		}
		return data
	}
	call("POST", "/auth/login", map[string]string{"login": "fixture", "password": "fixture-test-password"}, 200)
	ep, e := st.CreateEpic(ctx, p.ID, "Build app", "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.CreateTask(ctx, ep.ID, store.NewTask{Title: "Build first artifact"}); e != nil {
		t.Fatal(e)
	}
	call("POST", "/epics/"+ep.ID+"/start", nil, 200)
	envs, e := st.ListEnvironments(ctx, p.ID)
	if e != nil || len(envs) != 1 {
		t.Fatal(envs, e)
	}
	env := envs[0]
	// Cleanup only the fixture scope, even when a test assertion fails.
	defer func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		for _, kind := range []string{"container", "image"} {
			a := []string{kind, "ls", "--quiet", "--no-trunc", "--filter", "label=rivet.environment=" + env.ID}
			if kind == "container" {
				a = append(a, "--all")
			}
			raw, _ := exec.CommandContext(c, "docker", a...).Output()
			for _, id := range strings.Fields(string(raw)) {
				_ = exec.CommandContext(c, "docker", kind, "rm", "--force", id).Run()
			}
		}
		_ = exec.CommandContext(c, "docker", "volume", "rm", "rivet-data-"+env.ID).Run()
	}()
	a := &agent{cfg: Config{Capabilities: []string{"deploy", "docker"}, Workdir: root, GitBase: "file://" + repos + "/"}}
	pump := func() {
		t.Helper()
		for range 50 {
			if _, e := st.TouchRunner(ctx, "docker-fixture", nil); e != nil {
				t.Fatal(e)
			}
			if e := engine.Tick(ctx); e != nil {
				t.Fatal(e)
			}
			processed := 0
			for len(out.jobs) > 0 {
				processed++
				job := <-out.jobs
				a.executeDeploy(ctx, job, func(m *pb.RunnerMsg) {
					if tr := m.GetTranscript(); tr != nil {
						engine.OnDeployTranscript(tr.DeployId, tr.Data)
					}
					if dr := m.GetDeployResult(); dr != nil {
						if e := engine.OnDeployResult(ctx, "docker-fixture", dr); e != nil {
							t.Fatal(e)
						}
					}
				})
			}
			var queued int
			if e := st.Pool.QueryRow(ctx, `SELECT count(*) FROM deployments WHERE env_id=$1 AND status IN('queued','deploying','verifying')`, env.ID).Scan(&queued); e != nil {
				t.Fatal(e)
			}
			if queued == 0 && processed == 0 {
				return
			}
		}
		t.Fatal("queue did not drain")
	}
	pump()
	if status, e := st.LocalPreparation(ctx, env.ID); e != nil || status != "waiting_artifact" {
		t.Fatal(status, e)
	}
	if _, e = st.LocalPublication(ctx, env.ID); e == nil {
		t.Fatal("preparation invented a publication")
	}
	if e = st.EnqueueAutoDeployments(ctx, p.ID, sha); e != nil {
		t.Fatal(e)
	}
	pump()
	path := "/environments/" + env.ID
	read := func() store.LocalPublication {
		t.Helper()
		var v store.LocalPublication
		if e := json.Unmarshal(call("GET", path+"/publication", nil, 200), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	first := read()
	if first.Status != "running" || first.Listen == nil {
		t.Fatalf("%+v", first)
	}
	checkApp := func(v store.LocalPublication) {
		t.Helper()
		res, e := http.Get("http://" + *v.Listen)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if string(b) != "durable" {
			t.Fatal(string(b))
		}
	}
	checkApp(first)
	if preview := os.Getenv("RIVET_DOCKER_E2E_PREVIEW_FILE"); preview != "" {
		data, _ := json.Marshal(map[string]string{"api_url": srv.URL, "project_id": p.ID, "environment_id": env.ID})
		if e = os.WriteFile(preview, data, 0600); e != nil {
			t.Fatal(e)
		}
		for {
			if _, e = st.TouchRunner(ctx, "docker-fixture", nil); e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(preview + ".done"); e == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("preview deadline")
			case <-time.After(time.Second):
			}
		}
	}
	call("POST", path+"/restart", nil, 202)
	pump()
	restarted := read()
	if restarted.Image != first.Image || restarted.Listen == nil || *restarted.Listen != *first.Listen {
		t.Fatal("restart changed generation")
	}
	checkApp(restarted)
	call("POST", path+"/redeploy", nil, 202)
	pump()
	second := read()
	if second.Status != "running" || second.Generation != 2 || second.Listen == nil || *second.Listen == *first.Listen {
		t.Fatalf("redeploy %+v", second)
	}
	checkApp(second)
	if e = os.WriteFile(filepath.Join(seed, "main.go"), []byte("syntax error"), 0644); e != nil {
		t.Fatal(e)
	}
	git(seed, "add", ".")
	git(seed, "commit", "-m", "broken fixture")
	git(seed, "push", bare, "main")
	call("POST", path+"/publish", nil, 202)
	pump()
	afterFailure := read()
	if afterFailure.Status != "running" || afterFailure.Reason == nil || afterFailure.Reason.Code != "build_failed" || afterFailure.Listen == nil || *afterFailure.Listen != *second.Listen {
		t.Fatalf("failure %+v", afterFailure)
	}
	checkApp(afterFailure)
	call("GET", path+"/publication/logs?stream=builder&tail=20", nil, 200)
	call("POST", path+"/stop", nil, 200)
	pump()
	stopped := read()
	if stopped.Status != "stopped" || stopped.DesiredState != "stopped" {
		t.Fatalf("stop %+v", stopped)
	}
	call("POST", path+"/stop", nil, 200)
	call("DELETE", path, nil, 202)
	pump()
	call("GET", path+"/publication", nil, 404)
}
