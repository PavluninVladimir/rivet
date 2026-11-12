// Package infra: провайдеры инфраструктуры: откуда у окружения хост
// (add-environment-provisioning, спека deployment «Хост окружения от
// провайдера инфраструктуры»). Провайдер не знает про окружения и базу:
// жизненный цикл хоста, решения человека и повторы ведёт оркестратор.
package infra

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Виды провайдеров.
const (
	// KindLocal: хост это машина deploy-runner'а: docker compose без SSH
	// и без денег.
	KindLocal = "local"
	// KindRUVDS: виртуальный сервер у хостинга RUVDS (API v2).
	KindRUVDS = "ruvds"
)

// Spec: конфигурация хоста, которую провайдер создаёт. Поля сервера
// значимы только у удалённых провайдеров.
type Spec struct {
	Datacenter    int
	TariffID      int
	OSID          int
	TemplateID    string
	PaymentPeriod int
	CPU           int
	RAM           float64 // ГБ
	Drive         int     // ГБ
	DriveTariffID int
	SSHPublicKey  string
	// Bootstrap: свой скрипт первичной настройки; пусто: скрипт провайдера.
	Bootstrap string
}

// Money: сумма в валюте провайдера.
type Money struct {
	Amount   float64
	Currency string
}

// Quote: стоимость конфигурации за период оплаты.
type Quote struct {
	Money
	Period int
}

// Labels: метки сервера: по ним Rivet находит свой сервер после потери
// ответа и до расчёта цены (design, решение 7).
type Labels struct {
	ComputerName string
	// Comment пишется в комментарий сервера целиком; Find ищет по метке.
	Comment string
}

// ServerState: состояние сервера у провайдера в терминах Rivet.
type ServerState string

const (
	// ServerPending: создание или удаление идёт.
	ServerPending ServerState = "pending"
	// ServerWaitingPayment: сервер ждёт оплаты у провайдера.
	ServerWaitingPayment ServerState = "waiting_payment"
	// ServerBlocked: сервер заблокирован провайдером.
	ServerBlocked ServerState = "blocked"
	// ServerActive: сервер работает и имеет адрес (у удалённого).
	ServerActive ServerState = "active"
	// ServerFailed: действие провайдера завершилось ошибкой.
	ServerFailed ServerState = "failed"
	// ServerDeleted: сервера больше нет.
	ServerDeleted ServerState = "deleted"
)

// Server: сервер провайдера и его текущее асинхронное действие.
type Server struct {
	ID       string
	ActionID string
	State    ServerState
	Progress int
	// Address: IPv4 сервера; пусто у локального хоста и пока адреса нет.
	Address string
	// Comment: комментарий сервера (для сверки метки).
	Comment string
	// Failure: код ошибки действия (insufficient_balance и т. п.), Detail:
	// пояснение провайдера, уже обрезанное.
	Failure string
	Detail  string
}

// Billing: стоимость продления и дата, до которой сервер оплачен.
type Billing struct {
	Cost     *Quote
	PaidTill *time.Time
}

// Provider: провайдер инфраструктуры (design, решение 2).
type Provider interface {
	Kind() string
	// Remote: хост отдельная машина, доступная по SSH; иначе машина
	// deploy-runner'а.
	Remote() bool
	// Paid: создание тратит деньги: перед ним цена и решение человека.
	Paid() bool
	// Quote: цена конфигурации без создания сервера.
	Quote(ctx context.Context, s Spec) (Quote, error)
	// Balance: баланс аккаунта; nil: провайдер баланса не знает.
	Balance(ctx context.Context) (*Money, error)
	// Find: неудалённый сервер с меткой в комментарии; nil: такого нет.
	Find(ctx context.Context, marker string) (*Server, error)
	// Create: отправка создания. Вызывать только после решения человека
	// у платного провайдера.
	Create(ctx context.Context, s Spec, l Labels) (Server, error)
	// Poll: текущее состояние сервера и его действия.
	Poll(ctx context.Context, srv Server) (Server, error)
	// Delete: отправка удаления. Вызывать только после подтверждения.
	Delete(ctx context.Context, serverID string) (Server, error)
	// Billing: стоимость продления и дата оплаты.
	Billing(ctx context.Context, serverID string) (Billing, error)
	// BootstrapScript: первичная настройка хоста deploy-runner'ом.
	BootstrapScript(s Spec) string
	// TeardownScript: снятие окружения командами на хосте; пусто:
	// снимать нечего, удаление сервера уносит всё.
	TeardownScript(s Spec, removeVolumes bool) string
}

