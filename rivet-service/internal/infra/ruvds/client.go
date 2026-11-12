// Package ruvds: клиент RUVDS API v2 и провайдер инфраструктуры `ruvds`
// (add-environment-provisioning). Ответы RUVDS: недоверенные данные:
// размер ограничен, типы строгие, значения проверяются (design, решение 14).
package ruvds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/infra"
)

// maxBody: предел тела ответа RUVDS.
const maxBody = 1 << 20

// maxErrText: сколько текста ошибки провайдера попадает в причину.
const maxErrText = 300

// Client: HTTP-клиент RUVDS API v2.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New: клиент с адресом API и токеном; hc nil: таймаут 30 секунд.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: hc}
}

// apiError: объект ошибки RUVDS (`api_error`).
type apiError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// ActionError: ошибка действия (`action_error`).
type ActionError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Action: асинхронное действие (`api_action`).
type Action struct {
	ID           int64        `json:"id"`
	Type         string       `json:"type"`
	Status       string       `json:"status"`
	Progress     int          `json:"progress"`
	ResourceID   int64        `json:"resource_id"`
	ResourceType string       `json:"resource_type"`
	Error        *ActionError `json:"action_error"`
}

// NetworkV4: IPv4-адрес сервера.
type NetworkV4 struct {
	IPAddress string `json:"ip_address"`
}

// ServerInfo: сервер (`server`). Пароль и прочие поля ответа в модель не
// читаются.
type ServerInfo struct {
	ID             int64       `json:"virtual_server_id"`
	Status         string      `json:"status"`
	CreateProgress int         `json:"create_progress"`
	Datacenter     int         `json:"datacenter"`
	TariffID       int         `json:"tariff_id"`
	PaymentPeriod  int         `json:"payment_period"`
	UserComment    string      `json:"user_comment"`
	PaidTill       *time.Time  `json:"paid_till"`
	NetworkV4      []NetworkV4 `json:"network_v4"`
}

// Pagination: страница списка.
type Pagination struct {
	Page     int  `json:"page"`
	NextPage *int `json:"next_page"`
	LastPage int  `json:"last_page"`
}

// createBody: тело POST /v2/servers (`server_create`).
type createBody struct {
	Datacenter    int     `json:"datacenter"`
	TariffID      int     `json:"tariff_id"`
	OSID          *int    `json:"os_id,omitempty"`
	TemplateID    *string `json:"template_id,omitempty"`
	PaymentPeriod int     `json:"payment_period"`
	CPU           int     `json:"cpu"`
	RAM           float64 `json:"ram"`
	Drive         int     `json:"drive"`
	DriveTariffID int     `json:"drive_tariff_id"`
	IP            int     `json:"ip"`
	SSHKeyID      *string `json:"ssh_key_id,omitempty"`
	ComputerName  string  `json:"computer_name,omitempty"`
	UserComment   *string `json:"user_comment,omitempty"`
}

func bodyFor(s infra.Spec) createBody {
	b := createBody{
		Datacenter: s.Datacenter, TariffID: s.TariffID, PaymentPeriod: s.PaymentPeriod,
		CPU: s.CPU, RAM: s.RAM, Drive: s.Drive, DriveTariffID: s.DriveTariffID, IP: 1,
	}
	if s.TemplateID != "" {
		t := s.TemplateID
		b.TemplateID = &t
	} else {
		id := s.OSID
		b.OSID = &id
	}
	return b
}

// CreateResponse: ответ на создание (`server_create_response`) без пароля:
// поле password RUVDS присылает, а Rivet его не читает (design, решение 3).
type CreateResponse struct {
	ID            int64    `json:"virtual_server_id"`
	PaymentPeriod int      `json:"payment_period"`
	CostRUB       *float64 `json:"cost_rub"`
	Action        *Action  `json:"action"`
}

