package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Хост окружения от провайдера инфраструктуры (add-environment-provisioning,
// спека deployment «Хост окружения от провайдера инфраструктуры», design
// решения 1–13). Тик ведёт хосты по состояниям: цена, создание, опрос,
// bootstrap, стоимость, удаление. Создание сервера у платного провайдера
// отправляется только из approved с решением человека, удаление: только из
// deleting с подтверждением; оба условия проверяет и CAS, и CHECK таблицы.

// Интервалы и пределы. Переменные ради тестов.
var (
	hostPollInterval  = 5 * time.Second
	hostRetryInterval = 30 * time.Second
	// hostLease: аренда хоста тиком: шаг с несколькими запросами к
	// провайдеру (по 30 секунд на запрос) укладывается с запасом.
	hostLease = 10 * time.Minute
	// hostUnknownWindow: сколько ищем сервер по метке после потери ответа
	// на создание, прежде чем признать исход неизвестным.
	hostUnknownWindow = 15 * time.Minute
	// hostSlowCreate: после этого срока незавершённое создание поднимает
	// эскалацию (опрос продолжается: сервер может достроиться).
	hostSlowCreate = 2 * time.Hour
	// hostQuoteGiveUp: расчёт цены без ответа провайдера дольше: провал.
	hostQuoteGiveUp   = time.Hour
	hostJobTimeout    = 20 * time.Minute
	hostJobGrace      = 2 * time.Minute
	hostSSHWait       = 5 * time.Minute
	hostJobRetryAfter = time.Minute
	hostJobAttempts   = 3
	hostBillingEvery  = 6 * time.Hour
	hostTickBudget    = 20
)

// hostFarFuture: «больше не обрабатывать тиком» (локальный готовый хост).
const hostFarFuture = 100 * 365 * 24 * time.Hour

// Виды заданий хоста.
const (
	hostJobBootstrap = "bootstrap"
	hostJobTeardown  = "teardown"
)

// hostSSHUser: логин на сервере RUVDS.
const hostSSHUser = "root"

func newHostJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "hj-" + hex.EncodeToString(b)
}

// infraSpec: привязка окружения в терминах провайдера.
func infraSpec(p domain.ProvisionConfig) infra.Spec {
	return infra.Spec{Datacenter: p.Datacenter, TariffID: p.TariffID, OSID: p.OSID, TemplateID: p.TemplateID,
		PaymentPeriod: p.PaymentPeriod, CPU: p.CPU, RAM: p.RAM, Drive: p.Drive, DriveTariffID: p.DriveTariffID,
		SSHPublicKey: strings.TrimSpace(p.SSHPublicKey), Bootstrap: p.BootstrapCmd}
}

// hostProvider: провайдер хоста с токеном его подключения.
func (e *Engine) hostProvider(ctx context.Context, h domain.EnvHost) (infra.Provider, error) {
	token := ""
	if h.Provider != infra.KindLocal {
		if h.ConnectionID == "" {
			return nil, &infra.Error{Kind: infra.ErrUnauthorized, Message: "подключение к провайдеру удалено"}
		}
		_, t, err := e.St.InfraConnectionToken(ctx, h.ConnectionID, e.Box)
		if err != nil {
			return nil, &infra.Error{Kind: infra.ErrUnauthorized, Message: err.Error()}
		}
		token = t
	}
	f := e.Infra
	if f == nil {
		f = &infra.Factory{}
	}
	return f.Provider(h.Provider, token)
}

// tickHosts: обработка хостов, которым пора (вызывается из tickOps).
func (e *Engine) tickHosts(ctx context.Context) {
	var skip []string
	for i := 0; i < hostTickBudget; i++ {
		h, ok, err := e.St.ClaimDueHost(ctx, hostLease, skip)
		if err != nil {
			slog.Error("хосты окружений", "err", err)
			return
		}
		if !ok {
			return
		}
		skip = append(skip, h.ID)
		next := e.processHost(ctx, h)
		if err := e.St.ReleaseHost(ctx, h.ID, next, h.Updated); err != nil {
			slog.Error("хост окружения: освобождение", "host", h.ID, "err", err)
		}
	}
}

// processHost: шаг хоста по его состоянию; возвращает время следующей
// обработки.
func (e *Engine) processHost(ctx context.Context, h domain.EnvHost) time.Time {
	now := e.Now()
	p, err := e.hostProvider(ctx, h)
	if err != nil {
		if h.State == domain.HostQuoting || h.State == domain.HostApproved {
			e.failHost(ctx, h, []string{h.State}, domain.HostFailUnauthorized, "провайдер недоступен: "+err.Error())
			return now.Add(hostRetryInterval)
		}
		e.hostNote(ctx, h, "провайдер недоступен: "+err.Error())
		return now.Add(hostRetryInterval)
	}
	switch h.State {
	case domain.HostQuoting:
		return e.hostQuote(ctx, h, p)
	case domain.HostApproved:
		return e.hostCreate(ctx, h, p)
	case domain.HostCreating:
		return e.hostPollCreate(ctx, h, p)
	case domain.HostBootstrapping:
		return e.hostJob(ctx, h, p, hostJobBootstrap)
	case domain.HostReady:
		return e.hostBilling(ctx, h, p)
	case domain.HostDeleting:
		return e.hostDelete(ctx, h, p)
	}
	return now.Add(hostFarFuture)
}

