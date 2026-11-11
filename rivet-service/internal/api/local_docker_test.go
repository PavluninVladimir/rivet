package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestLocalDockerEnvironmentAPI(t *testing.T) {
	f := seedEnvAPI(t)
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	member := loginSession(t, f.srv, f.member, "pw-testpass")
	endpoint := f.srv.URL + "/api/v1/projects/" + f.projectID + "/environments"
	body := map[string]any{"name": "local", "exec_type": "local_docker", "config": map[string]any{"local_docker": map[string]any{"container_port": 8080}}}
	resp, _ := call(t, "POST", endpoint, member, "", body)
	mustStatus(t, resp, http.StatusForbidden, "member cannot configure delivery")
	resp, raw := call(t, "POST", endpoint, admin, "", body)
	mustStatus(t, resp, http.StatusCreated, "local docker")
	var env struct {
		ID     string           `json:"id"`
		Config domain.EnvConfig `json:"config"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Config.LocalDocker == nil || env.Config.LocalDocker.Recipe != "auto" || env.Config.LocalDocker.HealthPath != "/" {
		t.Fatalf("missing defaults: %s", raw)
	}
	for i, config := range []map[string]any{
		{"local_docker": map[string]any{"container_port": 0}},
		{"local_docker": map[string]any{"container_port": 8080, "network_mode": "host"}},
		{"local_docker": map[string]any{"container_port": 8080}, "network_mode": "host"},
		{"local_docker": map[string]any{"container_port": 8080}, "deploy_cmd": "true"},
	} {
		body["name"] = fmt.Sprintf("invalid-%d", i)
		body["config"] = config
		resp, raw = call(t, "POST", endpoint, admin, "", body)
		mustStatus(t, resp, http.StatusUnprocessableEntity, "unsafe docker config")
		var errBody struct {
			Field string `json:"field"`
		}
		_ = json.Unmarshal(raw, &errBody)
		if errBody.Field == "" {
			t.Fatalf("no field for invalid config: %s", raw)
		}
	}
	resp, _ = call(t, "PATCH", f.srv.URL+"/api/v1/environments/"+env.ID, admin, "", map[string]any{"config": map[string]any{"local_docker": map[string]any{"container_port": 9000}}})
	mustStatus(t, resp, http.StatusOK, "edit local config")
}

func TestLocalPublicationAPI(t *testing.T) {
	f := seedEnvAPI(t)
	ctx := context.Background()
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	member := loginSession(t, f.srv, f.member, "pw-testpass")
	other := loginSession(t, f.srv, f.mallory, "pw-testpass")
	env, e := f.st.CreateEnvironment(ctx, domain.Environment{ProjectID: f.projectID, Name: "docker", ExecType: domain.ExecLocalDocker, Trigger: "manual", Config: domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}})
	if e != nil {
		t.Fatal(e)
	}
	base := f.srv.URL + "/api/v1/environments/" + env.ID
	for _, action := range []string{"publish", "redeploy", "restart", "stop"} {
		resp, _ := call(t, "POST", base+"/"+action, other, "", nil)
		mustStatus(t, resp, 404, "foreign operation")
	}
	resp, raw := call(t, "POST", base+"/publish", member, "", nil)
	mustStatus(t, resp, 409, "no driver")
	if !strings.Contains(string(raw), "no_runner") {
		t.Fatal(string(raw))
	}
	if e = f.st.UpsertRunner(ctx, domain.Runner{ID: "docker", Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1"}}); e != nil {
		t.Fatal(e)
	}
	resp, raw = call(t, "POST", base+"/deploy", member, "", nil)
	mustStatus(t, resp, 202, "deploy alias")
	var pub store.LocalPublication
	if e = json.Unmarshal(raw, &pub); e != nil {
		t.Fatal(e)
	}
	if pub.Status != "queued" || pub.Generation != 1 {
		t.Fatal(string(raw))
	}
	resp, raw = call(t, "POST", base+"/publish", admin, "", nil)
	mustStatus(t, resp, 409, "concurrent publish")
	if !strings.Contains(string(raw), "publication_in_progress") {
		t.Fatal(string(raw))
	}
	resp, _ = call(t, "GET", base+"/publication", other, "", nil)
	mustStatus(t, resp, 404, "foreign status")
	resp, _ = call(t, "GET", base+"/publication", member, "", nil)
	mustStatus(t, resp, 200, "member status")
	for _, q := range []string{"tail=0", "tail=5001", "tail=bad", "stream=unknown"} {
		resp, _ = call(t, "GET", base+"/publication/logs?"+q, member, "", nil)
		mustStatus(t, resp, 422, "invalid log query")
	}
	resp, _ = call(t, "POST", base+"/redeploy", member, "", map[string]any{"version": "outside"})
	mustStatus(t, resp, 422, "history-only version")
}