// do: запрос к API: JSON-тело, Bearer, предел ответа, типизированные
// ошибки. out nil: тело ответа не читается.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return &infra.Error{Kind: infra.ErrNotSent, Message: "соединение с провайдером не установлено"}
		}
		// Ошибку запроса не показываем целиком: в ней полный URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("запрос к провайдеру: %w", ue.Err)
		}
		return fmt.Errorf("запрос к провайдеру: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("ответ провайдера: %w", err)
	}
	if len(data) > maxBody {
		return &infra.Error{Kind: infra.ErrBadResponse, Status: resp.StatusCode, Message: "ответ больше 1 МиБ"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &infra.Error{Kind: infra.ErrBadResponse, Status: resp.StatusCode, Message: "невалидный JSON"}
	}
	return nil
}

// statusError: отказ RUVDS по коду ответа.
func statusError(resp *http.Response, data []byte) error {
	var ae apiError
	_ = json.Unmarshal(data, &ae)
	e := &infra.Error{Status: resp.StatusCode, Code: clip(ae.ID), Message: clip(ae.Message)}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Kind = infra.ErrUnauthorized
	case http.StatusForbidden:
		e.Kind = infra.ErrForbidden
	case http.StatusTooManyRequests:
		e.Kind = infra.ErrRateLimited
		e.RetryAfter = retryAfter(resp.Header)
	case http.StatusNotFound:
		e.Kind = infra.ErrNotFound
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		e.Kind = infra.ErrRejected
		if mentionsBalance(ae.ID + " " + ae.Message) {
			e.Kind = infra.ErrInsufficientBalance
		}
	default:
		// 5xx и прочее: исход неизвестен.
		e.Kind = infra.ErrBadResponse
	}
	return e
}

// mentionsBalance: отказ из-за денег: RUVDS называет его кодом
// insufficient_balance или текстом про баланс.
func mentionsBalance(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "insufficient_balance") || strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "недостаточно средств")
}

// retryAfter: пауза из Retry-After или ratelimit-reset, не больше часа.
func retryAfter(h http.Header) time.Duration {
	for _, k := range []string{"Retry-After", "Ratelimit-Reset"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				return min(time.Duration(n)*time.Second, time.Hour)
			}
		}
	}
	return time.Minute
}

// clip: текст провайдера для причины: валидный UTF-8, без управляющих
// символов, не длиннее maxErrText рун.
func clip(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxErrText {
		r := []rune(s)
		s = string(r[:maxErrText]) + "…"
	}
	return s
}

// ─── методы API ──────────────────────────────────────────────────────────

var currencies = map[int]string{1: "RUB", 3: "USD", 4: "EUR"}

