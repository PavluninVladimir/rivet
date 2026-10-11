package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Публикация в Kubernetes (change add-k8s-delivery): команды собирает
// control plane, исполняет обычная деплой-джоба.

func TestK8sJobManifests(t *testing.T) {
	deploy, verify := k8sJob(domain.EnvConfig{
		Namespace: "prod", Manifests: "deploy/k8s", Workload: "deployment/api",
	}, "sha-1")
	if deploy != "kubectl apply -n 'prod' -f 'deploy/k8s'" {
		t.Fatalf("команда доставки: %q", deploy)
	}
	if !strings.HasPrefix(verify, "kubectl rollout status -n 'prod' 'deployment/api'") {
		t.Fatalf("проверка по умолчанию: %q", verify)
	}
}

func TestK8sJobChart(t *testing.T) {
	deploy, verify := k8sJob(domain.EnvConfig{
		Namespace: "stage", Chart: "charts/api", Release: "api",
		Values: map[string]string{"image.tag": "v1", "replicas": "2"},
	}, "sha-1")
	for _, want := range []string{"helm upgrade --install 'api' 'charts/api'", "-n 'stage'",
		"--wait", "'rivetVersion=sha-1'", "'image.tag=v1'", "'replicas=2'"} {
		if !strings.Contains(deploy, want) {
			t.Fatalf("в команде нет %q: %q", want, deploy)
		}
	}
	if !strings.Contains(verify, "helm status 'api'") {
		t.Fatalf("проверка релиза: %q", verify)
	}
	// Значения идут в стабильном порядке: команда не меняется от прохода.
	again, _ := k8sJob(domain.EnvConfig{
		Namespace: "stage", Chart: "charts/api", Release: "api",
		Values: map[string]string{"replicas": "2", "image.tag": "v1"},
	}, "sha-1")
	if again != deploy {
		t.Fatalf("порядок значений нестабилен:\n%s\n%s", deploy, again)
	}
}

// Своя проверка перекрывает умолчание, а команду доставки задать нельзя:
// её собирает система из параметров кластера.
func TestK8sJobVerifyOverride(t *testing.T) {
	deploy, verify := k8sJob(domain.EnvConfig{
		Namespace: "prod", Manifests: "deploy/k8s", Workload: "deployment/api",
		VerifyCmd: "make smoke",
	}, "sha-1")
	if verify != "make smoke" {
		t.Fatalf("своя проверка должна перекрывать умолчание: %q", verify)
	}
	if !strings.Contains(deploy, "kubectl apply") {
		t.Fatalf("команда доставки собирается системой: %q", deploy)
	}
	cfg := domain.EnvConfig{Namespace: "prod", Manifests: "deploy/k8s",
		Workload: "deployment/api", DeployCmd: "make deploy"}
	if err := cfg.Validate(domain.ExecK8s); err == nil {
		t.Fatal("своя команда доставки у k8s-окружения должна отклоняться")
	}
}

// Конфигурация кластера проверяется до шелла runner'а.
func TestK8sConfigValidation(t *testing.T) {
	bad := []domain.EnvConfig{
		{Namespace: "prod"}, // ни манифестов, ни чарта
		{Namespace: "prod", Manifests: "deploy", Chart: "charts/api"},           // и то, и другое
		{Namespace: "prod; rm -rf /", Manifests: "deploy"},                      // подстановка в namespace
		{Namespace: "prod", Manifests: "deploy/$(whoami)"},                      // подстановка в пути
		{Namespace: "prod", Manifests: "../../etc"},                             // выход за корень
		{Namespace: "prod", Chart: "charts/api", Release: "api;touch x"},        // подстановка в релизе
		{Namespace: "prod", Manifests: "deploy", Workload: "deployment/api; x"}, // подстановка в объекте
		{Namespace: "prod", Chart: "charts/api", Release: "api",
			Values: map[string]string{"image.tag": "$(id)"}}, // подстановка в значении
		{Namespace: "prod", Manifests: "deploy", Host: "server"}, // хоста у k8s нет
		{Namespace: "prod", Manifests: "deploy"},                 // Verify нечем: нет workload
	}
	for i, cfg := range bad {
		if err := cfg.Validate(domain.ExecK8s); err == nil {
			t.Fatalf("конфигурация #%d должна отклоняться: %+v", i, cfg)
		}
	}
	ok := domain.EnvConfig{Namespace: "prod", Manifests: "deploy/k8s", Workload: "deployment/api"}
	if err := ok.Validate(domain.ExecK8s); err != nil {
		t.Fatalf("нормальная конфигурация: %v", err)
	}
	okChart := domain.EnvConfig{Namespace: "prod", Chart: "charts/api", Release: "api",
		Values: map[string]string{"image.tag": "v1"}}
	if err := okChart.Validate(domain.ExecK8s); err != nil {
		t.Fatalf("нормальный чарт: %v", err)
	}
}

// Джоба k8s-окружения уходит deploy-runner'у с собранными командами.
func TestK8sDeployJob(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "prod", ExecType: domain.ExecK8s, Trigger: "manual",
		Config: domain.EnvConfig{Namespace: "prod", Manifests: "deploy/k8s", Workload: "deployment/api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "deployer", Agent: "wrap",
		Capabilities: []string{"deploy"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova"); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	job := out.lastDeploy(t)
	if !job.Checkout {
		t.Fatal("k8s-джоба должна требовать рабочую копию репозитория")
	}
	if !strings.Contains(job.DeployCmd, "kubectl apply -n 'prod'") {
		t.Fatalf("команда доставки в джобе: %q", job.DeployCmd)
	}
	if !strings.Contains(job.VerifyCmd, "rollout status") {
		t.Fatalf("команда проверки в джобе: %q", job.VerifyCmd)
	}
	if job.Version != "sha-1" {
		t.Fatalf("версия: %q", job.Version)
	}
}

// Публикация уходит только runner'у с требуемыми capability окружения:
// доступ к кластеру и к закрытому периметру даёт окружение runner'а.
func TestDeployRunnerCaps(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := &capture{}
	e := New(st, &fakeSCM{}, nil, out, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{
		ProjectID: p.ID, Name: "cluster", ExecType: domain.ExecK8s, Trigger: "manual",
		RunnerCaps: []string{"k8s-prod"},
		Config: domain.EnvConfig{Namespace: "prod", Manifests: "deploy/k8s",
			Workload: "deployment/api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Runner без нужной capability публикацию не получает.
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "plain", Agent: "wrap",
		Capabilities: []string{"deploy"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "vova"); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	out.mu.Lock()
	sent := len(out.sent)
	out.mu.Unlock()
	if sent != 0 {
		t.Fatalf("публикация не должна уходить runner'у без capability: %d сообщений", sent)
	}
	// Появился подходящий runner — публикация уходит ему.
	if err := st.UpsertRunner(ctx, domain.Runner{ID: "cluster-runner", Agent: "wrap",
		Capabilities: []string{"deploy", "k8s-prod"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if job := out.lastDeploy(t); job.EnvName != "cluster" {
		t.Fatalf("джоба: %+v", job)
	}
}
