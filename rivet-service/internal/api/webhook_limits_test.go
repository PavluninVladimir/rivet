package api

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/orchestrator"
)

// Ограничитель webhook'ов (ревью Opus, финальная проверка): перед rivetd
// стоит TLS-прокси, адрес у всех доставок один, поэтому потолка на адрес
// нет. Общий семафор чтений с ожиданием места, память по пришедшим байтам.

// webhookServer — сервер с одним обработчиком GitHub без базы: события без
// репозитория отвечают 200 ignored.
func webhookServer(t *testing.T, lim *webhookLimiter) (*httptest.Server, string) {
	t.Helper()
	s := &Server{webhookLim: lim}
	srv := httptest.NewServer(http.HandlerFunc(s.githubWebhook))
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "http://")
}

// slowConn — соединение с объявленным телом, из которого прислано только
// sent байт.
func slowConn(t *testing.T, addr string, declared, sent int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nX-GitHub-Event: pull_request\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		declared, strings.Repeat(" ", sent))
	return c
}

// trickle — честная доставка, тело которой идёт по частям за ~150 мс (так
// его передаёт потоковый прокси).
type trickle struct {
	parts []string
	i     int
}

func (t *trickle) Read(p []byte) (int, error) {
	if t.i >= len(t.parts) {
		return 0, io.EOF
	}
	if t.i > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	n := copy(p, t.parts[t.i])
	t.i++
	return n, nil
}

func deliver(t *testing.T, url, event string, body io.Reader, size int64) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.ContentLength = size
	req.Header.Set("X-GitHub-Event", event)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("%s: %v", event, err)
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Пачка из трёх честных доставок через один адрес (push, pull_request,
// workflow_run) проходит: раньше третья получала 429.
func TestWebhookHonestBurstSameAddress(t *testing.T) {
	srv, _ := webhookServer(t, newWebhookLimiter(webhookReaders, webhookMemoryBudget, 10*time.Second))
	body := `{"action":"closed","pad":"` + strings.Repeat("x", 300) + `"}`
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i, ev := range []string{"push", "pull_request", "workflow_run"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := &trickle{parts: []string{body[:100], body[100:200], body[200:]}}
			codes[i] = deliver(t, srv.URL, ev, tr, int64(len(body)))
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("доставка %d: %d (все: %v)", i, c, codes)
		}
	}
}

// Два медленных соединения без тела не блокируют честные доставки: они
// занимают два места из восьми и почти не держат памяти.
func TestWebhookSlowClientsDoNotBlock(t *testing.T) {
	srv, addr := webhookServer(t, newWebhookLimiter(webhookReaders, webhookMemoryBudget, 10*time.Second))
	slowConn(t, addr, 1000, 1)
	slowConn(t, addr, 1000, 1)
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 5; i++ {
		start := time.Now()
		code := deliver(t, srv.URL, "pull_request", strings.NewReader(`{"action":"closed"}`), -1)
		if code != http.StatusOK || time.Since(start) > 2*time.Second {
			t.Fatalf("честная доставка %d при медленных соединениях: %d за %v", i, code, time.Since(start))
		}
	}
}

// Бюджет памяти считается по пришедшим байтам: занятый бюджет даёт 503
// после ожидания, а не немедленный отказ.
func TestWebhookMemoryBudgetOverflow(t *testing.T) {
	lim := newWebhookLimiter(webhookReaders, 1000, 300*time.Millisecond)
	srv, addr := webhookServer(t, lim)
	// Медленный клиент прислал 900 байт из 1000 и молчит: бюджет почти занят.
	slowConn(t, addr, 1000, 900)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	body := `{"action":"closed","pad":"` + strings.Repeat("x", 200) + `"}`
	code := deliver(t, srv.URL, "pull_request", strings.NewReader(body), int64(len(body)))
	if code != http.StatusServiceUnavailable || time.Since(start) < lim.wait {
		t.Fatalf("переполнение бюджета: %d за %v", code, time.Since(start))
	}
}