// Balance: денежный баланс аккаунта (GET /v2/balance).
func (c *Client) Balance(ctx context.Context) (infra.Money, error) {
	var out struct {
		Amount   *float64 `json:"amount"`
		Currency int      `json:"currency"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/balance", url.Values{"type": {"default"}}, nil, &out); err != nil {
		return infra.Money{}, err
	}
	if out.Amount == nil || !finite(*out.Amount) {
		return infra.Money{}, &infra.Error{Kind: infra.ErrBadResponse, Message: "баланс без суммы"}
	}
	cur, ok := currencies[out.Currency]
	if !ok {
		cur = "RUB"
	}
	return infra.Money{Amount: *out.Amount, Currency: cur}, nil
}

// Price: цена конфигурации без создания (POST /v2/servers?get_price_only=true).
func (c *Client) Price(ctx context.Context, s infra.Spec) (infra.Quote, error) {
	var out CreateResponse
	q := url.Values{"get_price_only": {"true"}}
	if err := c.do(ctx, http.MethodPost, "/v2/servers", q, bodyFor(s), &out); err != nil {
		return infra.Quote{}, err
	}
	if out.CostRUB == nil || !finite(*out.CostRUB) || *out.CostRUB < 0 {
		return infra.Quote{}, &infra.Error{Kind: infra.ErrBadResponse, Message: "цена без суммы"}
	}
	period := out.PaymentPeriod
	if period == 0 {
		period = s.PaymentPeriod
	}
	return infra.Quote{Money: infra.Money{Amount: *out.CostRUB, Currency: "RUB"}, Period: period}, nil
}

// Create: создание сервера (POST /v2/servers). Пароль из ответа не
// читается.
func (c *Client) Create(ctx context.Context, s infra.Spec, l infra.Labels, sshKeyID string) (CreateResponse, error) {
	b := bodyFor(s)
	if sshKeyID != "" {
		b.SSHKeyID = &sshKeyID
	}
	b.ComputerName = l.ComputerName
	if l.Comment != "" {
		cm := l.Comment
		b.UserComment = &cm
	}
	var out CreateResponse
	if err := c.do(ctx, http.MethodPost, "/v2/servers", nil, b, &out); err != nil {
		return out, err
	}
	if out.ID <= 0 {
		return out, &infra.Error{Kind: infra.ErrBadResponse, Message: "ответ на создание без идентификатора сервера"}
	}
	return out, nil
}

// SSHKey: ключ SSH аккаунта.
type SSHKey struct {
	ID        string `json:"ssh_key_id"`
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
}

// EnsureSSHKey: идентификатор публичного ключа в аккаунте: ищет по типу и
// телу ключа, при отсутствии добавляет.
func (c *Client) EnsureSSHKey(ctx context.Context, pub, name string) (string, error) {
	var list struct {
		Keys []SSHKey `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/ssh_keys", nil, nil, &list); err != nil {
		return "", err
	}
	want := keyBody(pub)
	for _, k := range list.Keys {
		if want != "" && keyBody(k.PublicKey) == want && validKeyID(k.ID) {
			return k.ID, nil
		}
	}
	var created SSHKey
	if err := c.do(ctx, http.MethodPost, "/v2/ssh_keys", nil,
		map[string]string{"public_key": strings.TrimSpace(pub), "name": name}, &created); err != nil {
		return "", err
	}
	if !validKeyID(created.ID) {
		return "", &infra.Error{Kind: infra.ErrBadResponse, Message: "ключ SSH без идентификатора"}
	}
	return created.ID, nil
}

// keyBody: тип и тело публичного ключа без комментария.
func keyBody(pub string) string {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return ""
	}
	return f[0] + " " + f[1]
}

func validKeyID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// FindServers: серверы, у которых в комментарии есть метка (поиск RUVDS
// грубый, поэтому метка сверяется ещё раз по ответу).
func (c *Client) FindServers(ctx context.Context, marker string) ([]ServerInfo, error) {
	var found []ServerInfo
	for page := 1; page <= 10; page++ {
		var out struct {
			Servers    []ServerInfo `json:"servers"`
			Pagination Pagination   `json:"pagination"`
		}
		q := url.Values{"search": {marker}, "get_network": {"true"}, "per_page": {"100"},
			"page": {strconv.Itoa(page)}}
		if err := c.do(ctx, http.MethodGet, "/v2/servers", q, nil, &out); err != nil {
			return nil, err
		}
		for _, s := range out.Servers {
			if s.ID > 0 && strings.Contains(s.UserComment, marker) {
				found = append(found, s)
			}
		}
		if out.Pagination.NextPage == nil || *out.Pagination.NextPage <= page {
			break
		}
	}
	return found, nil
}

// Action: действие по идентификатору.
func (c *Client) Action(ctx context.Context, id string) (Action, error) {
	var a Action
	if !numericID(id) {
		return a, &infra.Error{Kind: infra.ErrNotFound, Message: "неверный идентификатор действия"}
	}
	err := c.do(ctx, http.MethodGet, "/v2/actions/"+id, nil, nil, &a)
	return a, err
}

// Server: сервер по идентификатору.
func (c *Client) Server(ctx context.Context, id string) (ServerInfo, error) {
	var s ServerInfo
	if !numericID(id) {
		return s, &infra.Error{Kind: infra.ErrNotFound, Message: "неверный идентификатор сервера"}
	}
	err := c.do(ctx, http.MethodGet, "/v2/servers/"+id, nil, nil, &s)
	return s, err
}

