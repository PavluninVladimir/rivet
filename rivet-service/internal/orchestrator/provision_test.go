package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds/ruvdstest"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Хосты окружений на fake RUVDS (add-environment-provisioning). Главное
// правило: без решения человека нет ни одного запроса на создание и
// удаление сервера: ни тиками, ни после рестарта, ни повтором.

// fakePubKey: заведомо фальшивый публичный ключ deploy-runner'а.
const fakePubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForRivetTestsOnly0000000000000 deploy@runner"

type hostRig struct {
	t    *testing.T
	st   *store.Store
	fake *ruvdstest.Fake
	out  *capture
	e    *Engine
	box  *secretbox.Box
	env  domain.Environment
	own  domain.User
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// newHostRig: проект с окружением, привязанным к fake RUVDS (или local),
// и deploy-runner'ом с поддержкой заданий хоста.
func newHostRig(t *testing.T, provider string) *hostRig {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	f := ruvdstest.New()
	srv := f.Start()
	t.Cleanup(srv.Close)
	r := &hostRig{t: t, st: st, fake: f, out: &capture{}, box: testBox(t)}
	r.e = r.engine(srv.URL)
	r.own = mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, r.own.ID)
	if err != nil {
		t.Fatal(err)
	}
	prov := &domain.ProvisionConfig{Provider: domain.ProvisionLocal, Port: 20001}
	if provider == domain.ProvisionRUVDS {
		token := "fake-ruvds-token"
		if _, _, err := st.UpsertInfraConnection(ctx, store.InfraConnInput{ID: "ruvds", Provider: "ruvds",
			Name: "RUVDS", Token: &token}, r.box, r.own.ID, r.own.Login); err != nil {
			t.Fatal(err)
		}
		prov = &domain.ProvisionConfig{Provider: domain.ProvisionRUVDS, Connection: "ruvds", Datacenter: 1,
			TariffID: 14, OSID: 52, PaymentPeriod: 2, CPU: 2, RAM: 2, Drive: 20, DriveTariffID: 1, SSHPublicKey: fakePubKey}
	}
	r.env, err = st.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "staging", ExecType: "ssh",
		Trigger: "manual", Config: domain.EnvConfig{DeployCmd: "docker compose up -d", VerifyCmd: "docker compose ps",
			Provision: prov}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Capabilities: []string{"deploy"},
		Features: []string{"monitor", "host_jobs"}}); err != nil {
		t.Fatal(err)
	}
	return r
}

// engine: новый процесс control plane на той же базе (рестарт rivetd).
func (r *hostRig) engine(url string) *Engine {
	e := New(r.st, &fakeSCM{}, nil, r.out, 90*time.Second)
	e.Box = r.box
	e.Infra = &infra.Factory{RUVDSBaseURL: url}
	return e
}

// tick: хосты проходят обработку сразу, без ожидания интервалов.
func (r *hostRig) tick(n int) {
	r.t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := r.st.Pool.Exec(ctx, `UPDATE env_hosts SET next_attempt_at=now(), lease_until=NULL`); err != nil {
			r.t.Fatal(err)
		}
		r.e.tickHosts(ctx)
	}
}

