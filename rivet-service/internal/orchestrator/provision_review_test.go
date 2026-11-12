package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func TestHostBillingSchedulesNextPoll(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.toReady()
	r.tick(1)
	if next := r.host().NextAttemptAt; next.Before(r.e.Now().Add(5 * time.Hour)) {
		t.Fatalf("стоимость должна проверяться через 6 часов: %v", next)
	}
}

func TestHostDeleteClearsCreationAction(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.toReady()
	if _, err := r.act(HostActDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	h, err := r.act(HostActConfirmDelete, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if h.ActionID != "" || h.DeleteSentAt != nil {
		t.Fatalf("удаление не должно опрашивать старое создание: %+v", h)
	}
}

func TestHostFailedBootstrapPinsAcceptedKey(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	r.tick(5)
	job := r.lastHostJob()
	key := "10.77.0.5 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKey"
	if err := r.e.OnHostJobResult(context.Background(), "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: false, HostKey: key, Output: "script failed"}); err != nil {
		t.Fatal(err)
	}
	if h := r.host(); h.HostKey != key {
		t.Fatalf("ключ первого подключения потерян: %+v", h)
	}
	r.tick(1)
	retry := r.lastHostJob()
	if retry.AcceptNewHostKey || retry.HostKey != key {
		t.Fatalf("повтор должен проверять закреплённый ключ: %+v", retry)
	}
}

func TestLocalHostStaysOnBootstrapMachine(t *testing.T) {
	ctx := context.Background()
	r := newHostRig(t, domain.ProvisionLocal)
	r.request()
	r.tick(2)
	job := r.lastHostJob()
	if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if r.host().LocalRunner != "deployer" {
		t.Fatal("машина bootstrap не сохранена")
	}
	if err := r.st.UpsertRunner(ctx, domain.Runner{ID: "other", Capabilities: []string{"deploy"}, Features: []string{"host_jobs", "monitor"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Pool.Exec(ctx, `UPDATE runners SET status='offline' WHERE id='deployer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.EnqueueDeployment(ctx, r.env.ID, "main", "test"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.st.StartNextDeployment(ctx); err != nil || ok {
		t.Fatalf("публикация не может уйти на другую машину: %v %v", ok, err)
	}
	if id, ok, err := r.st.HostRunner(ctx, r.env); err != nil || ok {
		t.Fatalf("задание хоста не может уйти на %q: %v %v", id, ok, err)
	}
	if id, ok, err := r.st.MonitorRunner(ctx, r.env, false); err != nil || ok {
		t.Fatalf("наблюдение не может уйти на %q: %v %v", id, ok, err)
	}
}
