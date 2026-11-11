package localdocker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func fixtureJob() Job {
	return Job{ProjectID: "test-project", EnvironmentID: "test-environment", PublicationID: "test-publication", Version: strings.Repeat("a", 40), Generation: 1, Config: domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}, Port: 20123, DesiredState: "running"}
}
func inspection(j Job, ip, mode string) string {
	c := Inspection{ID: strings.Repeat("a", 64), Image: "sha256:" + strings.Repeat("b", 64)}
	c.Config.Labels = j.labels()
	c.HostConfig.NetworkMode = mode
	c.State.Running = true
	c.NetworkSettings.Ports = map[string][]PortBinding{"8080/tcp": {{ip, strconv.Itoa(j.Port)}}}
	b, _ := json.Marshal([]Inspection{c})
	return string(b)
}
func TestInspectLoopbackAndOwnership(t *testing.T) {
	j := fixtureJob()
	for _, ip := range []string{"127.0.0.1", "::1", "", "0.0.0.0", "::", "192.168.1.2"} {
		_, e := ParseInspection(inspection(j, ip, "bridge"), j)
		if (e == nil) != (ip == "127.0.0.1" || ip == "::1") {
			t.Fatalf("%q: %v", ip, e)
		}
	}
	for _, mode := range []string{"host", "macvlan", "ipvlan", "container:id"} {
		if _, e := ParseInspection(inspection(j, "127.0.0.1", mode), j); e == nil {
			t.Fatal(mode)
		}
	}
	raw := inspection(j, "127.0.0.1", "bridge")
	j.PublicationID = "another"
	if _, e := ParseInspection(raw, j); e == nil {
		t.Fatal("accepted foreign container")
	}
}
func TestProbeNoRedirect(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	if e := (&Runtime{}).probe(context.Background(), redirect.URL); e != nil {
		t.Fatal(e)
	}
	if called {
		t.Fatal("followed redirect")
	}
}
func TestLogBound(t *testing.T) {
	b := &tailBuffer{}
	_, _ = b.Write([]byte(strings.Repeat("x", LogLimit+10)))
	_, _ = b.Write([]byte("last"))
	if len(b.data) != LogLimit || !strings.HasSuffix(string(b.data), "last") {
		t.Fatal("unbounded tail")
	}
}
func TestStopNeverTouchesForeignObject(t *testing.T) {
	j := fixtureJob()
	raw := inspection(j, "127.0.0.1", "bridge")
	j.PublicationID = "different"
	mutated := false
	r := Runtime{Command: func(ctx context.Context, a []string, w io.Writer) (string, error) {
		if a[0] == "info" {
			return "ok", nil
		}
		if a[1] == "inspect" {
			return raw, nil
		}
		mutated = true
		return "", nil
	}}
	res := r.Operate(context.Background(), "stop", j)
	if res.Status != "failed" || mutated {
		t.Fatalf("%+v mutated=%v", res, mutated)
	}
}

func TestDockerLifecycle(t *testing.T) {
	if os.Getenv("RIVET_DOCKER_TEST") != "1" {
		t.Skip("RIVET_DOCKER_TEST=1 enables isolated real Docker fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	j := fixtureJob()
	j.EnvironmentID = "fixture-" + suffix
	j.PublicationID = "fixture-pub-" + suffix
	j.Config.StartupTimeoutS = 15
	r := &Runtime{}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		for _, gen := range []int64{1, 2} {
			jj := j
			jj.Generation = gen
			_ = exec.CommandContext(c, "docker", "container", "rm", "--force", jj.name()).Run()
			_ = exec.CommandContext(c, "docker", "image", "rm", "--force", fmt.Sprintf("rivet/%s:g%d", j.EnvironmentID, gen)).Run()
		}
		_ = exec.CommandContext(c, "docker", "volume", "rm", "rivet-data-"+j.EnvironmentID).Run()
	})
	root := files(t, map[string]string{"go.mod": "module fixture\ngo 1.26.11\n", "main.go": `package main
import("net/http";"os")
func main(){http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){p:="/data/marker";b,_:=os.ReadFile(p);if len(b)==0{b=[]byte("persisted");_ = os.WriteFile(p,b,0600)};_,_=w.Write(b)});_ = http.ListenAndServe(":"+os.Getenv("PORT"),nil)}
`})
	res := r.Publish(ctx, root, j)
	if res.Status != "running" {
		t.Fatalf("publish: %+v", res)
	}
	j.ContainerID = res.ContainerID
	j.Image = res.Image
	j.Port = res.Port
	c, e := r.inspect(ctx, j)
	if e != nil || !c.State.Running || c.Config.User != "65532:65532" {
		t.Fatalf("inspect: %+v %v", c, e)
	}
	if e := r.probe(ctx, res.HealthURL); e != nil {
		t.Fatal(e)
	}
	if v := r.Operate(ctx, "restart", j); v.Status != "running" || v.Port != j.Port {
		t.Fatalf("restart %+v", v)
	}
	resp, e := http.Get(res.HealthURL)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "persisted" {
		t.Fatal(string(b))
	}
	bad := files(t, map[string]string{"go.mod": "module bad\ngo 1.26.11\n", "main.go": "syntax error"})
	next := j
	next.Generation = 2
	next.PublicationID = "fixture-next-" + suffix
	next.ContainerID = ""
	next.Image = ""
	next.Port = 0
	if v := r.Publish(ctx, bad, next); v.Status != "failed" || v.Reason.Code != "build_failed" {
		t.Fatalf("bad build: %+v", v)
	}
	if e := r.probe(ctx, res.HealthURL); e != nil {
		t.Fatalf("failed redeploy stopped old generation: %v", e)
	}
	if v := r.Operate(ctx, "stop", j); v.Status != "stopped" {
		t.Fatalf("stop: %+v", v)
	}
	j.DesiredState = "stopped"
	v := r.Operate(ctx, "reconcile", j)
	if v.Status != "stopped" {
		t.Fatalf("stopped reconcile: %+v", v)
	}
	raw, e := exec.CommandContext(ctx, "docker", "container", "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}} {{.State.Running}}", j.ContainerID).Output()
	if e != nil || strings.TrimSpace(string(raw)) != "no false" {
		t.Fatalf("stop policy %s %v", raw, e)
	}
}

