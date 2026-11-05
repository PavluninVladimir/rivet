//go:build linux

package native

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDumpableProbe — проба в отдельном процессе: он помечает себя
// недампируемым, как runner после старта, и запускает изолированную
// команду. Вне пробы тест пропускается.
func TestDumpableProbe(t *testing.T) {
	if flag.Arg(0) != "dumpable-probe" {
		t.Skip("проба запускается только из TestSandboxFromNonDumpableProcess")
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Println("PRCTL-FAIL", err)
		return
	}
	v, _ := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	fmt.Println("dumpable", v)
	fmt.Printf("available %v reason=%s\n", NetworkIsolationAvailable(), SandboxUnavailableReason())
	dir := flag.Arg(1)
	sp := spec(dir, dir, probeCmd("net-probe")+`; echo "pidns=$(readlink /proc/self/ns/pid)"`)
	sp.isolate = true
	res := runCommand(context.Background(), sp)
	fmt.Printf("isolated %v err=%v\n", res.isolated, res.err)
	fmt.Print(res.output)
}

// Песочница поднимается и у недампируемого процесса: namespace'ы создаёт
// промежуточный процесс. Без user namespaces — честный откат с причиной.
func TestSandboxFromNonDumpableProcess(t *testing.T) {
	dir := t.TempDir()
	out, err := exec.Command(os.Args[0], "-test.run=^TestDumpableProbe$", "dumpable-probe", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("проба: %v\n%s", err, out)
	}
	report := string(out)
	if !strings.Contains(report, "dumpable 0\n") {
		t.Fatalf("проба должна быть недампируемой:\n%s", report)
	}
	if !NetworkIsolationAvailable() {
		// Ядро не даёт user namespace (Docker с seccomp по умолчанию):
		// откат без изоляции, причина — настоящая.
		if !strings.Contains(report, "available false reason=ядро не даёт создать user namespace") ||
			!strings.Contains(report, "isolated false err=<nil>") {
			t.Fatalf("ожидается откат с причиной:\n%s", report)
		}
		t.Logf("песочница недоступна: %s", SandboxUnavailableReason())
		return
	}
	own, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"available true", "isolated true err=<nil>", "LOOPBACK-OK", "NET-BLOCKED", "pidns="} {
		if !strings.Contains(report, want) {
			t.Errorf("нет %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "pidns="+own+"\n") {
		t.Errorf("команда в PID namespace runner'а:\n%s", report)
	}
}