// Мест для чтения нет дольше ожидания: 503, а не немедленный отказ.
func TestWebhookSlotsExhausted(t *testing.T) {
	lim := newWebhookLimiter(webhookReaders, webhookMemoryBudget, 300*time.Millisecond)
	srv, addr := webhookServer(t, lim)
	for i := 0; i < webhookReaders; i++ {
		slowConn(t, addr, 1000, 1)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	code := deliver(t, srv.URL, "pull_request", strings.NewReader(`{"action":"closed"}`), -1)
	if code != http.StatusServiceUnavailable || time.Since(start) < lim.wait {
		t.Fatalf("все места заняты: %d за %v", code, time.Since(start))
	}
}

// Репро ревью ограничителя (TestLimSlowlorisSustained): один адрес держит
// 24 простаивающих соединения и сразу переоткрывает закрытые. При 8 местах
// честная доставка ждала до 9 с при таймауте хостинга 10 с. Теперь мест 128,
// и доставка проходит без ожидания.
func TestWebhookSlowlorisSustained(t *testing.T) {
	srv, addr := webhookServer(t, newWebhookLimiter(webhookReaders, webhookMemoryBudget, webhookSlotWait))
	var stop atomic.Bool
	var wg sync.WaitGroup
	defer func() {
		stop.Store(true)
		srv.CloseClientConnections()
		wg.Wait()
	}()
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				fmt.Fprint(c, "POST / HTTP/1.1\r\nHost: x\r\nX-GitHub-Event: pull_request\r\nContent-Length: 1000\r\n\r\n{")
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	cl := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 5; i++ {
		start := time.Now()
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"action":"closed"}`))
		req.Header.Set("X-GitHub-Event", "pull_request")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("доставка %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || time.Since(start) > 2*time.Second {
			t.Fatalf("доставка %d при атаке: %d за %v", i, resp.StatusCode, time.Since(start))
		}
	}
}

// Соединение, которое прислало байт и замолчало, держит место не дольше
// дедлайна простоя, а не всё время чтения.
func TestWebhookIdleReadTimeout(t *testing.T) {
	lim := newWebhookLimiter(webhookReaders, webhookMemoryBudget, webhookSlotWait)
	lim.idle = 300 * time.Millisecond
	_, addr := webhookServer(t, lim)
	c := slowConn(t, addr, 1000, 1)
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.Copy(io.Discard, c)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("простаивающее соединение держалось %v", time.Since(start))
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(lim.slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(lim.slots) != 0 {
		t.Fatalf("место не отпущено: занято %d", len(lim.slots))
	}
}

// Push без Content-Length (chunked через прокси) получает лимит push, а не
// обычный 1 МиБ (ревью ограничителя, P3-7).
func TestWebhookChunkedPushLimit(t *testing.T) {
	srv, _ := webhookServer(t, nil)
	body := `{"ref":"refs/heads/main","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	code := deliver(t, srv.URL, "push", io.MultiReader(strings.NewReader(body)), -1)
	if code == http.StatusRequestEntityTooLarge {
		t.Fatal("chunked push больше 1 МиБ отклонён")
	}
}

// Объявленное тело больше лимита отклоняется до чтения.
func TestWebhookContentLengthTooLarge(t *testing.T) {
	_, addr := webhookServer(t, nil)
	c := slowConn(t, addr, webhookBodyLimit+1, 0)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "413") {
		t.Fatalf("Content-Length больше лимита: %q", buf[:n])
	}
}

// Отказы merge задачи описания различаются кодом: лимит сдвигов ветки
// оборачивает сам сдвиг и проверяется раньше него (ревью ограничителя, P3-2).
func TestArchitectureMergeErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("merge: %w", orchestrator.ErrArchitectureHeadMovedLimit), http.StatusConflict, "architecture_head_moved_limit"},
		{fmt.Errorf("merge: %w", orchestrator.ErrArchitectureHeadMoved), http.StatusConflict, "architecture_head_moved"},
		{orchestrator.ErrArchitecturePRRejected, http.StatusConflict, "architecture_outside_docs"},
		{fmt.Errorf("%w: 502", orchestrator.ErrArchitectureFilesUnavailable), http.StatusBadGateway, "architecture_files_unavailable"},
	} {
		status, ae, ok := architectureMergeError(tc.err)
		if !ok || status != tc.status || ae.Code != tc.code {
			t.Fatalf("%v: %d %q %v", tc.err, status, ae.Code, ok)
		}
	}
	if _, _, ok := architectureMergeError(fmt.Errorf("другое")); ok {
		t.Fatal("посторонняя ошибка принята за отказ задачи описания")
	}
}