// Запускать только на одноразовой VM CI. На рабочей машине общий Docker
// не перезапускаем: там могут работать посторонние приложения.
func TestDockerDaemonRecovery(t *testing.T) {
	if os.Getenv("RIVET_DOCKER_DAEMON_RESTART_TEST") != "1" || os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("requires disposable GitHub Actions VM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r := &Runtime{}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	root := files(t, map[string]string{"index.html": "daemon recovery"})
	live := fixtureJob()
	live.EnvironmentID = "recovery-live-" + suffix
	live.PublicationID = "recovery-pub-" + suffix
	live.Config.Recipe = "static"
	live.Config.StartupTimeoutS = 30
	stopped := live
	stopped.EnvironmentID = "recovery-stopped-" + suffix
	stopped.PublicationID = "stopped-pub-" + suffix
	jobs := []*Job{&live, &stopped}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		for _, j := range jobs {
			_ = exec.CommandContext(c, "docker", "container", "rm", "--force", j.name()).Run()
			_ = exec.CommandContext(c, "docker", "image", "rm", "--force", fmt.Sprintf("rivet/%s:g1", j.EnvironmentID)).Run()
			_ = exec.CommandContext(c, "docker", "volume", "rm", "rivet-data-"+j.EnvironmentID).Run()
		}
	})
	for _, j := range jobs {
		v := r.Publish(ctx, root, *j)
		if v.Status != "running" {
			t.Fatalf("publish %+v", v)
		}
		j.ContainerID = v.ContainerID
		j.Image = v.Image
		j.Port = v.Port
	}
	if v := r.Operate(ctx, "stop", stopped); v.Status != "stopped" {
		t.Fatalf("stop %+v", v)
	}
	stopped.DesiredState = "stopped"
	if out, e := exec.CommandContext(ctx, "sudo", "systemctl", "restart", "docker").CombinedOutput(); e != nil {
		t.Fatalf("restart isolated daemon: %v %s", e, out)
	}
	var recovered Result
	for range 30 {
		recovered = r.Operate(ctx, "reconcile", live)
		if recovered.Status == "running" {
			break
		}
		time.Sleep(time.Second)
	}
	if recovered.Status != "running" || recovered.ContainerID != live.ContainerID || recovered.Port != live.Port || recovered.Image != live.Image {
		t.Fatalf("recovery %+v", recovered)
	}
	if v := r.Operate(ctx, "reconcile", stopped); v.Status != "stopped" {
		t.Fatalf("resurrected stopped publication %+v", v)
	}
}

func TestDockerRecipes(t *testing.T) {
	if os.Getenv("RIVET_DOCKER_TEST") != "1" {
		t.Skip("explicit Docker recipe fixtures")
	}
	for _, tt := range []struct {
		name string
		data map[string]string
	}{
		{"static", map[string]string{"index.html": "static fixture"}},
		{"node", map[string]string{"package.json": `{"name":"fixture","version":"1.0.0","scripts":{"start":"node server.cjs"}}`, "package-lock.json": `{"name":"fixture","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"fixture","version":"1.0.0"}}}`, "server.cjs": `require('http').createServer((q,s)=>s.end('node fixture')).listen(process.env.PORT,'0.0.0.0')`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			j := fixtureJob()
			j.EnvironmentID = fmt.Sprintf("recipe-%s-%d", tt.name, time.Now().UnixNano())
			j.PublicationID = j.EnvironmentID
			j.Config.Recipe = tt.name
			j.Config.StartupTimeoutS = 10
			r := &Runtime{}
			t.Cleanup(func() {
				c, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				_ = exec.CommandContext(c, "docker", "container", "rm", "--force", j.name()).Run()
				_ = exec.CommandContext(c, "docker", "image", "rm", "--force", fmt.Sprintf("rivet/%s:g1", j.EnvironmentID)).Run()
				_ = exec.CommandContext(c, "docker", "volume", "rm", "rivet-data-"+j.EnvironmentID).Run()
			})
			res := r.Publish(ctx, files(t, tt.data), j)
			if res.Status != "running" {
				t.Fatalf("recipe %s: %+v", tt.name, res)
			}
		})
	}
}