// Networks: IPv4-адреса сервера.
func (c *Client) Networks(ctx context.Context, id string) ([]NetworkV4, error) {
	var out struct {
		V4 []NetworkV4 `json:"v4"`
	}
	if !numericID(id) {
		return nil, &infra.Error{Kind: infra.ErrNotFound, Message: "неверный идентификатор сервера"}
	}
	err := c.do(ctx, http.MethodGet, "/v2/servers/"+id+"/networks", nil, nil, &out)
	return out.V4, err
}

// Delete: удаление сервера (DELETE /v2/servers/{id}).
func (c *Client) Delete(ctx context.Context, id string) (Action, error) {
	var a Action
	if !numericID(id) {
		return a, &infra.Error{Kind: infra.ErrNotFound, Message: "неверный идентификатор сервера"}
	}
	err := c.do(ctx, http.MethodDelete, "/v2/servers/"+id, nil, nil, &a)
	return a, err
}

// Cost: стоимость продления сервера.
func (c *Client) Cost(ctx context.Context, id string) (infra.Quote, error) {
	var out struct {
		CostRUB       *float64 `json:"cost_rub"`
		PaymentPeriod int      `json:"payment_period"`
	}
	if !numericID(id) {
		return infra.Quote{}, &infra.Error{Kind: infra.ErrNotFound, Message: "неверный идентификатор сервера"}
	}
	if err := c.do(ctx, http.MethodGet, "/v2/servers/"+id+"/cost", nil, nil, &out); err != nil {
		return infra.Quote{}, err
	}
	if out.CostRUB == nil || !finite(*out.CostRUB) || *out.CostRUB < 0 {
		return infra.Quote{}, &infra.Error{Kind: infra.ErrBadResponse, Message: "стоимость без суммы"}
	}
	return infra.Quote{Money: infra.Money{Amount: *out.CostRUB, Currency: "RUB"}, Period: out.PaymentPeriod}, nil
}

// PaidTill: дата, до которой сервер оплачен.
func (c *Client) PaidTill(ctx context.Context, id string) (*time.Time, error) {
	var out struct {
		PaidTill *time.Time `json:"paid_till"`
	}
	if !numericID(id) {
		return nil, &infra.Error{Kind: infra.ErrNotFound, Message: "неверный идентификатор сервера"}
	}
	if err := c.do(ctx, http.MethodGet, "/v2/servers/"+id+"/paid_till", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.PaidTill, nil
}

// ─── справочники ─────────────────────────────────────────────────────────

// Datacenter: дата-центр и доступные в нём тарифы.
type Datacenter struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Tariffs      []int  `json:"tariffs"`
	DriveTariffs []int  `json:"drive_tariffs"`
}

// Tariff: тариф виртуального сервера.
type Tariff struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	CPUPrice float64 `json:"cpu_price"`
	RAMPrice float64 `json:"ram_price"`
	Active   bool    `json:"active"`
}

// DriveTariff: тариф диска.
type DriveTariff struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Price  float64 `json:"price"`
	Active bool    `json:"active"`
}

// OS: операционная система для создания сервера.
type OS struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Active  bool   `json:"active"`
	SSHKeys bool   `json:"ssh_keys"`
}

// Period: период оплаты.
type Period struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// Catalog: справочники для формы окружения.
type Catalog struct {
	Datacenters    []Datacenter  `json:"datacenters"`
	Tariffs        []Tariff      `json:"tariffs"`
	DriveTariffs   []DriveTariff `json:"drive_tariffs"`
	OS             []OS          `json:"os"`
	PaymentPeriods []Period      `json:"payment_periods"`
}

// PeriodLabels: периоды оплаты RUVDS, которые предлагает форма (тестовый
// период и «не задан» в ней не участвуют).
var PeriodLabels = map[int]string{2: "1 месяц", 3: "3 месяца", 4: "6 месяцев", 5: "1 год"}

// PeriodLabel: подпись периода оплаты.
func PeriodLabel(p int) string {
	if l, ok := PeriodLabels[p]; ok {
		return l
	}
	if p == 1 {
		return "тестовый период"
	}
	return ""
}

