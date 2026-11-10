package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Хост окружения от провайдера инфраструктуры (add-environment-provisioning,
// спека deployment «Хост окружения от провайдера инфраструктуры»).

// Провайдеры хоста окружения.
const (
	ProvisionLocal = "local"
	ProvisionRUVDS = "ruvds"
)

// ProvisionConfig: привязка окружения «Linux-хост» к провайдеру
// инфраструктуры. Хост и его ключ при привязке задаёт Rivet.
type ProvisionConfig struct {
	Provider string `json:"provider"`
	// Connection: подключение к провайдеру (у local пусто).
	Connection    string  `json:"connection,omitempty"`
	Datacenter    int     `json:"datacenter,omitempty"`
	TariffID      int     `json:"tariff_id,omitempty"`
	OSID          int     `json:"os_id,omitempty"`
	TemplateID    string  `json:"template_id,omitempty"`
	PaymentPeriod int     `json:"payment_period,omitempty"`
	CPU           int     `json:"cpu,omitempty"`
	RAM           float64 `json:"ram,omitempty"`
	Drive         int     `json:"drive,omitempty"`
	DriveTariffID int     `json:"drive_tariff_id,omitempty"`
	// SSHPublicKey: публичный ключ deploy-runner'а: приватный остаётся на
	// его машине (design, решение 3).
	SSHPublicKey string `json:"ssh_public_key,omitempty"`
	// BootstrapCmd: свой скрипт первичной настройки; пусто: скрипт провайдера.
	BootstrapCmd string `json:"bootstrap_cmd,omitempty"`
	// Port: порт хоста локального окружения (RIVET_HOST_PORT); 0: назначит Rivet.
	Port int `json:"port,omitempty"`
}

// Пределы порта локального хоста: назначаемый диапазон и допустимые значения.
const (
	LocalPortFirst = 20000
	LocalPortLast  = 29999
	localPortMin   = 1024
	localPortMax   = 65535
)

// sshPubKeyRe: публичный ключ OpenSSH в одну строку.
var sshPubKeyRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/]+={0,3}( [^\x00-\x1f\x7f]{0,200})?$`)

// templateIDRe: идентификатор шаблона RUVDS (uuid).
var templateIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

// connIDRe: идентификатор подключения к провайдеру инфраструктуры.
var connIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidInfraConnID: допустим ли идентификатор подключения.
func ValidInfraConnID(id string) bool { return connIDRe.MatchString(id) }

// RUVDSPaymentPeriods: периоды оплаты RUVDS, доступные привязке.
var RUVDSPaymentPeriods = map[int]bool{2: true, 3: true, 4: true, 5: true}

// Validate: привязка окружения к провайдеру; ошибки с полем формы.
func (p ProvisionConfig) Validate() error {
	field := func(f, msg string) error { return &EnvFieldError{Field: "config.provision." + f, Msg: msg} }
	if strings.ContainsAny(p.BootstrapCmd, "\x00") {
		return field("bootstrap_cmd", "недопустимый символ в скрипте")
	}
	if len(p.BootstrapCmd) > 16<<10 {
		return field("bootstrap_cmd", "скрипт длиннее 16 КиБ")
	}
	switch p.Provider {
	case ProvisionLocal:
		if p.Connection != "" || p.Datacenter != 0 || p.TariffID != 0 || p.SSHPublicKey != "" {
			return field("provider", "у локального хоста нет подключения, тарифа и SSH-ключа")
		}
		if p.Port != 0 && (p.Port < localPortMin || p.Port > localPortMax) {
			return field("port", fmt.Sprintf("порт от %d до %d или 0", localPortMin, localPortMax))
		}
		return nil
	case ProvisionRUVDS:
	default:
		return field("provider", "ожидается local или ruvds")
	}
	if !connIDRe.MatchString(p.Connection) {
		return field("connection", "выберите подключение к RUVDS")
	}
	if p.Port != 0 {
		return field("port", "порт назначается только локальному хосту")
	}
	switch {
	case p.Datacenter <= 0:
		return field("datacenter", "выберите дата-центр")
	case p.TariffID <= 0:
		return field("tariff_id", "выберите тариф")
	case (p.OSID > 0) == (p.TemplateID != ""):
		return field("os_id", "нужна операционная система или шаблон, одно из двух")
	case p.TemplateID != "" && !templateIDRe.MatchString(p.TemplateID):
		return field("template_id", "ожидается идентификатор шаблона")
	case !RUVDSPaymentPeriods[p.PaymentPeriod]:
		return field("payment_period", "период оплаты: 1, 3, 6 месяцев или год")
	case p.CPU < 1 || p.CPU > 64:
		return field("cpu", "процессоров от 1 до 64")
	case p.RAM <= 0 || p.RAM > 512:
		return field("ram", "память от 0.5 до 512 ГБ")
	case p.Drive < 10 || p.Drive > 4096:
		return field("drive", "диск от 10 до 4096 ГБ")
	case p.DriveTariffID <= 0:
		return field("drive_tariff_id", "выберите тариф диска")
	case !sshPubKeyRe.MatchString(strings.TrimSpace(p.SSHPublicKey)):
		return field("ssh_public_key", "нужен публичный SSH-ключ deploy-runner'а в формате OpenSSH (ssh-ed25519 AAAA…)")
	}
	return nil
}

// SameServer: у двух привязок один и тот же сервер: провайдер,
// подключение и ресурсы. Команды и скрипт bootstrap сюда не входят: их
// можно менять при живом хосте.
func (p ProvisionConfig) SameServer(o ProvisionConfig) bool {
	a, b := p, o
	a.BootstrapCmd, b.BootstrapCmd = "", ""
	a.SSHPublicKey, b.SSHPublicKey = strings.TrimSpace(a.SSHPublicKey), strings.TrimSpace(b.SSHPublicKey)
	return a == b
}

// hostKeyLineRe: строка known_hosts: адрес, тип и ключ.
var hostKeyLineRe = regexp.MustCompile(`^[A-Za-z0-9.:\[\],_-]+ (ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/]+={0,3}$`)

// ValidHostKey: закреплённый ключ хоста: строки known_hosts без
// комментариев и маркеров (@cert-authority, @revoked).
func ValidHostKey(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 16<<10 {
		return errors.New("host_key: длиннее 16 КиБ")
	}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if !hostKeyLineRe.MatchString(strings.TrimSpace(line)) {
			return errors.New("host_key: ожидаются строки known_hosts «адрес тип ключ»")
		}
	}
	return nil
}

// Состояния хоста окружения (design, решение 1).
const (
	HostQuoting          = "quoting"
	HostAwaitingApproval = "awaiting_approval"
	HostApproved         = "approved"
	HostCreating         = "creating"
	HostBootstrapping    = "bootstrapping"
	HostReady            = "ready"
	HostDeleteRequested  = "delete_requested"
	HostDeleting         = "deleting"
	HostDeleted          = "deleted"
	HostRejected         = "rejected"
	HostFailed           = "failed"
)

// HostTerminal: конечное состояние хоста.
func HostTerminal(state string) bool {
	return state == HostDeleted || state == HostRejected || state == HostFailed
}

// Коды провала хоста.
const (
	HostFailInsufficientBalance = "insufficient_balance"
	HostFailRejected            = "provider_rejected"
	HostFailUnauthorized        = "unauthorized"
	HostFailUnknownOutcome      = "unknown_outcome"
	HostFailTimeout             = "timeout"
	HostFailBootstrap           = "bootstrap"
	HostFailTeardown            = "teardown"
	HostFailDelete              = "delete"
)

// EnvHost: хост окружения.
type EnvHost struct {
	ID           string
	EnvID        string
	ProjectID    string
	Provider     string
	ConnectionID string
	Spec         ProvisionConfig
	// SpecLabels: названия из справочника провайдера: datacenter, tariff,
	// os, drive_tariff.
	SpecLabels  map[string]string
	State       string
	PrevState   string
	RequestedBy string

	QuoteAmount     *float64
	QuoteCurrency   string
	QuotePeriod     int
	BalanceAmount   *float64
	BalanceCurrency string

	ApprovedBy     string
	ApprovedAt     *time.Time
	ApprovedAmount *float64
	CreateSentAt   *time.Time

	ServerID       string
	ActionID       string
	Address        string
	HostKey        string
	WaitingPayment bool
	Port           int

	JobID        string
	JobKind      string
	JobRunner    string
	JobStartedAt *time.Time
	Attempts     int
	NeedsRetry   bool

	RemoveVolumes     bool
	DeleteRequestedBy string
	DeleteConfirmedBy string
	DeleteSentAt      *time.Time

	PaidTill      *time.Time
	CostAmount    *float64
	CostCurrency  string
	CostPeriod    int
	CostCheckedAt *time.Time

	Failure       string
	Detail        string
	NextAttemptAt time.Time
	Created       time.Time
	Updated       time.Time
	Ended         *time.Time
}

// InfraConnection: подключение к провайдеру инфраструктуры.
type InfraConnection struct {
	ID              string
	Provider        string
	Name            string
	TokenPrefix     string
	HasToken        bool
	Enabled         bool
	State           LLMProviderState
	CheckDetail     string
	CheckedAt       *time.Time
	BalanceAmount   *float64
	BalanceCurrency string
	UpdatedAt       time.Time
	UpdatedBy       string
}

// ComposeProject: имя compose-проекта окружения на машине runner'а:
// идентификаторы проекта и окружения, чтобы окружения разных проектов на
// одной машине не пересекались (design, решение 10).
func ComposeProject(projectID, envID string) string {
	short := func(id string) string {
		id = strings.ReplaceAll(strings.ToLower(id), "-", "")
		if len(id) > 8 {
			id = id[:8]
		}
		return id
	}
	return "rivet-" + short(projectID) + "-" + short(envID)
}

// HostEnvMarker и HostMarker: метки сервера в комментарии у провайдера.
func HostEnvMarker(envID string) string { return "rivet-env-" + envID }
func HostMarker(hostID string) string   { return "rivet-host-" + hostID }

// HostComputerName: имя сервера у провайдера.
func HostComputerName(envID string) string {
	id := strings.ReplaceAll(envID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return "rivet-" + id
}