// Describer: провайдер со справочником: названия дата-центра, тарифа и
// ОС конфигурации для решения человека (ключи datacenter, tariff, os,
// drive_tariff). Необязательная возможность провайдера.
type Describer interface {
	Describe(ctx context.Context, s Spec) (map[string]string, error)
}

// Ошибки провайдера. Все, кроме неизвестного исхода, означают, что
// провайдер запрос не выполнил: сервер не создан и не удалён.
var (
	ErrUnauthorized        = errors.New("провайдер не принял токен")
	ErrForbidden           = errors.New("у токена нет прав на эту операцию")
	ErrRateLimited         = errors.New("превышен лимит запросов к провайдеру")
	ErrInsufficientBalance = errors.New("недостаточно средств на балансе провайдера")
	ErrRejected            = errors.New("провайдер отклонил запрос")
	ErrNotFound            = errors.New("объект у провайдера не найден")
	// ErrNotSent: запрос не ушёл: соединение не установлено.
	ErrNotSent = errors.New("запрос к провайдеру не отправлен")
	// ErrBadResponse: ответ провайдера не разобран: исход неизвестен.
	ErrBadResponse = errors.New("ответ провайдера не разобран")
)

// Error: отказ провайдера с причиной.
type Error struct {
	Kind       error
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := e.Kind.Error()
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Status != 0 {
		msg = fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Kind }

// Definitive: провайдер точно не выполнил запрос (сервер не создан):
// повтор после такой ошибки не создаст второй сервер.
func Definitive(err error) bool {
	for _, k := range []error{ErrUnauthorized, ErrForbidden, ErrRateLimited,
		ErrInsufficientBalance, ErrRejected, ErrNotSent} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}

// RetryAfter: сколько ждать после отказа по лимиту запросов.
func RetryAfter(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return 0
}

// Factory собирает провайдера по виду и токену подключения.
type Factory struct {
	// RUVDSBaseURL: адрес API RUVDS (RIVET_RUVDS_API_URL).
	RUVDSBaseURL string
	// HTTP: клиент запросов к провайдерам; nil: с таймаутом по умолчанию.
	HTTP *http.Client
	// New: подмена сборки провайдера в тестах; nil: настоящие провайдеры.
	New func(kind, token string) (Provider, error)
}

// DefaultRUVDSURL: адрес API RUVDS по умолчанию.
const DefaultRUVDSURL = "https://api.ruvds.com"

// ErrUnknownProvider: вид провайдера не поддерживается.
var ErrUnknownProvider = errors.New("неизвестный провайдер инфраструктуры")

// RemoteBuilder собирает удалённого провайдера (регистрирует пакет
// провайдера: infra не импортирует свои реализации).
type RemoteBuilder func(baseURL, token string, hc *http.Client) Provider

var builders = map[string]RemoteBuilder{}

// Register: регистрация удалённого провайдера при инициализации пакета.
func Register(kind string, b RemoteBuilder) { builders[kind] = b }

// Provider: провайдер вида kind; token нужен удалённым.
func (f *Factory) Provider(kind, token string) (Provider, error) {
	if f != nil && f.New != nil {
		return f.New(kind, token)
	}
	if kind == KindLocal {
		return Local{}, nil
	}
	b, ok := builders[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, kind)
	}
	base := DefaultRUVDSURL
	var hc *http.Client
	if f != nil {
		if f.RUVDSBaseURL != "" {
			base = f.RUVDSBaseURL
		}
		hc = f.HTTP
	}
	return b(base, token, hc), nil
}