// Catalog: дата-центры, тарифы и ОС аккаунта. В список ОС входят только
// Linux с поддержкой SSH-ключей: bootstrap идёт по SSH.
func (c *Client) Catalog(ctx context.Context) (Catalog, error) {
	var dcs struct {
		Datacenters []struct {
			ID           int    `json:"id"`
			Name         string `json:"name"`
			VPSTariffs   []int  `json:"vps_tariffs"`
			DriveTariffs []int  `json:"drive_tariffs"`
		} `json:"datacenters"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/datacenters", nil, nil, &dcs); err != nil {
		return Catalog{}, err
	}
	var tf struct {
		VPS []struct {
			ID       int     `json:"id"`
			Name     string  `json:"name"`
			CPU      float64 `json:"cpu"`
			RAM      float64 `json:"ram"`
			IsActive bool    `json:"is_active"`
		} `json:"vps"`
		Drive []struct {
			ID       int     `json:"id"`
			Name     string  `json:"name"`
			Price    float64 `json:"price"`
			IsActive bool    `json:"is_active"`
		} `json:"drive"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/tariffs", nil, nil, &tf); err != nil {
		return Catalog{}, err
	}
	var oss struct {
		OS []struct {
			ID               int    `json:"id"`
			Name             string `json:"name"`
			IsActive         bool   `json:"is_active"`
			Type             string `json:"type"`
			SSHKeysSupported bool   `json:"ssh_keys_supported"`
		} `json:"os"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/os", nil, nil, &oss); err != nil {
		return Catalog{}, err
	}
	out := Catalog{Datacenters: []Datacenter{}, Tariffs: []Tariff{}, DriveTariffs: []DriveTariff{}, OS: []OS{}}
	for _, d := range dcs.Datacenters {
		if d.ID <= 0 {
			continue
		}
		out.Datacenters = append(out.Datacenters, Datacenter{ID: d.ID, Name: clip(d.Name),
			Tariffs: positive(d.VPSTariffs), DriveTariffs: positive(d.DriveTariffs)})
	}
	for _, t := range tf.VPS {
		if t.ID <= 0 {
			continue
		}
		out.Tariffs = append(out.Tariffs, Tariff{ID: t.ID, Name: clip(t.Name),
			CPUPrice: finiteOr0(t.CPU), RAMPrice: finiteOr0(t.RAM), Active: t.IsActive})
	}
	for _, t := range tf.Drive {
		if t.ID <= 0 {
			continue
		}
		out.DriveTariffs = append(out.DriveTariffs, DriveTariff{ID: t.ID, Name: clip(t.Name),
			Price: finiteOr0(t.Price), Active: t.IsActive})
	}
	for _, o := range oss.OS {
		if o.ID <= 0 || o.Type != "linux" || !o.SSHKeysSupported {
			continue
		}
		out.OS = append(out.OS, OS{ID: o.ID, Name: clip(o.Name), Type: o.Type, Active: o.IsActive, SSHKeys: true})
	}
	for _, p := range []int{2, 3, 4, 5} {
		out.PaymentPeriods = append(out.PaymentPeriods, Period{ID: p, Label: PeriodLabels[p]})
	}
	return out, nil
}

// ─── проверки значений ───────────────────────────────────────────────────

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func finiteOr0(f float64) float64 {
	if !finite(f) || f < 0 {
		return 0
	}
	return f
}

func positive(in []int) []int {
	out := []int{}
	for _, v := range in {
		if v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// numericID: идентификатор RUVDS: положительное целое (уезжает в путь
// запроса, поэтому проверяется до подстановки).
func numericID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

// ipv4: первый публичный IPv4 из списка адресов: адрес уезжает в host
// окружения и в аргумент ssh, поэтому проверяется разбором.
func ipv4(nets []NetworkV4) string {
	for _, n := range nets {
		a, err := netip.ParseAddr(strings.TrimSpace(n.IPAddress))
		if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() {
			continue
		}
		return a.String()
	}
	return ""
}
