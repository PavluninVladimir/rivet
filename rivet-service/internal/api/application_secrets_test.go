package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

func TestApplicationSecretsAPIWriteOnlyAndPermissions(t *testing.T) {
	f := seedEnvAPI(t)
	ctx := context.Background()
	box, err := secretbox.New("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{St: f.st, Secrets: box}).Handler())
	defer srv.Close()
	admin := loginSession(t, srv, f.admin, "pw-testpass")
	member := loginSession(t, srv, f.member, "pw-testpass")
	other := loginSession(t, srv, f.mallory, "pw-testpass")
	env, err := f.st.CreateEnvironment(ctx, domain.Environment{ProjectID: f.projectID, Name: "app", ExecType: domain.ExecLocalDocker, Trigger: "manual", Config: domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080, Env: map[string]string{"PORT": "8080"}}}})
	if err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/v1/environments/" + env.ID
	const marker = "test-only-unique-application-password"
	body := map[string]any{"set": map[string]string{"ADMIN_PASSWORD": marker}}
	resp, _ := call(t, "PATCH", base+"/secrets", member, "", body)
	mustStatus(t, resp, 403, "member write denied")
	resp, _ = call(t, "GET", base+"/secrets", other, "", nil)
	mustStatus(t, resp, 404, "foreign read denied")
	resp, _ = call(t, "PATCH", base+"/secrets", other, "", body)
	mustStatus(t, resp, 403, "foreign write denied")
	resp, raw := call(t, "PATCH", base+"/secrets", admin, "", body)
	mustStatus(t, resp, 200, "secret saved")
	if strings.Contains(string(raw), marker) || !strings.Contains(string(raw), "ADMIN_PASSWORD") || !strings.Contains(string(raw), "***") {
		t.Fatal("unsafe metadata response")
	}
	for _, path := range []string{base + "/secrets", srv.URL + "/api/v1/projects/" + f.projectID + "/environments"} {
		resp, raw = call(t, "GET", path, member, "", nil)
		mustStatus(t, resp, 200, "member metadata")
		if strings.Contains(string(raw), marker) {
			t.Fatal("secret in read API")
		}
	}
	resp, raw = call(t, "PATCH", base, admin, "", map[string]any{"config": map[string]any{"local_docker": map[string]any{"container_port": 8080, "env": map[string]string{"ADMIN_PASSWORD": "public"}}}})
	mustStatus(t, resp, 422, "secret cannot become public")
	if strings.Contains(string(raw), marker) {
		t.Fatal("secret in error")
	}
	resp, _ = call(t, "PATCH", base+"/secrets", admin, "", map[string]any{"set": map[string]string{"ADMIN_PASSWORD": ""}})
	mustStatus(t, resp, 422, "empty replacement rejected")
	resp, _ = call(t, "PATCH", base+"/secrets", admin, "", map[string]any{"delete": []string{"ADMIN_PASSWORD"}})
	mustStatus(t, resp, 200, "delete")
	resp, raw = call(t, "GET", base+"/secrets", admin, "", nil)
	mustStatus(t, resp, 200, "deleted metadata")
	if string(raw) != "[]\n" {
		t.Fatal("secret still present")
	}
}

func TestApplicationSecretsAPINoKey(t *testing.T) {
	f := seedEnvAPI(t)
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	env, err := f.st.CreateEnvironment(context.Background(), domain.Environment{ProjectID: f.projectID, Name: "app", ExecType: domain.ExecLocalDocker, Trigger: "manual", Config: domain.EnvConfig{LocalDocker: &domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, raw := call(t, "PATCH", f.srv.URL+"/api/v1/environments/"+env.ID+"/secrets", admin, "", map[string]any{"set": map[string]string{"ADMIN_PASSWORD": "test-value"}})
	mustStatus(t, resp, 503, "no encryption key")
	if !strings.Contains(string(raw), "no_secret_key") || strings.Contains(string(raw), "test-value") {
		t.Fatal("unsafe key error")
	}
}