// backoff: когда повторить после ошибки провайдера.
func (e *Engine) backoff(err error) time.Time {
	if d := infra.RetryAfter(err); d > 0 {
		return e.Now().Add(d)
	}
	return e.Now().Add(hostRetryInterval)
}

// hostNote: пояснение к состоянию без перехода (сбой сети и т. п.).
func (e *Engine) hostNote(ctx context.Context, h domain.EnvHost, detail string) {
	detail = cleanDetail(detail)
	if h.Detail == detail {
		return
	}
	if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{h.State},
		Set: map[string]any{"detail": detail}}); err != nil {
		slog.Error("хост окружения: пояснение", "host", h.ID, "err", err)
	}
}

// failHost: конечный провал: сервер не создан (или исход неизвестен), у
// человека эскалация с причиной.
func (e *Engine) failHost(ctx context.Context, h domain.EnvHost, from []string, failure, detail string) {
	detail = cleanDetail(detail)
	if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: from, To: domain.HostFailed,
		Set: map[string]any{"failure": failure, "detail": detail, "ended_at": store.SQL("now()"),
			"job_id": "", "job_kind": "", "job_runner": "", "job_started_at": nil},
		Resolve: []domain.AttentionReason{domain.AttHostCreate, domain.AttHostDelete},
		Open:    []store.HostAttention{{Reason: domain.AttHostFailed, Message: detail}},
		Event:   "хост не создан: " + detail, Action: "fail"}); err != nil {
		slog.Error("хост окружения: провал", "host", h.ID, "err", err)
	}
}

// cleanDetail: текст провайдера и вывода для event log и эскалации:
// маскирование и предел длины.
func cleanDetail(s string) string {
	s = redact.String(s)
	r := []rune(s)
	if len(r) > 1500 {
		s = "…" + string(r[len(r)-1500:])
	}
	return s
}

// money: сумма для человека.
func money(amount float64, currency string) string {
	sym := map[string]string{"RUB": "₽", "USD": "$", "EUR": "€"}[currency]
	if sym == "" {
		sym = currency
	}
	if amount == math.Trunc(amount) {
		return fmt.Sprintf("%.0f %s", amount, sym)
	}
	return fmt.Sprintf("%.2f %s", amount, sym)
}

// specText: конфигурация сервера для решения человека: названия из
// справочника, где они есть, иначе идентификаторы.
func specText(p domain.ProvisionConfig, labels map[string]string) string {
	name := func(key string, id int) string {
		if v := labels[key]; v != "" {
			return v
		}
		return fmt.Sprintf("№%d", id)
	}
	osPart := "ОС " + name("os", p.OSID)
	if p.TemplateID != "" {
		osPart = "шаблон " + p.TemplateID
	}
	return fmt.Sprintf("дата-центр %s, тариф %s, %d vCPU, %g ГБ RAM, диск %d ГБ (%s), %s",
		name("datacenter", p.Datacenter), name("tariff", p.TariffID), p.CPU, p.RAM, p.Drive,
		name("drive_tariff", p.DriveTariffID), osPart)
}

// ─── расчёт цены ─────────────────────────────────────────────────────────

