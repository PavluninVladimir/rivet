package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Внук держит вывод открытым: дедлайн убивает группу процессов, проверка
// проваливается по таймауту вовремя (ревью: таймаут не срабатывал).
func TestMonitorTimeoutGrandchild(t *testing.T) {
	start := time.Now()
	r := runMonitor(t, &pb.MonitorJob{CheckId: "c6", EnvName: "Демо", CheckCmd: "sleep 8; echo done", TimeoutS: 1})
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("таймаут 1 с не сработал: %s", took)
	}
	if r.Ok || !strings.Contains(r.Output, "таймаут") {
		t.Fatalf("ожидался провал по таймауту: %+v", r)
	}
}

// Обрыв сессии: прерванная проверка результата не отправляет (ложного
// провала нет).
func TestMonitorCancelNoResult(t *testing.T) {
	a := &agent{cfg: Config{Workdir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	sent := 0
	a.executeMonitor(ctx, &pb.MonitorJob{CheckId: "c7", CheckCmd: "sleep 5", LogsCmd: "echo logs", TimeoutS: 60},
		func(*pb.RunnerMsg) { sent++ })
	if sent != 0 {
		t.Fatalf("прерванная проверка отправила результат: %d", sent)
	}
}

// Обрезка хвоста посреди руны: вывод остаётся UTF-8 и сериализуется.
func TestMonitorTailUTF8(t *testing.T) {
	for n := 4000; n < 4006; n++ {
		r := runMonitor(t, &pb.MonitorJob{CheckId: "c8", TimeoutS: 30,
			CheckCmd: fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo "ошибка проверки строка $i"; i=$((i+1)); done; exit 1`, n)})
		if !utf8.ValidString(r.Output) {
			t.Fatalf("n=%d: невалидный UTF-8", n)
		}
		if _, err := proto.Marshal(&pb.RunnerMsg{MsgId: "x", Kind: &pb.RunnerMsg_MonitorResult{MonitorResult: r}}); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
}

// tail у DeployResult и StageResult режет по границе руны.
func TestTailUTF8(t *testing.T) {
	s := strings.Repeat("я", 100) + "\xd0"
	for n := 1; n < 20; n++ {
		if out := tail(s, n); !utf8.ValidString(out) {
			t.Fatalf("n=%d: %q", n, out)
		}
	}
}

// URL-проверка с сети runner'а: тело только при провале и не больше 512 байт.
func TestMonitorURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(strings.Repeat("я", 2000)))
			return
		}
		_, _ = w.Write([]byte("secret-body"))
	}))
	defer srv.Close()
	ok := runMonitor(t, &pb.MonitorJob{CheckId: "u1", CheckUrl: srv.URL + "/ok", TimeoutS: 10})
	if !ok.Ok || strings.Contains(ok.Output, "secret-body") {
		t.Fatalf("успешная проверка: %+v", ok)
	}
	bad := runMonitor(t, &pb.MonitorJob{CheckId: "u2", CheckUrl: srv.URL + "/bad", TimeoutS: 10})
	if bad.Ok || !utf8.ValidString(bad.Output) || len(bad.Output) > 700 {
		t.Fatalf("провал: ok=%v len=%d", bad.Ok, len(bad.Output))
	}
}

func runMonitor(t *testing.T, job *pb.MonitorJob) *pb.MonitorResult {
	t.Helper()
	a := &agent{cfg: Config{Workdir: t.TempDir()}}
	var got []*pb.MonitorResult
	a.executeMonitor(context.Background(), job, func(m *pb.RunnerMsg) {
		if r := m.GetMonitorResult(); r != nil {
			got = append(got, r)
		}
	})
	if len(got) != 1 {
		t.Fatalf("ожидался один MonitorResult, пришло %d", len(got))
	}
	if got[0].CheckId != job.CheckId {
		t.Fatalf("check_id не совпал: %q", got[0].CheckId)
	}
	return got[0]
}

// Успешная проверка: логи не читаются, переменные окружения доступны.
func TestMonitorOK(t *testing.T) {
	r := runMonitor(t, &pb.MonitorJob{CheckId: "c1", EnvName: "Демо", Repo: "o/r",
		CheckCmd: `echo "check $RIVET_ENV $RIVET_REPO"`, LogsCmd: "echo logs", TimeoutS: 10})
	if !r.Ok || r.Logs != "" {
		t.Fatalf("ожидался успех без логов: %+v", r)
	}
	if !strings.Contains(r.Output, "check Демо o/r") {
		t.Fatalf("вывод без переменных: %q", r.Output)
	}
}

// Провал: команда логов исполняется, токен в выводе и логах замаскирован.
func TestMonitorFailMasksSecrets(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	r := runMonitor(t, &pb.MonitorJob{CheckId: "c2", EnvName: "Демо",
		CheckCmd: "echo 'GET /api/contacts?page=2 → 500 token=" + token + "'; exit 1",
		LogsCmd:  "echo 'TypeError: cannot read cursor'; echo 'Authorization: Bearer " + token + "'", TimeoutS: 10})
	if r.Ok {
		t.Fatal("ожидался провал")
	}
	if !strings.Contains(r.Logs, "TypeError") {
		t.Fatalf("логи не прочитаны: %q", r.Logs)
	}
	if strings.Contains(r.Output, token) || strings.Contains(r.Logs, token) {
		t.Fatalf("токен не замаскирован: output=%q logs=%q", r.Output, r.Logs)
	}
}

// Зависшая проверка обрывается по дедлайну и считается провалом.
func TestMonitorTimeout(t *testing.T) {
	r := runMonitor(t, &pb.MonitorJob{CheckId: "c3", EnvName: "Демо", CheckCmd: "sleep 30", TimeoutS: 1})
	if r.Ok || !strings.Contains(r.Output, "таймаут") {
		t.Fatalf("ожидался провал по таймауту: %+v", r)
	}
}

// Feature monitor (и задания хоста, add-environment-provisioning)
// объявляется только runner'ом с capability deploy.
func TestFeaturesMonitor(t *testing.T) {
	if f := (Config{Capabilities: []string{"coding"}}).features(); len(f) != 1 || f[0] != featureRoles {
		t.Fatalf("coding-runner: %v", f)
	}
	f := (Config{Capabilities: []string{"deploy"}}).features()
	if len(f) != 4 || f[1] != featureMonitor || f[2] != featureMonitorURL || f[3] != featureHostJobs {
		t.Fatalf("deploy-runner: %v", f)
	}
}

// Общий бюджет проверки: команда и логи укладываются в budget_s, результат
// приходит вовремя и с выводом проверки.
func TestMonitorBudget(t *testing.T) {
	start := time.Now()
	r := runMonitor(t, &pb.MonitorJob{CheckId: "b1", CheckCmd: "echo up; sleep 10", LogsCmd: "echo logs; sleep 10",
		TimeoutS: 1, LogsTimeoutS: 1, BudgetS: 3})
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("бюджет 3 с превышен: %s", took)
	}
	if r.Ok || !strings.Contains(r.Output, "up") {
		t.Fatalf("результат: %+v", r)
	}
	ch, logs, budget := monitorDeadlines(&pb.MonitorJob{TimeoutS: 60, LogsTimeoutS: 30})
	if ch != 60*time.Second || logs != 30*time.Second || budget != 96*time.Second {
		t.Fatalf("дедлайны без бюджета: %s %s %s", ch, logs, budget)
	}
}

// Пустое задание (ни команды, ни URL) — провал, а не зелёная проверка.
func TestMonitorEmptyJob(t *testing.T) {
	r := runMonitor(t, &pb.MonitorJob{CheckId: "e1", TimeoutS: 5})
	if r.Ok || !strings.Contains(r.Output, "пустая проверка") {
		t.Fatalf("пустая проверка: %+v", r)
	}
}
