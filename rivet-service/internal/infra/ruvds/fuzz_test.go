package ruvds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/infra"
)

// FuzzRUVDSParse: ответы RUVDS недоверенные: разбор любого тела и кода
// ответа не паникует, адрес хоста: только проверенный IPv4 без символов
// shell, причина: валидный UTF-8 без управляющих символов и в пределе
// длины (design, решение 14).
func FuzzRUVDSParse(f *testing.F) {
	f.Add(200, `{"virtual_server_id": 6935, "status": "active", "create_progress": 100, "network_v4": [{"ip_address": "186.2.163.33"}]}`)
	f.Add(200, `{"id": 7, "type": "server_create", "status": "error", "progress": 100, "action_error": {"id": "insufficient_balance", "message": "You need 320.00 RUB"}}`)
	f.Add(200, `{"v4": [{"ip_address": "127.0.0.1"}, {"ip_address": "1.2.3.4; rm -rf /"}]}`)
	f.Add(400, `{"id": "json_error", "message": "Insufficient balance\n\x1b[31m"}`)
	f.Add(429, `{"id": "rate_limit_exceeded"}`)
	f.Add(200, `{"cost_rub": 1e400, "payment_period": 2}`)
	f.Add(200, `not json`)
	// Один сервер на процесс фаззинга: ответ текущего входа под мьютексом.
	var mu sync.Mutex
	var curStatus int
	var curBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		status, body := curStatus, curBody
		mu.Unlock()
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	p := &Provider{C: New(ts.URL, "fake-token", ts.Client())}
	f.Fuzz(func(t *testing.T, status int, body string) {
		if status < 200 || status > 599 {
			status = 200
		}
		mu.Lock()
		curStatus, curBody = status, body
		mu.Unlock()
		ctx := context.Background()
		check := func(srv infra.Server, err error) {
			if err != nil {
				if !utf8.ValidString(err.Error()) {
					t.Fatalf("причина не UTF-8: %q", err.Error())
				}
				return
			}
			if strings.ContainsAny(srv.Address, " ;|&$`'\"\n") {
				t.Fatalf("адрес с символами shell: %q", srv.Address)
			}
			for _, s := range []string{srv.Detail, srv.Failure, srv.Comment} {
				if !utf8.ValidString(s) || strings.ContainsAny(s, "\n\r\x1b") || utf8.RuneCountInString(s) > maxErrText+1 {
					t.Fatalf("текст провайдера не очищен: %q", s)
				}
			}
		}
		check(p.Poll(ctx, infra.Server{ID: "6935", ActionID: "7"}))
		check(p.Create(ctx, infra.Spec{Datacenter: 1, OSID: 52}, infra.Labels{}))
		if found, err := p.Find(ctx, "rivet-env-x"); err == nil && found != nil {
			check(*found, nil)
		}
		_, _ = p.Quote(ctx, infra.Spec{})
		_, _ = p.Balance(ctx)
		_, _ = p.Billing(ctx, "6935")
		_, _ = p.C.Catalog(ctx)
	})
}