func (e *Engine) hostQuote(ctx context.Context, h domain.EnvHost, p infra.Provider) time.Time {
	now := e.Now()
	if !p.Paid() {
		// Бесплатный хост (машина runner'а): решения нет, сразу первичная
		// настройка. Создание у такого провайдера ничего не тратит.
		srv, err := p.Create(ctx, infraSpec(h.Spec), infra.Labels{})
		if err != nil {
			e.failHost(ctx, h, []string{domain.HostQuoting}, domain.HostFailRejected, err.Error())
			return now
		}
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostQuoting},
			To: domain.HostBootstrapping, Set: map[string]any{"server_id": srv.ID, "address": srv.Address,
				"attempts": 0, "detail": ""},
			Event: "хост на машине deploy-runner'а: проверка docker compose", Action: "create"}); err != nil {
			slog.Error("хост окружения: локальный", "host", h.ID, "err", err)
		}
		return now
	}
	// Сервер окружения у провайдера уже есть (прошлая попытка, потерянный
	// ответ): принимаем его без решения: денег это не тратит.
	found, err := p.Find(ctx, domain.HostEnvMarker(h.EnvID))
	if err != nil {
		return e.quoteError(ctx, h, err)
	}
	if found != nil {
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostQuoting},
			To: domain.HostCreating, Set: map[string]any{"server_id": found.ID, "action_id": "", "detail": ""},
			Event:  "у провайдера найден сервер окружения " + found.ID + ": принят без нового создания",
			Action: "adopt"}); err != nil {
			slog.Error("хост окружения: принятие сервера", "host", h.ID, "err", err)
		}
		return now
	}
	q, err := p.Quote(ctx, infraSpec(h.Spec))
	if err != nil {
		return e.quoteError(ctx, h, err)
	}
	bal, err := p.Balance(ctx)
	if err != nil && !errors.Is(err, infra.ErrForbidden) {
		return e.quoteError(ctx, h, err)
	}
	set := map[string]any{"quote_amount": q.Amount, "quote_currency": q.Currency, "quote_period": q.Period,
		"detail": "", "failure": ""}
	labels := h.SpecLabels
	if d, ok := p.(infra.Describer); ok {
		// Названия нужны только человеку: без справочника решение
		// показывает идентификаторы.
		if l, err := d.Describe(ctx, infraSpec(h.Spec)); err == nil {
			labels = l
			set["spec_labels"] = l
		}
	}
	msg := fmt.Sprintf("создать сервер RUVDS: %s, период %s. Стоимость %s. ",
		specText(h.Spec, labels), ruvds.PeriodLabel(q.Period), money(q.Amount, q.Currency))
	if bal != nil {
		set["balance_amount"], set["balance_currency"] = bal.Amount, bal.Currency
		msg += "Баланс " + money(bal.Amount, bal.Currency) + "."
	}
	msg += " Сервер создаётся только после подтверждения владельцем проекта."
	if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostQuoting},
		To: domain.HostAwaitingApproval, Set: set,
		Open:   []store.HostAttention{{Reason: domain.AttHostCreate, Message: msg}},
		Event:  fmt.Sprintf("цена сервера %s за %s: ждёт решения", money(q.Amount, q.Currency), ruvds.PeriodLabel(q.Period)),
		Action: "quote"}); err != nil {
		slog.Error("хост окружения: цена", "host", h.ID, "err", err)
	}
	return now
}

// quoteError: ошибка расчёта цены: отказ по существу: провал, сбой:
// повтор, пока не выйдет время.
func (e *Engine) quoteError(ctx context.Context, h domain.EnvHost, err error) time.Time {
	switch {
	case errors.Is(err, infra.ErrRateLimited):
		return e.backoff(err)
	case errors.Is(err, infra.ErrUnauthorized), errors.Is(err, infra.ErrForbidden), errors.Is(err, infra.ErrRejected):
		e.failHost(ctx, h, []string{domain.HostQuoting}, failureCode(err), "расчёт цены: "+err.Error())
		return e.Now()
	}
	if e.Now().Sub(h.Created) > hostQuoteGiveUp {
		e.failHost(ctx, h, []string{domain.HostQuoting}, domain.HostFailTimeout, "провайдер не ответил на расчёт цены: "+err.Error())
		return e.Now()
	}
	e.hostNote(ctx, h, "расчёт цены: "+err.Error())
	return e.backoff(err)
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, infra.ErrInsufficientBalance):
		return domain.HostFailInsufficientBalance
	case errors.Is(err, infra.ErrUnauthorized), errors.Is(err, infra.ErrForbidden):
		return domain.HostFailUnauthorized
	}
	return domain.HostFailRejected
}

// ─── создание ────────────────────────────────────────────────────────────