func (r *hostRig) host() domain.EnvHost {
	r.t.Helper()
	h, err := r.st.CurrentHost(context.Background(), r.env.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *hostRig) request() domain.EnvHost {
	r.t.Helper()
	h, err := r.st.RequestHost(context.Background(), r.env, "admin")
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *hostRig) act(action string, amount *float64, removeVolumes bool) (domain.EnvHost, error) {
	return r.e.HostAction(context.Background(), r.env, HostActionInput{Action: action, Actor: r.own.Login,
		Amount: amount, RemoveVolumes: removeVolumes})
}

func (r *hostRig) mustState(want string) domain.EnvHost {
	r.t.Helper()
	h := r.host()
	if h.State != want {
		r.t.Fatalf("состояние хоста %s (detail %q), ожидалось %s", h.State, h.Detail, want)
	}
	return h
}

func (r *hostRig) openAttention(reason domain.AttentionReason) int {
	r.t.Helper()
	var n int
	if err := r.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM attention a JOIN env_hosts h ON h.id=a.host_id
		WHERE h.env_id=$1 AND a.reason=$2 AND a.status <> 'resolved'`, r.env.ID, string(reason)).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *hostRig) lastHostJob() *pb.HostJob {
	r.t.Helper()
	r.out.mu.Lock()
	defer r.out.mu.Unlock()
	for i := len(r.out.sent) - 1; i >= 0; i-- {
		if j := r.out.sent[i].GetHostJob(); j != nil {
			return j
		}
	}
	r.t.Fatal("HostJob не отправлен")
	return nil
}

func (r *hostRig) noMoney(where string) {
	r.t.Helper()
	if c, d := r.fake.Creates(), r.fake.Deletes(); c != 0 || d != 0 {
		r.t.Fatalf("%s: без решения человека к провайдеру ушли создание (%d) или удаление (%d)", where, c, d)
	}
}

// toReady: хост RUVDS от запроса до готовности (решение владельца,
// создание, bootstrap с ключом хоста).
func (r *hostRig) toReady() domain.EnvHost {
	r.t.Helper()
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		r.t.Fatal(err)
	}
	r.tick(4)
	r.mustState(domain.HostBootstrapping)
	r.tick(1)
	job := r.lastHostJob()
	if err := r.e.OnHostJobResult(context.Background(), "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true,
		HostKey: strings.TrimPrefix(job.Host, "root@") + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKey"}); err != nil {
		r.t.Fatal(err)
	}
	return r.mustState(domain.HostReady)
}

func TestHostNoMoneyWithoutApproval(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.request()
	r.tick(5)
	h := r.mustState(domain.HostAwaitingApproval)
	if h.QuoteAmount == nil || *h.QuoteAmount != 658 || h.BalanceAmount == nil || *h.BalanceAmount != 10000 {
		t.Fatalf("цена и баланс: %+v %+v", h.QuoteAmount, h.BalanceAmount)
	}
	if r.openAttention(domain.AttHostCreate) != 1 {
		t.Fatal("решение HOST_CREATE не открыто")
	}
	// Рестарт control plane и ещё тики: решение никто не дал: денег нет.
	for i := 0; i < 3; i++ {
		r.e = r.engine(r.e.Infra.RUVDSBaseURL)
		r.tick(3)
	}
	r.noMoney("ожидание решения")
	if r.fake.Quotes() != 1 {
		t.Fatalf("цена считалась %d раз: ожидание решения не должно ходить к провайдеру", r.fake.Quotes())
	}
	// Отказ закрывает решение, хост в конечном состоянии, денег нет.
	if _, err := r.act(HostActReject, nil, false); err != nil {
		t.Fatal(err)
	}
	r.tick(3)
	r.mustState(domain.HostRejected)
	r.noMoney("после отказа")
	if r.openAttention(domain.AttHostCreate) != 0 {
		t.Fatal("решение осталось открытым после отказа")
	}
}

func TestHostApproveCreatesOnceAndReady(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	wrong := *h.QuoteAmount - 1
	if _, err := r.act(HostActApprove, &wrong, false); err != store.ErrPriceChanged {
		t.Fatalf("подтверждение чужой суммы: %v", err)
	}
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	// Повторное подтверждение: не своё состояние, второго создания нет.
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err == nil {
		t.Fatal("повторное подтверждение принято")
	}
	r.tick(1)
	if r.fake.Creates() != 1 {
		t.Fatalf("создание отправлено %d раз", r.fake.Creates())
	}
	// Рестарты посреди создания второго запроса не дают.
	for i := 0; i < 3; i++ {
		r.e = r.engine(r.e.Infra.RUVDSBaseURL)
		r.tick(2)
	}
	if r.fake.Creates() != 1 {
		t.Fatalf("после рестартов создание отправлено %d раз", r.fake.Creates())
	}
	h = r.mustState(domain.HostBootstrapping)
	if !strings.HasPrefix(h.Address, "10.77.") || h.ServerID == "" {
		t.Fatalf("адрес сервера: %+v", h)
	}
	r.tick(1)
	job := r.lastHostJob()
	if job.Host != "root@"+h.Address || !job.AcceptNewHostKey || job.HostKey != "" || job.SshWaitS == 0 ||
		!strings.Contains(job.Script, "docker compose version") || job.Env["COMPOSE_PROJECT_NAME"] == "" {
		t.Fatalf("задание bootstrap: %+v", job)
	}
	// Итог от чужого runner'а и без ключа хоста не принимается.
	ctx := context.Background()
	if err := r.e.OnHostJobResult(ctx, "intruder", &pb.HostJobResult{JobId: job.JobId, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true}); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostBootstrapping)
	r.tick(1)
	job = r.lastHostJob()
	key := h.Address + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKey"
	if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true, HostKey: key}); err != nil {
		t.Fatal(err)
	}
	h = r.mustState(domain.HostReady)
	env, err := r.st.GetEnvironment(ctx, r.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env.Config.Host != "root@"+h.Address || env.Config.HostKey != key {
		t.Fatalf("конфигурация окружения: host=%q key=%q", env.Config.Host, env.Config.HostKey)
	}
	// Стоимость и дата оплаты.
	r.tick(1)
	h = r.host()
	if h.CostAmount == nil || *h.CostAmount != 658 || h.PaidTill == nil {
		t.Fatalf("стоимость: %+v %+v", h.CostAmount, h.PaidTill)
	}
	// Публикация идёт на хост с закреплённым ключом и переменными compose.
	dep, err := r.st.EnqueueDeployment(ctx, r.env.ID, "sha-1", "vova")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	dj := r.out.lastDeploy(t)
	if dj.DeploymentId != dep.ID || dj.Host != "root@"+h.Address || dj.HostKey != key ||
		!strings.Contains(dj.DeployCmd, "COMPOSE_PROJECT_NAME=") || !strings.HasSuffix(dj.DeployCmd, "docker compose up -d") ||
		dj.Checkout {
		t.Fatalf("публикация: %+v", dj)
	}
	if r.openAttention(domain.AttHostCreate)+r.openAttention(domain.AttHostFailed) != 0 {
		t.Fatal("после готовности остались открытые эскалации")
	}
}

func TestHostPriceGrewAfterApproval(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	r.fake.PriceFactor = 1.5
	r.tick(3)
	h = r.mustState(domain.HostAwaitingApproval)
	r.noMoney("цена выросла")
	if h.ApprovedBy != "" || h.QuoteAmount == nil || *h.QuoteAmount != 987 {
		t.Fatalf("новая цена: %+v %v", h.ApprovedBy, h.QuoteAmount)
	}
	if r.openAttention(domain.AttHostCreate) != 1 {
		t.Fatal("новое решение не открыто")
	}
}

func TestHostInsufficientBalance(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	r.fake.SetBalance(100)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	r.tick(3)
	h = r.mustState(domain.HostFailed)
	r.noMoney("нехватка баланса")
	if h.Failure != domain.HostFailInsufficientBalance || !strings.Contains(h.Detail, "недостаточно средств") {
		t.Fatalf("причина: %s %q", h.Failure, h.Detail)
	}
	if r.openAttention(domain.AttHostFailed) != 1 {
		t.Fatal("эскалация HOST_FAILED не открыта")
	}
	// Новый запрос закрывает эскалацию прежнего хоста.
	r.request()
	if r.openAttention(domain.AttHostFailed) != 0 {
		t.Fatal("эскалация прежнего хоста не закрыта новым запросом")
	}
}

func TestHostLostCreateResponseFoundByMarker(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	r.fake.DropCreateResponse = true
	r.tick(1)
	h = r.mustState(domain.HostCreating)
	if h.ServerID != "" || h.CreateSentAt == nil {
		t.Fatalf("исход должен быть неизвестен: %+v", h)
	}
	r.e = r.engine(r.e.Infra.RUVDSBaseURL)
	r.tick(5)
	r.mustState(domain.HostBootstrapping)
	if r.fake.Creates() != 1 {
		t.Fatalf("после потери ответа создание отправлено %d раз", r.fake.Creates())
	}
}

func TestHostUnknownOutcomeNoSecondCreate(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	old := hostUnknownWindow
	hostUnknownWindow = 0
	t.Cleanup(func() { hostUnknownWindow = old })
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	r.fake.DropCreateResponse, r.fake.HideFromSearch = true, true
	r.tick(5)
	h = r.mustState(domain.HostFailed)
	if h.Failure != domain.HostFailUnknownOutcome || r.fake.Creates() != 1 {
		t.Fatalf("неизвестный исход: %s, создание %d раз", h.Failure, r.fake.Creates())
	}
	if r.openAttention(domain.AttHostFailed) != 1 {
		t.Fatal("эскалация неизвестного исхода не открыта")
	}
}

func TestHostAdoptsExistingServerWithoutMoney(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	h := r.toReady()
	// Хост потерян в базе (сбой), а сервер окружения у провайдера остался:
	// новый запрос принимает его без решения и без создания.
	if _, err := r.st.Pool.Exec(context.Background(), `UPDATE env_hosts SET state='failed' WHERE id=$1`, h.ID); err != nil {
		t.Fatal(err)
	}
	creates := r.fake.Creates()
	r.request()
	r.tick(3)
	nh := r.mustState(domain.HostBootstrapping)
	if nh.ServerID != h.ServerID || r.fake.Creates() != creates || r.openAttention(domain.AttHostCreate) != 0 {
		t.Fatalf("сервер не принят: %+v, создание %d", nh, r.fake.Creates())
	}
}

func TestHostWaitPayment(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	r.fake.WaitPayment = true
	r.request()
	r.tick(1)
	h := r.mustState(domain.HostAwaitingApproval)
	if _, err := r.act(HostActApprove, h.QuoteAmount, false); err != nil {
		t.Fatal(err)
	}
	r.tick(3)
	h = r.mustState(domain.HostCreating)
	if !h.WaitingPayment || r.openAttention(domain.AttHostFailed) != 1 {
		t.Fatalf("ожидание оплаты не показано: %+v", h)
	}
	r.fake.Pay()
	r.tick(4)
	r.mustState(domain.HostBootstrapping)
	if r.fake.Creates() != 1 || r.openAttention(domain.AttHostFailed) != 0 {
		t.Fatalf("после оплаты: создание %d, эскалаций %d", r.fake.Creates(), r.openAttention(domain.AttHostFailed))
	}
}

func TestHostDeleteOnlyAfterConfirmation(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	h := r.toReady()
	if _, err := r.act(HostActConfirmDelete, nil, false); err == nil {
		t.Fatal("подтверждение без запроса удаления принято")
	}
	if _, err := r.act(HostActDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostDeleteRequested)
	if r.openAttention(domain.AttHostDelete) != 1 {
		t.Fatal("решение HOST_DELETE не открыто")
	}
	for i := 0; i < 2; i++ {
		r.e = r.engine(r.e.Infra.RUVDSBaseURL)
		r.tick(3)
	}
	if r.fake.Deletes() != 0 {
		t.Fatalf("удаление ушло без подтверждения: %d", r.fake.Deletes())
	}
	// Окружение с живым хостом не удаляется.
	if err := r.st.DeleteEnvironment(context.Background(), r.env.ID); err == nil {
		t.Fatal("окружение с хостом удалено")
	}
	// Отмена возвращает готовность; затем запрос и подтверждение.
	if _, err := r.act(HostActCancelDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostReady)
	if _, err := r.act(HostActDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.act(HostActConfirmDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	env, _ := r.st.GetEnvironment(context.Background(), r.env.ID)
	if env.Config.Host != "" || env.Config.HostKey != "" {
		t.Fatalf("адрес удаляемого хоста остался в окружении: %+v", env.Config)
	}
	r.tick(6)
	r.mustState(domain.HostDeleted)
	if r.fake.Deletes() != 1 || r.fake.ServerCount() != 0 {
		t.Fatalf("удаление: %d запросов, серверов %d (сервер %s)", r.fake.Deletes(), r.fake.ServerCount(), h.ServerID)
	}
	if err := r.st.DeleteEnvironment(context.Background(), r.env.ID); err != nil {
		t.Fatalf("окружение без хоста не удаляется: %v", err)
	}
}

func TestHostDeploymentWaitsForReadyHost(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	ctx := context.Background()
	r.request()
	if _, err := r.st.EnqueueDeployment(ctx, r.env.ID, "sha-1", "vova"); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r.out.mu.Lock()
	for _, m := range r.out.sent {
		if m.GetDeploy() != nil {
			r.out.mu.Unlock()
			t.Fatal("публикация ушла до готовности хоста: команды пошли бы на машину runner'а")
		}
	}
	r.out.mu.Unlock()
}

func TestLocalHostLifecycle(t *testing.T) {
	r := newHostRig(t, domain.ProvisionLocal)
	ctx := context.Background()
	r.request()
	r.tick(1)
	r.mustState(domain.HostBootstrapping)
	r.tick(1)
	job := r.lastHostJob()
	compose := domain.ComposeProject(r.env.ProjectID, r.env.ID)
	if job.Host != "" || job.AcceptNewHostKey || job.Script != infra.LocalBootstrap ||
		job.Env["COMPOSE_PROJECT_NAME"] != compose || job.Env["RIVET_HOST_PORT"] != "20001" {
		t.Fatalf("задание локального хоста: %+v", job)
	}
	if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true}); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostReady)
	if _, err := r.st.EnqueueDeployment(ctx, r.env.ID, "main", "vova"); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	dj := r.out.lastDeploy(t)
	if dj.Host != "" || !dj.Checkout || !strings.Contains(dj.DeployCmd, "RIVET_HOST_PORT='20001'") ||
		!strings.Contains(dj.VerifyCmd, "COMPOSE_PROJECT_NAME='"+compose+"'") {
		t.Fatalf("локальная публикация: %+v", dj)
	}
	if err := r.e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: dj.DeploymentId, Stage: pb.DeployResult_DEPLOY, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.e.OnDeployResult(ctx, "deployer", &pb.DeployResult{DeploymentId: dj.DeploymentId, Stage: pb.DeployResult_VERIFY, Ok: true}); err != nil {
		t.Fatal(err)
	}
	// Снятие без томов: без решения, тома сохраняются.
	if _, err := r.act(HostActDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostDeleting)
	r.tick(1)
	job = r.lastHostJob()
	if !strings.Contains(job.Script, "down --remove-orphans") || strings.Contains(job.Script, "--volumes") {
		t.Fatalf("снятие: %q", job.Script)
	}
	if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true}); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostDeleted)
	// С томами: только через решение.
	r.request()
	r.tick(1)
	r.tick(1)
	job = r.lastHostJob()
	if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: true}); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostReady)
	if _, err := r.act(HostActDelete, nil, true); err != nil {
		t.Fatal(err)
	}
	r.mustState(domain.HostDeleteRequested)
	if r.openAttention(domain.AttHostDelete) != 1 {
		t.Fatal("решение об удалении томов не открыто")
	}
	if _, err := r.act(HostActConfirmDelete, nil, false); err != nil {
		t.Fatal(err)
	}
	r.tick(1)
	job = r.lastHostJob()
	if !strings.Contains(job.Script, "--volumes") {
		t.Fatalf("снятие с томами: %q", job.Script)
	}
}

func TestLocalBootstrapRetriesThenEscalates(t *testing.T) {
	r := newHostRig(t, domain.ProvisionLocal)
	ctx := context.Background()
	r.request()
	r.tick(1)
	for i := 0; i < hostJobAttempts; i++ {
		r.tick(1)
		job := r.lastHostJob()
		if err := r.e.OnHostJobResult(ctx, "deployer", &pb.HostJobResult{JobId: job.JobId, Ok: false,
			Output: "docker: command not found"}); err != nil {
			t.Fatal(err)
		}
	}
	h := r.mustState(domain.HostBootstrapping)
	if !h.NeedsRetry || r.openAttention(domain.AttHostFailed) != 1 || !strings.Contains(h.Detail, "docker") {
		t.Fatalf("после трёх неудач: %+v", h)
	}
	sent := len(r.out.sent)
	r.tick(3)
	if len(r.out.sent) != sent {
		t.Fatal("bootstrap повторяется без решения человека")
	}
	if _, err := r.act(HostActRetry, nil, false); err != nil {
		t.Fatal(err)
	}
	r.tick(1)
	if len(r.out.sent) == sent || r.openAttention(domain.AttHostFailed) != 0 {
		t.Fatal("повтор не запустил bootstrap")
	}
}

func TestHostTableRefusesUnapprovedCreate(t *testing.T) {
	r := newHostRig(t, domain.ProvisionRUVDS)
	h := r.request()
	ctx := context.Background()
	if _, err := r.st.Pool.Exec(ctx, `UPDATE env_hosts SET create_sent_at=now() WHERE id=$1`, h.ID); err == nil {
		t.Fatal("база приняла отправку создания без решения")
	}
	if _, err := r.st.Pool.Exec(ctx, `UPDATE env_hosts SET delete_sent_at=now() WHERE id=$1`, h.ID); err == nil {
		t.Fatal("база приняла отправку удаления без подтверждения")
	}
}
