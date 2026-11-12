package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/localdocker"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

func TestApplicationSecretsSnapshotAssignmentRotation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	actor := mustOwner(t, s)
	box, _ := rotateBox(t)
	nextBox, _ := rotateBox(t)
	const value = "unique-application-value-old"
	patch := ApplicationSecretPatch{Set: map[string]string{"ADMIN_PASSWORD": value}}
	if err := s.PatchApplicationSecrets(ctx, env.ID, patch, nil, actor.ID); !errors.Is(err, secretbox.ErrNoKey) {
		t.Fatal(err)
	}
	if err := s.PatchApplicationSecrets(ctx, env.ID, patch, box, actor.ID); err != nil {
		t.Fatal(err)
	}
	metadata, err := s.ApplicationSecrets(ctx, env.ID)
	if err != nil || len(metadata) != 1 || metadata[0].Masked != "***" {
		t.Fatal(metadata, err)
	}
	raw, _ := json.Marshal(metadata)
	if bytes.Contains(raw, []byte(value)) {
		t.Fatal("plaintext metadata")
	}
	var enc []byte
	if err = s.Pool.QueryRow(ctx, `SELECT value_enc FROM environment_secrets WHERE env_id=$1`, env.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte(value)) {
		t.Fatal("plaintext at rest")
	}
	dockerRunner(t, s)
	if _, err = s.EnqueueLocalPublication(ctx, env.ID, "v1", actor.ID, false); !errors.Is(err, ErrNoDockerRunner) {
		t.Fatal("old runner accepted", err)
	}
	compatible := domain.Runner{ID: "docker", Secure: true, Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1", LocalDockerSecretsFeature}}
	if err = s.UpsertRunner(ctx, compatible); err != nil {
		t.Fatal(err)
	}
	p, err := s.EnqueueLocalPublication(ctx, env.ID, "v1", actor.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	// A queued snapshot cannot be delivered to any unassigned runner.
	if _, _, err = s.LocalSecretsForRunner(ctx, p.ActiveDeploymentID, "docker", box, true); err == nil {
		t.Fatal("unassigned delivery")
	}
	if err = s.PatchApplicationSecrets(ctx, env.ID, ApplicationSecretPatch{Set: map[string]string{"ADMIN_PASSWORD": "new-value"}}, box, actor.ID); err != nil {
		t.Fatal(err)
	}
	// An insecure or old runner must leave the assignment in the queue.
	for _, r := range []domain.Runner{
		{ID: "docker", Capabilities: compatible.Capabilities, Features: compatible.Features},
		{ID: "docker", Secure: true, Capabilities: compatible.Capabilities, Features: []string{"local_docker_v1"}},
		{ID: "docker", Secure: true, Capabilities: []string{"deploy", "docker", "coding"}, Features: compatible.Features},
	} {
		if err = s.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.StartNextDeployment(ctx); err != nil || ok {
			t.Fatal("unsafe assignment", ok, err)
		}
	}
	if err = s.UpsertRunner(ctx, compatible); err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.StartNextDeployment(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, _, err = s.LocalSecretsForRunner(ctx, a.Deployment.ID, "other", box, true); err == nil {
		t.Fatal("foreign delivery")
	}
	values, sensitive, err := s.LocalSecretsForRunner(ctx, a.Deployment.ID, a.RunnerID, box, true)
	if err != nil || !sensitive || values["ADMIN_PASSWORD"] != value {
		t.Fatal("snapshot mismatch", sensitive, err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE runners SET secure=false WHERE id='docker'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.LocalSecretsForRunner(ctx, a.Deployment.ID, "docker", box, true); !errors.Is(err, ErrSecretAssignment) {
		t.Fatal("insecure delivery", err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE runners SET secure=true WHERE id='docker'`); err != nil {
		t.Fatal(err)
	}
	metadataOnly, hidden, err := s.LocalSecretsForRunner(ctx, a.Deployment.ID, a.RunnerID, nil, false)
	if err != nil || !hidden || len(metadataOnly) != 0 {
		t.Fatal("technical operation required a decryption key", err)
	}
	res, err := s.RotateSecrets(ctx, box, nextBox)
	if err != nil || res.ApplicationSecrets != 2 {
		t.Fatal(res, err)
	}
	values, _, err = s.LocalSecretsForRunner(ctx, a.Deployment.ID, a.RunnerID, nextBox, true)
	if err != nil || values["ADMIN_PASSWORD"] != value {
		t.Fatal("rotation snapshot", err)
	}
	if _, _, err = s.LocalSecretsForRunner(ctx, a.Deployment.ID, a.RunnerID, box, true); err == nil {
		t.Fatal("old key accepted")
	}
	var config string
	if err = s.Pool.QueryRow(ctx, `SELECT config::text FROM local_publications WHERE id=$1`, p.ID).Scan(&config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config, "ADMIN_PASSWORD") || strings.Contains(config, value) {
		t.Fatal("secret in public snapshot")
	}
	if _, err = s.Pool.Exec(ctx, `DELETE FROM environments WHERE id=$1`, env.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM environment_secrets)+(SELECT count(*) FROM local_publication_secrets)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("secret orphan", count, err)
	}
}

func TestApplicationSecretsAtomicValidationAndPublicConfig(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	actor := mustOwner(t, s)
	box, _ := rotateBox(t)
	env.Config.LocalDocker.Env = map[string]string{"PORT": "8080"}
	var err error
	env, err = s.UpdateEnvironment(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	set := func(in ApplicationSecretPatch) error {
		return s.PatchApplicationSecrets(ctx, env.ID, in, box, actor.ID)
	}
	if err = set(ApplicationSecretPatch{Set: map[string]string{"ADMIN_PASSWORD": "valid"}}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []ApplicationSecretPatch{
		{Set: map[string]string{"PORT": "secret"}, Delete: []string{"ADMIN_PASSWORD"}},
		{Set: map[string]string{"RIVET_TOKEN": "secret"}},
		{Set: map[string]string{"ADMIN_PASSWORD": ""}},
		{Set: map[string]string{"ADMIN_PASSWORD": "line\nbreak"}},
		{Set: map[string]string{"ADMIN_PASSWORD": "new"}, Delete: []string{"ADMIN_PASSWORD"}},
	} {
		if err = set(in); err == nil {
			t.Fatal("invalid patch accepted")
		}
	}
	items, err := s.ApplicationSecrets(ctx, env.ID)
	if err != nil || len(items) != 1 {
		t.Fatal("failed patch changed secrets", err)
	}
	env.Config.LocalDocker.Env["ADMIN_PASSWORD"] = "public"
	if _, err = s.UpdateEnvironment(ctx, env); err == nil {
		t.Fatal("public overwrite of secret")
	}
	delete(env.Config.LocalDocker.Env, "ADMIN_PASSWORD")
	if err = set(ApplicationSecretPatch{}); err != nil {
		t.Fatal(err)
	}
	if err = set(ApplicationSecretPatch{Delete: []string{"ADMIN_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEnvironment(ctx, env.ID)
	if err != nil || got.Config.LocalDocker.Env["PORT"] != "8080" {
		t.Fatal("public env changed", err)
	}
	items, err = s.ApplicationSecrets(ctx, env.ID)
	if err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
}

func TestApplicationSecretsSuppressStoredOutput(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	env := seedLocal(t, s)
	actor := mustOwner(t, s)
	box, _ := rotateBox(t)
	const marker = "never-store-this-app-value"
	if err := s.PatchApplicationSecrets(ctx, env.ID, ApplicationSecretPatch{Set: map[string]string{"ADMIN_PASSWORD": marker}}, box, actor.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRunner(ctx, domain.Runner{ID: "docker", Secure: true, Capabilities: []string{"deploy", "docker"}, Features: []string{"local_docker_v1", LocalDockerSecretsFeature}}); err != nil {
		t.Fatal(err)
	}
	p, err := s.EnqueueLocalPublication(ctx, env.ID, "v1", actor.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.StartNextDeployment(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if a.Deployment.ID != p.ActiveDeploymentID {
		t.Fatal("wrong snapshot assignment")
	}
	if yes, err := s.DeploymentHasApplicationSecrets(ctx, a.Deployment.ID); err != nil || !yes {
		t.Fatal("output boundary missed secrets", err)
	}
	if err = s.SaveLocalBuilderLog(ctx, a.Deployment.ID, []byte(marker)); err != nil {
		t.Fatal(err)
	}
	p, ok, err = s.AcceptLocalResult(ctx, a.Deployment.ID, "docker", localdocker.Result{Status: "failed", Logs: marker, Reason: &localdocker.Failure{Code: marker, Message: marker}})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	raw, _ := json.Marshal(p)
	if bytes.Contains(raw, []byte(marker)) {
		t.Fatal("secret in publication read")
	}
	for _, stream := range []string{"builder", "container"} {
		logs, err := s.LocalLogs(ctx, env.ID, stream)
		if err != nil || logs != localdocker.SensitiveLogMessage {
			t.Fatal("logs not suppressed", err)
		}
	}
	var persisted string
	if err = s.Pool.QueryRow(ctx, `SELECT builder_log||container_log||reason::text FROM local_publications WHERE id=$1`, p.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, marker) {
		t.Fatal("plaintext output stored")
	}
	if err = s.Pool.QueryRow(ctx, `SELECT COALESCE(string_agg(text||payload::text,''),'') FROM events WHERE project_id=$1`, env.ProjectID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, marker) {
		t.Fatal("plaintext in audit")
	}
}