// hostCreate: решение есть: цена пересчитывается, баланс проверяется, и
// только после этого CAS approved → creating отправляет одно создание.
func (e *Engine) hostCreate(ctx context.Context, h domain.EnvHost, p infra.Provider) time.Time {
	now := e.Now()
	if !p.Paid() || h.ApprovedBy == "" || h.ApprovedAmount == nil {
		// Бесплатному провайдеру сюда не попасть, а платный без решения
		// создавать нельзя ни при каком раскладе.
		slog.Error("хост окружения: approved без решения человека", "host", h.ID)
		return now.Add(hostFarFuture)
	}
	spec := infraSpec(h.Spec)
	q, err := p.Quote(ctx, spec)
	if err != nil {
		if errors.Is(err, infra.ErrUnauthorized) || errors.Is(err, infra.ErrForbidden) || errors.Is(err, infra.ErrRejected) {
			e.failHost(ctx, h, []string{domain.HostApproved}, failureCode(err), "пересчёт цены: "+err.Error())
			return now
		}
		e.hostNote(ctx, h, "пересчёт цены: "+err.Error())
		return e.backoff(err)
	}
	if q.Amount > *h.ApprovedAmount+0.005 {
		msg := fmt.Sprintf("цена сервера изменилась: подтверждено %s, сейчас %s за %s. %s. Подтвердите новую цену или отклоните.",
			money(*h.ApprovedAmount, h.QuoteCurrency), money(q.Amount, q.Currency), ruvds.PeriodLabel(q.Period), specText(h.Spec, h.SpecLabels))
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostApproved},
			To: domain.HostAwaitingApproval,
			Set: map[string]any{"quote_amount": q.Amount, "quote_currency": q.Currency, "quote_period": q.Period,
				"approved_by": "", "approved_at": nil, "approved_amount": nil},
			Open:  []store.HostAttention{{Reason: domain.AttHostCreate, Message: msg}},
			Event: "цена выросла до " + money(q.Amount, q.Currency) + ": нужно новое решение", Action: "requote"}); err != nil {
			slog.Error("хост окружения: новая цена", "host", h.ID, "err", err)
		}
		return now
	}
	bal, err := p.Balance(ctx)
	if err != nil && !errors.Is(err, infra.ErrForbidden) {
		e.hostNote(ctx, h, "баланс: "+err.Error())
		return e.backoff(err)
	}
	if bal != nil && bal.Amount < q.Amount {
		e.failHost(ctx, h, []string{domain.HostApproved}, domain.HostFailInsufficientBalance,
			fmt.Sprintf("недостаточно средств: баланс %s, нужно %s. Пополните баланс RUVDS и запросите сервер снова",
				money(bal.Amount, bal.Currency), money(q.Amount, q.Currency)))
		return now
	}
	// Единственная точка отправки создания: CAS с отметкой времени до
	// запроса. Второй процесс, тик после рестарта и повтор человека сюда
	// уже не пройдут: состояние creating.
	_, ok, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostApproved},
		To: domain.HostCreating, Where: "approved_by <> '' AND create_sent_at IS NULL",
		Set: map[string]any{"create_sent_at": store.SQL("now()"), "detail": ""}})
	if err != nil || !ok {
		if err != nil {
			slog.Error("хост окружения: отметка создания", "host", h.ID, "err", err)
		}
		return now
	}
	labels := infra.Labels{ComputerName: domain.HostComputerName(h.EnvID),
		Comment: domain.HostEnvMarker(h.EnvID) + " " + domain.HostMarker(h.ID) + " (Rivet)"}
	srv, err := p.Create(ctx, spec, labels)
	if err != nil {
		return e.createError(ctx, h, err)
	}
	set := map[string]any{"server_id": srv.ID, "action_id": srv.ActionID}
	if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating}, Set: set,
		Event: "создание сервера " + srv.ID + " отправлено", Action: "create", Actor: ""}); err != nil {
		slog.Error("хост окружения: сервер создан, запись не удалась", "host", h.ID, "server", srv.ID, "err", err)
	}
	h.ServerID, h.ActionID, h.State = srv.ID, srv.ActionID, domain.HostCreating
	return e.applyPoll(ctx, h, srv)
}

// createError: ошибка отправки создания. Отказ провайдера по существу:
// сервер не создан: провал или (не отправлено, лимит) повтор из approved.
// Иначе исход неизвестен: хост остаётся в creating без сервера и ищет его
// по метке, повторной отправки нет.
func (e *Engine) createError(ctx context.Context, h domain.EnvHost, err error) time.Time {
	now := e.Now()
	switch {
	case errors.Is(err, infra.ErrNotSent), errors.Is(err, infra.ErrRateLimited):
		if _, _, cerr := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating},
			To: domain.HostApproved, Where: "server_id = ''",
			Set: map[string]any{"create_sent_at": nil, "detail": cleanDetail("создание не отправлено: " + err.Error())}}); cerr != nil {
			slog.Error("хост окружения: возврат к решению", "host", h.ID, "err", cerr)
		}
		return e.backoff(err)
	case infra.Definitive(err):
		e.failHost(ctx, h, []string{domain.HostCreating}, failureCode(err), "создание сервера: "+err.Error())
		return now
	}
	e.hostNote(ctx, h, "ответ на создание не получен, ищем сервер по метке: "+err.Error())
	return now.Add(hostPollInterval)
}

// hostPollCreate: опрос создания; без идентификатора сервера: поиск по
// метке хоста (ответ на создание потерялся).
func (e *Engine) hostPollCreate(ctx context.Context, h domain.EnvHost, p infra.Provider) time.Time {
	now := e.Now()
	if h.ServerID == "" {
		found, err := p.Find(ctx, domain.HostMarker(h.ID))
		if err != nil {
			e.hostNote(ctx, h, "поиск сервера по метке: "+err.Error())
			return e.backoff(err)
		}
		if found == nil {
			sent := h.Created
			if h.CreateSentAt != nil {
				sent = *h.CreateSentAt
			}
			if now.Sub(sent) > hostUnknownWindow {
				e.failHost(ctx, h, []string{domain.HostCreating}, domain.HostFailUnknownOutcome,
					"исход создания сервера неизвестен: ответ провайдера потерян, сервер с меткой "+
						domain.HostMarker(h.ID)+" не найден. Проверьте панель RUVDS: если сервер там есть, удалите его или запросите хост снова: Rivet найдёт сервер окружения по метке и примет его")
				return now
			}
			return now.Add(hostPollInterval)
		}
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating},
			Where: "server_id = ''", Set: map[string]any{"server_id": found.ID, "action_id": "", "detail": ""},
			Event: "сервер " + found.ID + " найден по метке после потери ответа", Action: "adopt"}); err != nil {
			slog.Error("хост окружения: сервер по метке", "host", h.ID, "err", err)
		}
		return now
	}
	srv, err := p.Poll(ctx, infra.Server{ID: h.ServerID, ActionID: h.ActionID})
	if err != nil {
		e.hostNote(ctx, h, "опрос сервера: "+err.Error())
		return e.backoff(err)
	}
	return e.applyPoll(ctx, h, srv)
}

