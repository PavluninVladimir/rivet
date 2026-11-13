package localdocker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDockerRecipesPythonAuto(t *testing.T) {
	if os.Getenv("RIVET_DOCKER_TEST") != "1" {
		t.Skip("RIVET_DOCKER_TEST=1 enables isolated Python HTTP fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	j := fixtureJob()
	j.EnvironmentID = fmt.Sprintf("python-fixture-%d", time.Now().UnixNano())
	j.PublicationID = j.EnvironmentID
	j.Config.Recipe = "auto"
	j.Config.ContainerPort = 8097
	j.Config.HealthPath = "/health"
	j.Config.StartupTimeoutS = 15
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = exec.CommandContext(c, "docker", "container", "rm", "--force", j.name()).Run()
		_ = exec.CommandContext(c, "docker", "image", "rm", "--force", fmt.Sprintf("rivet/%s:g1", j.EnvironmentID)).Run()
		_ = exec.CommandContext(c, "docker", "volume", "rm", "rivet-data-"+j.EnvironmentID).Run()
	})
	root := files(t, map[string]string{
		"requirements.txt": "idna==3.10\n",
		"index.html":       "static placeholder must not serve instead of Python",
		"Dockerfile":       "FROM ignored-user-image",
		"app.py": `import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer
import idna

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            with open('/app/forbidden-write', 'w') as f:
                f.write('unexpected')
            read_only = False
        except OSError:
            read_only = True
        body = json.dumps({'uid': os.getuid(), 'port': int(os.environ['PORT']),
                           'host': os.environ['HOST'], 'dependency': idna.__version__,
                           'read_only': read_only}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(body)

HTTPServer((os.environ['HOST'], int(os.environ['PORT'])), Handler).serve_forever()
`,
	})
	r := &Runtime{}
	res := r.Publish(ctx, root, j)
	if res.Status != "running" {
		t.Fatalf("Python auto publication failed: %+v", res)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, res.HealthURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got struct {
		UID        int    `json:"uid"`
		Port       int    `json:"port"`
		Host       string `json:"host"`
		Dependency string `json:"dependency"`
		ReadOnly   bool   `json:"read_only"`
	}
	if err = json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || got.UID != 65532 || got.Port != 8097 || got.Host != "0.0.0.0" || got.Dependency != "3.10" || !got.ReadOnly {
		t.Fatalf("unexpected Python HTTP/runtime result: status=%d %+v", response.StatusCode, got)
	}
	t.Logf("auto Python served HTTP on PORT=%d as UID=%d; requirements installed; app tree unwritable", got.Port, got.UID)
}
