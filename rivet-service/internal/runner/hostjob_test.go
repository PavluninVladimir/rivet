package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Задания хоста и закреплённый ключ хоста (add-environment-provisioning).
// Настоящего SSH в тестах нет: подмена ssh в PATH пишет свои аргументы и
// исполняет скрипт локально, как подмена на e2e-стенде.

// fakeSSH: каталог с подменой ssh: аргументы в args.log, копия known_hosts
// из UserKnownHostsFile в seen.log, при accept-new: фиктивный ключ хоста.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
log="` + dir + `"
printf '%s\n' "$*" >> "$log/args.log"
known=""; accept=""; host=""
while [ $# -gt 1 ]; do
  case "$1" in
    -o) case "$2" in
          UserKnownHostsFile=*) known="${2#UserKnownHostsFile=}" ;;
          StrictHostKeyChecking=accept-new) accept=1 ;;
        esac; shift 2 ;;
    *) if [ -z "$host" ]; then host="$1"; shift; else break; fi ;;
  esac
done
[ -n "$known" ] && cat "$known" >> "$log/seen.log"
if [ -n "$accept" ] && [ -n "$known" ]; then
  echo "${host#*@} ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKeyFromSSH" >> "$known"
fi
exec /bin/sh -c "$1"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func collectHostJob(t *testing.T) (func(*pb.RunnerMsg), func() *pb.HostJobResult) {
	t.Helper()
	ch := make(chan *pb.RunnerMsg, 4)
	return func(m *pb.RunnerMsg) { ch <- m }, func() *pb.HostJobResult {
		select {
		case m := <-ch:
			return m.GetHostJobResult()
		case <-time.After(10 * time.Second):
			t.Fatal("итог задания хоста не пришёл")
			return nil
		}
	}
}

func TestHostJobLocal(t *testing.T) {
	a := &agent{cfg: Config{Workdir: t.TempDir()}}
	emit, result := collectHostJob(t)
	go a.executeHostJob(context.Background(), &pb.HostJob{JobId: "hj-1", EnvName: "dev",
		Script: `echo "проект $COMPOSE_PROJECT_NAME порт $RIVET_HOST_PORT"`,
		Env:    map[string]string{"COMPOSE_PROJECT_NAME": "rivet-a-b", "RIVET_HOST_PORT": "20001"}}, emit)
	r := result()
	if !r.Ok || r.JobId != "hj-1" || !strings.Contains(r.Output, "проект rivet-a-b порт 20001") || r.HostKey != "" {
		t.Fatalf("локальное задание: %+v", r)
	}
	go a.executeHostJob(context.Background(), &pb.HostJob{JobId: "hj-2", Script: "echo docker: not found; exit 3"}, emit)
	if r := result(); r.Ok || !strings.Contains(r.Output, "docker: not found") {
		t.Fatalf("провал локального задания: %+v", r)
	}
}

func TestHostJobAcceptsNewHostKey(t *testing.T) {
	dir := fakeSSH(t)
	a := &agent{cfg: Config{Workdir: t.TempDir()}}
	emit, result := collectHostJob(t)
	go a.executeHostJob(context.Background(), &pb.HostJob{JobId: "hj-3", EnvName: "staging", Host: "root@10.77.0.5",
		Script: "echo bootstrap-ok", AcceptNewHostKey: true, SshWaitS: 30,
		Env: map[string]string{"RIVET_ENV": "staging"}}, emit)
	r := result()
	if !r.Ok || r.HostKey != "10.77.0.5 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKeyFromSSH" || !strings.Contains(r.Output, "bootstrap-ok") {
		t.Fatalf("первое подключение: %+v", r)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	for _, want := range []string{"StrictHostKeyChecking=accept-new", "HashKnownHosts=no", "GlobalKnownHostsFile=/dev/null", "BatchMode=yes"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("нет опции %s: %s", want, args)
		}
	}
	// Временный known_hosts задания удалён.
	for _, line := range strings.Split(string(args), "\n") {
		if i := strings.Index(line, "UserKnownHostsFile="); i >= 0 {
			path := strings.Fields(line[i+len("UserKnownHostsFile="):])[0]
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("known_hosts задания не удалён: %s", path)
			}
		}
	}
}

func TestDeployUsesPinnedHostKey(t *testing.T) {
	dir := fakeSSH(t)
	a := &agent{cfg: Config{Workdir: t.TempDir()}}
	key := "10.77.0.5 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPinnedKey"
	out, err := a.runDeployCmd(context.Background(), &pb.DeployJob{EnvName: "staging", Host: "root@10.77.0.5",
		HostKey: key, Version: "abc"}, a.cfg.Workdir, `echo "версия $RIVET_VERSION"`, nil)
	if err != nil || !strings.Contains(out, "версия abc") {
		t.Fatalf("публикация: %v %q", err, out)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	seen, _ := os.ReadFile(filepath.Join(dir, "seen.log"))
	if !strings.Contains(string(args), "StrictHostKeyChecking=yes") || strings.TrimSpace(string(seen)) != key {
		t.Fatalf("ключ хоста не закреплён: args=%s seen=%s", args, seen)
	}
	// Без ключа: прежнее поведение: опций проверки ключа нет.
	if err := os.Remove(filepath.Join(dir, "args.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.runDeployCmd(context.Background(), &pb.DeployJob{Host: "deploy@legacy"}, a.cfg.Workdir, "true", nil); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(filepath.Join(dir, "args.log"))
	if strings.Contains(string(args), "StrictHostKeyChecking") || strings.Contains(string(args), "UserKnownHostsFile") {
		t.Fatalf("хост без ключа получил опции проверки: %s", args)
	}
}

func TestAcceptedHostKeyFiltersLines(t *testing.T) {
	f := filepath.Join(t.TempDir(), "known")
	content := "|1|hashed|entry ssh-ed25519 AAAA\n@cert-authority * ssh-rsa AAAA\n10.0.0.1 ssh-ed25519 AAAAC3Nza\n# comment\n10.0.0.1 ecdsa-sha2-nistp256 AAAAE2Vj\n"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := acceptedHostKey(f)
	if err != nil || got != "10.0.0.1 ssh-ed25519 AAAAC3Nza\n10.0.0.1 ecdsa-sha2-nistp256 AAAAE2Vj" {
		t.Fatalf("ключ хоста: %q %v", got, err)
	}
	if err := os.WriteFile(f, []byte("|1|only|hashed ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acceptedHostKey(f); err == nil {
		t.Fatal("хэшированная строка принята за ключ хоста")
	}
}

func TestFeaturesDeclareHostJobs(t *testing.T) {
	if f := (Config{Capabilities: []string{"deploy"}}).features(); !strings.Contains(strings.Join(f, ","), featureHostJobs) {
		t.Fatalf("runner выкатки не объявил host_jobs: %v", f)
	}
	if f := (Config{Adapter: AdapterNative, Capabilities: []string{"coding"}}).features(); strings.Contains(strings.Join(f, ","), featureHostJobs) {
		t.Fatalf("runner агента объявил host_jobs: %v", f)
	}
}