// applyPoll: реакция на состояние сервера при создании.
func (e *Engine) applyPoll(ctx context.Context, h domain.EnvHost, srv infra.Server) time.Time {
	now := e.Now()
	switch srv.State {
	case infra.ServerActive:
		if srv.Address == "" && needsAddress(h) {
			return now.Add(hostPollInterval)
		}
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating},
			To: domain.HostBootstrapping, Set: map[string]any{"address": srv.Address, "waiting_payment": false,
				"attempts": 0, "detail": "", "failure": ""},
			Resolve: []domain.AttentionReason{domain.AttHostFailed},
			Event:   "сервер готов, адрес " + srv.Address + ": первичная настройка", Action: "ready"}); err != nil {
			slog.Error("хост окружения: сервер готов", "host", h.ID, "err", err)
		}
		return now
	case infra.ServerWaitingPayment, infra.ServerBlocked:
		msg := "сервер " + h.ServerID + " ждёт оплаты у провайдера: пополните баланс RUVDS, после оплаты он достроится сам"
		if srv.State == infra.ServerBlocked {
			msg = "сервер " + h.ServerID + " заблокирован провайдером: разберитесь в панели RUVDS или удалите хост"
		}
		if !h.WaitingPayment {
			if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating},
				Set:   map[string]any{"waiting_payment": true, "detail": msg},
				Open:  []store.HostAttention{{Reason: domain.AttHostFailed, Message: msg}},
				Event: msg, Action: "waiting_payment"}); err != nil {
				slog.Error("хост окружения: ожидание оплаты", "host", h.ID, "err", err)
			}
		}
		return now.Add(hostPollInterval * 6)
	case infra.ServerFailed:
		detail := srv.Detail
		if detail == "" {
			detail = srv.Failure
		}
		failure := domain.HostFailRejected
		msg := "провайдер не создал сервер: " + detail
		if srv.Failure == "insufficient_balance" {
			failure = domain.HostFailInsufficientBalance
			msg = "недостаточно средств на балансе RUVDS: " + detail + ". Пополните баланс и запросите сервер снова"
		}
		e.failHost(ctx, h, []string{domain.HostCreating}, failure, msg)
		return now
	case infra.ServerDeleted:
		e.failHost(ctx, h, []string{domain.HostCreating}, domain.HostFailRejected,
			"сервер "+h.ServerID+" удалён у провайдера до готовности")
		return now
	}
	// Идёт создание. Ожидание оплаты снято: эскалация закрывается.
	if h.WaitingPayment {
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating},
			Set:     map[string]any{"waiting_payment": false, "detail": ""},
			Resolve: []domain.AttentionReason{domain.AttHostFailed},
			Event:   "оплата получена: сервер создаётся", Action: "paid"}); err != nil {
			slog.Error("хост окружения: оплата", "host", h.ID, "err", err)
		}
	}
	started := h.Created
	if h.CreateSentAt != nil {
		started = *h.CreateSentAt
	}
	if now.Sub(started) > hostSlowCreate && h.Failure != domain.HostFailTimeout {
		msg := fmt.Sprintf("сервер %s не готов больше %s: проверьте его в панели RUVDS или удалите хост", h.ServerID, hostSlowCreate)
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostCreating},
			Set:  map[string]any{"failure": domain.HostFailTimeout, "detail": msg},
			Open: []store.HostAttention{{Reason: domain.AttHostFailed, Message: msg}}}); err != nil {
			slog.Error("хост окружения: долгое создание", "host", h.ID, "err", err)
		}
	}
	return now.Add(hostPollInterval)
}

// needsAddress: хосту нужен адрес (удалённый провайдер).
func needsAddress(h domain.EnvHost) bool { return h.Provider != infra.KindLocal }

// ─── задания deploy-runner'а ─────────────────────────────────────────────

// hostEnv: переменные скриптов хоста и публикаций окружения с провайдером.
func hostEnv(env domain.Environment, address string) map[string]string {
	compose := domain.ComposeProject(env.ProjectID, env.ID)
	m := map[string]string{"RIVET_ENV": env.Name, "COMPOSE_PROJECT_NAME": compose, "RIVET_COMPOSE_PROJECT": compose}
	if p := env.Config.Provision; p != nil {
		if p.Provider == domain.ProvisionLocal && p.Port > 0 {
			m["RIVET_HOST_PORT"] = fmt.Sprint(p.Port)
		}
	}
	if address != "" {
		m["RIVET_HOST_ADDRESS"] = address
	}
	return m
}

// exportPrefix: те же переменные как префикс скрипта (команды выкатки и
// наблюдения приходят runner'у строкой): имена и значения задаёт Rivet.
func exportPrefix(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("export")
	for _, k := range sortedVarKeys(vars) {
		fmt.Fprintf(&b, " %s=%s", k, shellQuote(vars[k]))
	}
	b.WriteString("\n")
	return b.String()
}

func sortedVarKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sshHost: адрес ssh удалённого хоста.
func sshHost(h domain.EnvHost) string {
	if h.Provider == infra.KindLocal || h.Address == "" {
		return ""
	}
	return hostSSHUser + "@" + h.Address
}

// hostJob: bootstrap (bootstrapping) или снятие (deleting локального):
// одно задание за раз, потерянное по дедлайну считается неудачной попыткой.
func (e *Engine) hostJob(ctx context.Context, h domain.EnvHost, p infra.Provider, kind string) time.Time {
	now := e.Now()
	if h.JobID != "" {
		if h.JobStartedAt != nil && now.Sub(*h.JobStartedAt) > hostJobTimeout+hostJobGrace {
			e.hostJobFailed(ctx, h, "результат задания не получен за "+(hostJobTimeout+hostJobGrace).String())
			return now
		}
		if h.JobStartedAt != nil {
			return h.JobStartedAt.Add(hostJobTimeout + hostJobGrace + time.Second)
		}
		return now.Add(hostRetryInterval)
	}
	env, repo, err := e.St.HostEnvironment(ctx, h.ID)
	if err != nil {
		slog.Error("хост окружения: окружение", "host", h.ID, "err", err)
		return now.Add(hostRetryInterval)
	}
	spec := infraSpec(h.Spec)
	script := p.BootstrapScript(spec)
	if kind == hostJobTeardown {
		script = p.TeardownScript(spec, h.RemoveVolumes)
	}
	runnerID, ok, err := e.St.HostRunner(ctx, env)
	if err != nil {
		slog.Error("хост окружения: runner", "host", h.ID, "err", err)
		return now.Add(hostRetryInterval)
	}
	if !ok {
		e.hostNote(ctx, h, "ждём deploy-runner с поддержкой заданий хоста (capability deploy"+capsText(env.RunnerCaps)+")")
		return now.Add(hostRetryInterval)
	}
	jobID := newHostJobID()
	_, claimed, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{h.State}, Where: "job_id = ''",
		Set: map[string]any{"job_id": jobID, "job_kind": kind, "job_runner": runnerID,
			"job_started_at": store.SQL("now()"), "detail": ""}})
	if err != nil || !claimed {
		return now.Add(hostRetryInterval)
	}
	vars := hostEnv(env, h.Address)
	vars["RIVET_REPO"] = repo
	remote := p.Remote()
	job := &pb.HostJob{JobId: jobID, EnvName: env.Name, Host: sshHost(h), Script: script,
		TimeoutS: int32(hostJobTimeout.Seconds()), HostKey: h.HostKey, Env: vars}
	if remote {
		job.AcceptNewHostKey = h.HostKey == ""
		job.SshWaitS = int32(hostSSHWait.Seconds())
	}
	msg := &pb.PlaneMsg{MsgId: jobID, Kind: &pb.PlaneMsg_HostJob{HostJob: job}}
	if !e.Out.Send(runnerID, msg) {
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{h.State},
			Where: "job_id = {1}", Args: []any{jobID},
			Set: map[string]any{"job_id": "", "job_kind": "", "job_runner": "", "job_started_at": nil,
				"detail": "runner " + runnerID + " недоступен"}}); err != nil {
			slog.Error("хост окружения: задание не отправлено", "host", h.ID, "err", err)
		}
		return now.Add(hostRetryInterval)
	}
	return now.Add(hostJobTimeout + hostJobGrace + time.Second)
}

func capsText(caps []string) string {
	if len(caps) == 0 {
		return ""
	}
	return ", " + strings.Join(caps, ", ")
}

