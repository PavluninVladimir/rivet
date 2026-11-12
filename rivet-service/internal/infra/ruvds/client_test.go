package ruvds_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds/ruvdstest"
)

// Токены тестов заведомо фальшивые: fake принимает любую непустую строку.
const testToken = "fake-ruvds-token"

func spec() infra.Spec {
	return infra.Spec{Datacenter: 1, TariffID: 14, OSID: 52, PaymentPeriod: 2, CPU: 2, RAM: 2,
		Drive: 20, DriveTariffID: 1, SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForTests deploy@runner"}
}

func provider(t *testing.T, f *ruvdstest.Fake, token string) *ruvds.Provider {
	t.Helper()
	srv := f.Start()
	t.Cleanup(srv.Close)
	return &ruvds.Provider{C: ruvds.New(srv.URL, token, srv.Client())}
}

func TestQuoteAndBalanceDoNotCreate(t *testing.T) {
	f := ruvdstest.New()
	p := provider(t, f, testToken)
	ctx := context.Background()
	q, err := p.Quote(ctx, spec())
	if err != nil {
		t.Fatal(err)
	}
	// 2×79 + 2×195 + 20×5.5 = 658 за месяц.
	if q.Amount != 658 || q.Currency != "RUB" || q.Period != 2 {
		t.Fatalf("цена: %+v", q)
	}
	b, err := p.Balance(ctx)
	if err != nil || b.Amount != 10000 {
		t.Fatalf("баланс: %+v %v", b, err)
	}
	if f.Creates() != 0 || f.ServerCount() != 0 {
		t.Fatalf("расчёт цены создал сервер: creates=%d servers=%d", f.Creates(), f.ServerCount())
	}
}

func TestCreatePollReady(t *testing.T) {
	f := ruvdstest.New()
	p := provider(t, f, testToken)
	ctx := context.Background()
	srv, err := p.Create(ctx, spec(), infra.Labels{ComputerName: "rivet-1a2b3c4d", Comment: "rivet-env-e1 rivet-host-h1"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.ID == "" || srv.ActionID == "" || srv.State != infra.ServerPending {
		t.Fatalf("создание: %+v", srv)
	}
	for i := 0; i < 5 && srv.State != infra.ServerActive; i++ {
		if srv, err = p.Poll(ctx, srv); err != nil {
			t.Fatal(err)
		}
	}
	if srv.State != infra.ServerActive || !strings.HasPrefix(srv.Address, "10.77.") {
		t.Fatalf("сервер не готов: %+v", srv)
	}
	found, err := p.Find(ctx, "rivet-host-h1")
	if err != nil || found == nil || found.ID != srv.ID {
		t.Fatalf("поиск по метке: %+v %v", found, err)
	}
	if other, err := p.Find(ctx, "rivet-host-other"); err != nil || other != nil {
		t.Fatalf("чужая метка нашлась: %+v %v", other, err)
	}
	bill, err := p.Billing(ctx, srv.ID)
	if err != nil || bill.Cost == nil || bill.Cost.Amount != 658 || bill.PaidTill == nil || bill.PaidTill.Before(time.Now()) {
		t.Fatalf("стоимость: %+v %v", bill, err)
	}
	if f.Creates() != 1 {
		t.Fatalf("создание отправлено %d раз", f.Creates())
	}
	// Ключ SSH добавлен один раз и переиспользуется.
	if _, err := p.Create(ctx, spec(), infra.Labels{ComputerName: "rivet-2", Comment: "x"}); err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, c := range f.Calls() {
		if c.Method == http.MethodPost && c.Path == "/v2/ssh_keys" {
			keys++
		}
	}
	if keys != 1 {
		t.Fatalf("ключ SSH добавлялся %d раз", keys)
	}
	// Удаление и опрос до исчезновения сервера.
	del, err := p.Delete(ctx, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5 && del.State != infra.ServerDeleted; i++ {
		if del, err = p.Poll(ctx, del); err != nil {
			t.Fatal(err)
		}
	}
	if del.State != infra.ServerDeleted {
		t.Fatalf("сервер не удалён: %+v", del)
	}
	// Повторное удаление идемпотентно.
	if again, err := p.Delete(ctx, srv.ID); err != nil || again.State != infra.ServerDeleted {
		t.Fatalf("повторное удаление: %+v %v", again, err)
	}
}

func TestInsufficientBalanceAction(t *testing.T) {
	f := ruvdstest.New()
	p := provider(t, f, "fake-ruvds-lowbalance")
	ctx := context.Background()
	srv, err := p.Create(ctx, spec(), infra.Labels{ComputerName: "x", Comment: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.State != infra.ServerFailed || srv.Failure != "insufficient_balance" || !strings.Contains(srv.Detail, "Insufficient balance") {
		t.Fatalf("нехватка баланса: %+v", srv)
	}
}

func TestWaitPayment(t *testing.T) {
	f := ruvdstest.New()
	f.WaitPayment = true
	p := provider(t, f, testToken)
	ctx := context.Background()
	srv, err := p.Create(ctx, spec(), infra.Labels{ComputerName: "x", Comment: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.State != infra.ServerWaitingPayment {
		t.Fatalf("ожидание оплаты: %+v", srv)
	}
	if srv, _ = p.Poll(ctx, srv); srv.State != infra.ServerWaitingPayment {
		t.Fatalf("опрос до оплаты: %+v", srv)
	}
	f.Pay()
	for i := 0; i < 5 && srv.State != infra.ServerActive; i++ {
		if srv, err = p.Poll(ctx, srv); err != nil {
			t.Fatal(err)
		}
	}
	if srv.State != infra.ServerActive {
		t.Fatalf("после оплаты: %+v", srv)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	ctx := context.Background()
	f := ruvdstest.New()
	bad := provider(t, f, "fake-invalid-token")
	if _, err := bad.Balance(ctx); !errors.Is(err, infra.ErrUnauthorized) || !infra.Definitive(err) {
		t.Fatalf("401: %v", err)
	}
	f.RateLimit = 1
	p := provider(t, f, testToken)
	_, err := p.Quote(ctx, spec())
	if !errors.Is(err, infra.ErrRateLimited) || infra.RetryAfter(err) != time.Second {
		t.Fatalf("429: %v", err)
	}
	// Ошибка 5xx: неизвестный исход, а не отказ.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer ts.Close()
	p5 := &ruvds.Provider{C: ruvds.New(ts.URL, testToken, ts.Client())}
	if _, err := p5.Create(ctx, infra.Spec{Datacenter: 1}, infra.Labels{}); err == nil || infra.Definitive(err) {
		t.Fatalf("502 считается отказом: %v", err)
	}
	// Отказ по существу: 400.
	if _, err := p.Quote(ctx, infra.Spec{}); !errors.Is(err, infra.ErrRejected) {
		t.Fatalf("400: %v", err)
	}
	// Соединение не установлено: запрос не ушёл.
	ts.Close()
	if _, err := p5.Balance(ctx); !errors.Is(err, infra.ErrNotSent) {
		t.Fatalf("dial: %v", err)
	}
}

func TestCatalogLinuxWithSSH(t *testing.T) {
	f := ruvdstest.New()
	p := provider(t, f, testToken)
	cat, err := p.C.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Datacenters) != 2 || len(cat.Tariffs) != 2 || len(cat.DriveTariffs) != 2 || len(cat.PaymentPeriods) != 4 {
		t.Fatalf("каталог: %+v", cat)
	}
	for _, o := range cat.OS {
		if o.Type != "linux" || !o.SSHKeys {
			t.Fatalf("в каталог попала ОС без SSH-ключей: %+v", o)
		}
	}
}

func TestResponseLimitAndPasswordIgnored(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"amount": 1, "pad": "` + strings.Repeat("x", 2<<20) + `"}`))
	}))
	defer big.Close()
	c := ruvds.New(big.URL, testToken, big.Client())
	if _, err := c.Balance(context.Background()); !errors.Is(err, infra.ErrBadResponse) {
		t.Fatalf("ответ больше предела: %v", err)
	}
}