// OnHostJobResult: итог задания хоста от runner'а: только исполнителя
// текущего задания.
func (e *Engine) OnHostJobResult(ctx context.Context, runnerID string, r *pb.HostJobResult) error {
	if r.JobId == "" {
		return nil
	}
	h, err := e.St.HostByJob(ctx, r.JobId)
	if errors.Is(err, store.ErrNotFound) {
		slog.Warn("итог чужого или устаревшего задания хоста отброшен", "job", r.JobId, "runner", runnerID)
		return nil
	}
	if err != nil {
		return err
	}
	if h.JobRunner != runnerID {
		slog.Warn("итог задания хоста от другого runner'а отброшен", "job", r.JobId, "runner", runnerID, "owner", h.JobRunner)
		return nil
	}
	output := cleanDetail(r.Output)
	if !r.Ok {
		e.hostJobFailed(ctx, h, output)
		return nil
	}
	switch h.JobKind {
	case hostJobBootstrap:
		remote := h.Provider != infra.KindLocal
		hostKey := h.HostKey
		if remote && hostKey == "" {
			hostKey = strings.TrimSpace(r.HostKey)
			if hostKey == "" || domain.ValidHostKey(hostKey) != nil {
				e.hostJobFailed(ctx, h, "runner не вернул ключ хоста при первом подключении")
				return nil
			}
		}
		next := e.Now()
		if !remote {
			next = e.Now().Add(hostFarFuture)
		}
		addr := h.Address
		_, ok, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostBootstrapping},
			To: domain.HostReady, Where: "job_id = {1}", Args: []any{r.JobId},
			Set: map[string]any{"host_key": hostKey, "job_id": "", "job_kind": "", "job_runner": "",
				"job_started_at": nil, "attempts": 0, "needs_retry": false, "detail": "", "failure": "",
				"next_attempt_at": next},
			Resolve: []domain.AttentionReason{domain.AttHostFailed},
			Env: func(cfg *domain.EnvConfig) {
				if remote {
					cfg.Host, cfg.HostKey = hostSSHUser+"@"+addr, hostKey
				} else {
					cfg.Host, cfg.HostKey = "", ""
				}
			},
			Event: "хост готов к публикациям", Action: "bootstrapped"})
		if err != nil {
			return err
		}
		if !ok {
			slog.Warn("итог bootstrap после смены состояния хоста отброшен", "host", h.ID)
		}
	case hostJobTeardown:
		_, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostDeleting},
			To: domain.HostDeleted, Where: "job_id = {1}", Args: []any{r.JobId},
			Set: map[string]any{"job_id": "", "job_kind": "", "job_runner": "", "job_started_at": nil,
				"ended_at": store.SQL("now()"), "detail": ""},
			ResolveAll: true, Event: "локальное окружение снято", Action: "deleted"})
		return err
	}
	return nil
}

// hostJobFailed: неудачная попытка задания: повтор через минуту, после
// третьей: эскалация и ожидание повтора от человека.
func (e *Engine) hostJobFailed(ctx context.Context, h domain.EnvHost, output string) {
	attempts := h.Attempts + 1
	set := map[string]any{"job_id": "", "job_kind": "", "job_runner": "", "job_started_at": nil,
		"attempts": attempts, "detail": cleanDetail(output)}
	c := store.HostChange{From: []string{h.State}, Where: "job_id = {1}", Args: []any{h.JobID}, Set: set}
	what := "первичная настройка хоста"
	failure := domain.HostFailBootstrap
	if h.JobKind == hostJobTeardown {
		what, failure = "снятие локального окружения", domain.HostFailTeardown
	}
	if attempts >= hostJobAttempts {
		set["needs_retry"], set["failure"] = true, failure
		msg := fmt.Sprintf("%s не прошла %d раза подряд: %s. Исправьте причину и нажмите «Повторить»", what, attempts, lastLines(output, 15))
		c.Open = []store.HostAttention{{Reason: domain.AttHostFailed, Message: msg}}
		c.Event, c.Action = what+" не прошла: нужен повтор", "job_failed"
	} else {
		set["next_attempt_at"] = e.Now().Add(hostJobRetryAfter)
	}
	if _, _, err := e.St.ChangeHost(ctx, h.ID, c); err != nil {
		slog.Error("хост окружения: неудача задания", "host", h.ID, "err", err)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ─── стоимость ───────────────────────────────────────────────────────────

func (e *Engine) hostBilling(ctx context.Context, h domain.EnvHost, p infra.Provider) time.Time {
	now := e.Now()
	if !p.Paid() || h.ServerID == "" {
		return now.Add(hostFarFuture)
	}
	b, err := p.Billing(ctx, h.ServerID)
	if err != nil {
		if errors.Is(err, infra.ErrNotFound) {
			msg := "сервер " + h.ServerID + " не найден у провайдера: возможно, его удалили в панели RUVDS. Удалите хост окружения и запросите новый"
			if _, _, cerr := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostReady},
				Set: map[string]any{"detail": msg}, Open: []store.HostAttention{{Reason: domain.AttHostFailed, Message: msg}}}); cerr != nil {
				slog.Error("хост окружения: сервер пропал", "host", h.ID, "err", cerr)
			}
			return now.Add(hostBillingEvery)
		}
		return e.backoff(err).Add(30 * time.Minute)
	}
	set := map[string]any{"cost_checked_at": store.SQL("now()"), "paid_till": b.PaidTill}
	c := store.HostChange{From: []string{domain.HostReady}, Set: set}
	if b.Cost != nil {
		set["cost_amount"], set["cost_currency"], set["cost_period"] = b.Cost.Amount, b.Cost.Currency, b.Cost.Period
	}
	// Событие (и обновление консоли): только когда стоимость или дата
	// оплаты изменились: проверка раз в 6 часов не засоряет event log.
	if billingChanged(h, b) {
		text := "стоимость продления"
		if b.Cost != nil {
			text += " " + money(b.Cost.Amount, b.Cost.Currency) + " за " + ruvds.PeriodLabel(b.Cost.Period)
		}
		if b.PaidTill != nil {
			text += ", оплачено до " + b.PaidTill.UTC().Format("02.01.2006")
		}
		c.Event, c.Action = text, "billing"
	}
	if _, _, err := e.St.ChangeHost(ctx, h.ID, c); err != nil {
		slog.Error("хост окружения: стоимость", "host", h.ID, "err", err)
	}
	return now.Add(hostBillingEvery)
}

// billingChanged: стоимость или дата оплаты отличаются от записанных.
func billingChanged(h domain.EnvHost, b infra.Billing) bool {
	if (h.PaidTill == nil) != (b.PaidTill == nil) || (b.PaidTill != nil && !h.PaidTill.Equal(*b.PaidTill)) {
		return true
	}
	if b.Cost == nil {
		return false
	}
	return h.CostAmount == nil || *h.CostAmount != b.Cost.Amount || h.CostPeriod != b.Cost.Period
}

// ─── удаление ────────────────────────────────────────────────────────────

// hostDelete: подтверждённое удаление: у локального снятие заданием, у
// удалённого DELETE (идемпотентен у провайдера) и опрос до исчезновения.
func (e *Engine) hostDelete(ctx context.Context, h domain.EnvHost, p infra.Provider) time.Time {
	now := e.Now()
	if h.DeleteConfirmedBy == "" {
		slog.Error("хост окружения: deleting без подтверждения человека", "host", h.ID)
		return now.Add(hostFarFuture)
	}
	if !p.Remote() {
		if p.TeardownScript(infraSpec(h.Spec), h.RemoveVolumes) == "" {
			e.finishDelete(ctx, h, "хост снят")
			return now
		}
		return e.hostJob(ctx, h, p, hostJobTeardown)
	}
	if h.ServerID == "" {
		e.finishDelete(ctx, h, "сервера у провайдера нет: хост снят")
		return now
	}
	if h.ActionID == "" || h.DeleteSentAt == nil {
		// Отметка до запроса: удаление уходит только из deleting с
		// подтверждением (CAS и CHECK таблицы).
		_, ok, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostDeleting},
			Where: "delete_confirmed_by <> ''", Set: map[string]any{"delete_sent_at": store.SQL("COALESCE(delete_sent_at, now())")}})
		if err != nil || !ok {
			return now.Add(hostRetryInterval)
		}
		srv, err := p.Delete(ctx, h.ServerID)
		if err != nil {
			return e.deleteError(ctx, h, err)
		}
		if srv.State == infra.ServerDeleted {
			e.finishDelete(ctx, h, "сервер "+h.ServerID+" удалён")
			return now
		}
		if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostDeleting},
			Set:   map[string]any{"action_id": srv.ActionID, "detail": ""},
			Event: "удаление сервера " + h.ServerID + " отправлено", Action: "delete_sent"}); err != nil {
			slog.Error("хост окружения: удаление отправлено", "host", h.ID, "err", err)
		}
		return now.Add(hostPollInterval)
	}
	srv, err := p.Poll(ctx, infra.Server{ID: h.ServerID, ActionID: h.ActionID})
	if err != nil {
		e.hostNote(ctx, h, "опрос удаления: "+err.Error())
		return e.backoff(err)
	}
	switch srv.State {
	case infra.ServerDeleted:
		e.finishDelete(ctx, h, "сервер "+h.ServerID+" удалён")
		return now
	case infra.ServerFailed:
		return e.deleteError(ctx, h, &infra.Error{Kind: infra.ErrRejected, Message: srv.Detail})
	}
	return now.Add(hostPollInterval)
}

// deleteError: удаление не выполнено: отказ провайдера ждёт повтора от
// человека (повтор снова отправит удаление), сбой: повтор тиком.
func (e *Engine) deleteError(ctx context.Context, h domain.EnvHost, err error) time.Time {
	if !infra.Definitive(err) || errors.Is(err, infra.ErrRateLimited) || errors.Is(err, infra.ErrNotSent) {
		e.hostNote(ctx, h, "удаление: "+err.Error())
		return e.backoff(err)
	}
	msg := "удаление сервера " + h.ServerID + " не выполнено: " + err.Error() +
		". Проверьте права токена (нужно remove) и нажмите «Повторить»"
	if _, _, cerr := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostDeleting},
		Set:   map[string]any{"needs_retry": true, "action_id": "", "failure": domain.HostFailDelete, "detail": cleanDetail(msg)},
		Open:  []store.HostAttention{{Reason: domain.AttHostFailed, Message: cleanDetail(msg)}},
		Event: "удаление сервера не выполнено", Action: "delete_failed"}); cerr != nil {
		slog.Error("хост окружения: удаление не выполнено", "host", h.ID, "err", cerr)
	}
	return e.Now()
}

func (e *Engine) finishDelete(ctx context.Context, h domain.EnvHost, text string) {
	if _, _, err := e.St.ChangeHost(ctx, h.ID, store.HostChange{From: []string{domain.HostDeleting},
		To: domain.HostDeleted, Set: map[string]any{"ended_at": store.SQL("now()"), "detail": "", "needs_retry": false},
		ResolveAll: true, Event: text, Action: "deleted"}); err != nil {
		slog.Error("хост окружения: удалён", "host", h.ID, "err", err)
	}
}
