// Package ruvdstest: fake RUVDS API v2 для тестов и e2e-стенда
// (add-environment-provisioning, design решение 15). Только тестовая
// инфраструктура: продукт его не импортирует. Fake повторяет нужные
// эндпоинты по спецификации и считает запросы, чтобы тесты проверяли, что
// без решения человека создания и удаления нет.
package ruvdstest

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Call: запрос к fake.
type Call struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	PriceOnly bool   `json:"price_only"`
}

type server struct {
	ID            int64
	Status        string
	Progress      int
	Datacenter    int
	TariffID      int
	PaymentPeriod int
	Comment       string
	Name          string
	IP            string
	Cost          float64
	PaidTill      time.Time
	Polls         int // опросов до готовности
	Token         string
}

type action struct {
	ID         int64
	Type       string
	Status     string
	Progress   int
	ResourceID int64
	ErrID      string
	ErrMsg     string
	Polls      int
}

// Fake: состояние fake RUVDS.
type Fake struct {
	mu sync.Mutex
	// Balance: баланс по умолчанию; у токена со словом lowbalance: 10 ₽.
	Balance float64
	// PriceFactor: множитель цены (тест роста цены после решения).
	PriceFactor float64
	// ReadyAfter: сколько опросов действия создание идёт до успеха.
	ReadyAfter int
	// WaitPayment: создание встаёт в ожидание оплаты.
	WaitPayment bool
	// RateLimit: сколько следующих запросов ответят 429.
	RateLimit int
	// DropCreateResponse: оборвать соединение на POST создания после
	// записи сервера (потеря ответа).
	DropCreateResponse bool
	// HideFromSearch: поиск серверов ничего не находит (потерянный ответ
	// без следа у провайдера).
	HideFromSearch bool

	servers map[int64]*server
	actions map[int64]*action
	keys    []map[string]string
	calls   []Call
	nextID  int64
	balance map[string]float64
}

// New: fake с балансом 10 000 ₽ и созданием за два опроса.
func New() *Fake {
	return &Fake{Balance: 10000, PriceFactor: 1, ReadyAfter: 2,
		servers: map[int64]*server{}, actions: map[int64]*action{}, nextID: 6000,
		balance: map[string]float64{}}
}

// Start: httptest-сервер с fake.
func (f *Fake) Start() *httptest.Server { return httptest.NewServer(f) }

// Calls: копия журнала запросов.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Creates: число запросов на создание сервера (без get_price_only).
func (f *Fake) Creates() int { return f.count(http.MethodPost, "/v2/servers", false) }

// Deletes: число запросов на удаление сервера.
func (f *Fake) Deletes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/v2/servers/") {
			n++
		}
	}
	return n
}

// Quotes: число расчётов цены.
func (f *Fake) Quotes() int { return f.count(http.MethodPost, "/v2/servers", true) }

func (f *Fake) count(method, path string, priceOnly bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method && c.Path == path && c.PriceOnly == priceOnly {
			n++
		}
	}
	return n
}

// ServerCount: серверы, которые не удалены.
func (f *Fake) ServerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.servers {
		if s.Status != "deleted" {
			n++
		}
	}
	return n
}

// SetBalance: баланс по умолчанию.
func (f *Fake) SetBalance(v float64) {
	f.mu.Lock()
	f.Balance = v
	f.mu.Unlock()
}

// Pay: оплатить ожидающие серверы: создание продолжается.
func (f *Fake) Pay() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WaitPayment = false
	for _, a := range f.actions {
		if a.Status == "wait_user_action" {
			a.Status = "running"
		}
	}
	for _, s := range f.servers {
		if s.Status == "notpaid" {
			s.Status = "initializing"
		}
	}
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__fake/calls" {
		f.mu.Lock()
		out := map[string]any{"calls": f.calls}
		f.mu.Unlock()
		out["creates"], out["deletes"], out["quotes"], out["servers"] = f.Creates(), f.Deletes(), f.Quotes(), f.ServerCount()
		writeJSON(w, 200, out)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path,
		PriceOnly: r.URL.Query().Get("get_price_only") == "true"})
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || strings.Contains(token, "invalid") {
		apiErr(w, 401, "auth_error", "Please, provide bearer-type Authorization header containing your OAuth token.")
		return
	}
	if f.RateLimit > 0 {
		f.RateLimit--
		w.Header().Set("Retry-After", "1")
		apiErr(w, 429, "rate_limit_exceeded", "Too Many Requests.")
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/v2/balance":
		writeJSON(w, 200, map[string]any{"amount": f.balanceOf(token), "currency": 1, "type": "default"})
	case r.Method == http.MethodGet && p == "/v2/datacenters":
		writeJSON(w, 200, map[string]any{"datacenters": []map[string]any{
			{"id": 1, "name": "Fake DC: Москва", "vps_tariffs": []int{14, 15}, "drive_tariffs": []int{1, 3},
				"additional_drive_tariffs": []int{}, "additional_service_tariffs": []int{}},
			{"id": 2, "name": "Fake DC: Санкт-Петербург", "vps_tariffs": []int{14}, "drive_tariffs": []int{3},
				"additional_drive_tariffs": []int{}, "additional_service_tariffs": []int{}},
		}})
	case r.Method == http.MethodGet && p == "/v2/tariffs":
		writeJSON(w, 200, map[string]any{
			"vps": []map[string]any{
				{"id": 14, "name": "Regular", "cpu": 79, "ram": 195, "vram": 1.95, "ip": 100, "is_active": true},
				{"id": 15, "name": "Turbo", "cpu": 120, "ram": 260, "vram": 1.95, "ip": 100, "is_active": true},
			},
			"drive": []map[string]any{
				{"id": 1, "name": "HDD", "price": 5.5, "is_active": true},
				{"id": 3, "name": "SSD", "price": 12, "is_active": true},
			},
			"additional_drive": []any{}, "additional_service": []any{}, "payment_period_discount": []any{},
		})
	case r.Method == http.MethodGet && p == "/v2/os":
		writeJSON(w, 200, map[string]any{"os": []map[string]any{
			{"id": 52, "name": "Ubuntu 24.04", "is_active": true, "type": "linux", "ssh_keys_supported": true, "os_requirements": nil},
			{"id": 53, "name": "Debian 12", "is_active": true, "type": "linux", "ssh_keys_supported": true, "os_requirements": nil},
			{"id": 14, "name": "Windows Server 2022", "is_active": true, "type": "windows", "ssh_keys_supported": false,
				"os_requirements": map[string]any{"cpu": 2, "ram": 2, "drive": 40}},
		}})
	case r.Method == http.MethodGet && p == "/v2/ssh_keys":
		writeJSON(w, 200, map[string]any{"ssh_keys": f.keys})
	case r.Method == http.MethodPost && p == "/v2/ssh_keys":
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["public_key"] == "" || in["name"] == "" {
			apiErr(w, 400, "json_error", "Missing field 'public_key'.")
			return
		}
		k := map[string]string{"ssh_key_id": fmt.Sprintf("153e8ffa-6b69-47b5-933f-%012d", len(f.keys)+1),
			"public_key": in["public_key"], "name": in["name"],
			"md5_fingerprint": "MD5:00", "sha256_fingerprint": "SHA256:fake"}
		f.keys = append(f.keys, k)
		writeJSON(w, 200, k)
	case r.Method == http.MethodPost && p == "/v2/servers":
		f.createServer(w, r, token)
	case r.Method == http.MethodGet && p == "/v2/servers":
		f.listServers(w, r)
	case strings.HasPrefix(p, "/v2/servers/"):
		f.serverOp(w, r, strings.TrimPrefix(p, "/v2/servers/"))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/v2/actions/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/v2/actions/"), 10, 64)
		a, ok := f.actions[id]
		if !ok {
			apiErr(w, 404, "not_found", fmt.Sprintf("Action with id %d is not found.", id))
			return
		}
		f.advance(a)
		writeJSON(w, 200, actionJSON(a))
	default:
		apiErr(w, 404, "not_found", "Not found.")
	}
}

func (f *Fake) balanceOf(token string) float64 {
	if v, ok := f.balance[token]; ok {
		return v
	}
	if strings.Contains(token, "lowbalance") {
		return 10
	}
	return f.Balance
}

type createIn struct {
	Datacenter    int      `json:"datacenter"`
	TariffID      int      `json:"tariff_id"`
	OSID          *int     `json:"os_id"`
	TemplateID    *string  `json:"template_id"`
	PaymentPeriod int      `json:"payment_period"`
	CPU           int      `json:"cpu"`
	RAM           float64  `json:"ram"`
	Drive         int      `json:"drive"`
	DriveTariffID int      `json:"drive_tariff_id"`
	IP            int      `json:"ip"`
	SSHKeyID      *string  `json:"ssh_key_id"`
	ComputerName  string   `json:"computer_name"`
	UserComment   *string  `json:"user_comment"`
	DDoS          *float64 `json:"ddos_protection"`
}

var months = map[int]float64{1: 0, 2: 1, 3: 3, 4: 6, 5: 12}

func (f *Fake) price(in createIn) float64 {
	cpu, ram := 79.0, 195.0
	if in.TariffID == 15 {
		cpu, ram = 120, 260
	}
	drive := 5.5
	if in.DriveTariffID == 3 {
		drive = 12
	}
	monthly := float64(in.CPU)*cpu + in.RAM*ram + float64(in.Drive)*drive
	return math.Round(monthly*months[in.PaymentPeriod]*f.PriceFactor*100) / 100
}

func (f *Fake) createServer(w http.ResponseWriter, r *http.Request, token string) {
	var in createIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apiErr(w, 400, "json_error", "Invalid JSON.")
		return
	}
	switch {
	case in.Datacenter <= 0:
		apiErr(w, 400, "json_error", "Missing field 'datacenter'.")
		return
	case in.TariffID <= 0 || in.DriveTariffID <= 0:
		apiErr(w, 400, "json_error", "Missing field 'tariff_id'.")
		return
	case (in.OSID == nil) == (in.TemplateID == nil):
		apiErr(w, 400, "json_error", "Either 'os_id' or 'template_id' is required.")
		return
	case in.CPU <= 0 || in.RAM <= 0 || in.Drive <= 0 || in.IP <= 0:
		apiErr(w, 400, "json_error", "Invalid server resources.")
		return
	}
	if _, ok := months[in.PaymentPeriod]; !ok {
		apiErr(w, 400, "json_error", "Invalid 'payment_period'.")
		return
	}
	cost := f.price(in)
	if r.URL.Query().Get("get_price_only") == "true" {
		writeJSON(w, 200, map[string]any{"payment_period": in.PaymentPeriod, "cost_rub": cost})
		return
	}
	f.nextID++
	id := f.nextID
	s := &server{ID: id, Status: "initializing", Datacenter: in.Datacenter, TariffID: in.TariffID,
		PaymentPeriod: in.PaymentPeriod, Name: in.ComputerName, Cost: cost, Token: token,
		IP: fmt.Sprintf("10.77.%d.%d", (id/250)%250, id%250+1)}
	if in.UserComment != nil {
		s.Comment = *in.UserComment
	}
	a := &action{ID: id + 100000, Type: "server_create", Status: "running", ResourceID: id}
	bal := f.balanceOf(token)
	switch {
	case f.WaitPayment:
		a.Status, a.ErrID, a.ErrMsg = "wait_user_action", "server_wait_for_payment",
			"Server creation request saved. Creation will start after payment."
		s.Status = "notpaid"
	case bal < cost:
		a.Status, a.ErrID = "error", "insufficient_balance"
		a.ErrMsg = fmt.Sprintf("Insufficient balance amount. You need %.2f RUB but actual balance is %.2f RUB.", cost, bal)
		s.Status = "deleted"
	default:
		f.balance[token] = bal - cost
	}
	f.servers[id] = s
	f.actions[a.ID] = a
	if f.DropCreateResponse {
		f.DropCreateResponse = false
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"virtual_server_id": id, "payment_period": in.PaymentPeriod, "cost_rub": cost,
		"password": "not-a-real-password", "action": actionJSON(a),
	})
}

// advance: асинхронный ход действия: каждый опрос двигает прогресс.
func (f *Fake) advance(a *action) {
	if a.Status != "running" {
		return
	}
	a.Polls++
	s := f.servers[a.ResourceID]
	need := max(f.ReadyAfter, 1)
	a.Progress = min(100, a.Polls*100/need)
	if a.Polls < need {
		if s != nil {
			s.Progress = a.Progress
		}
		return
	}
	a.Status, a.Progress = "success", 100
	if s == nil {
		return
	}
	switch a.Type {
	case "server_create":
		s.Status, s.Progress = "active", 100
		s.PaidTill = time.Now().UTC().AddDate(0, int(months[s.PaymentPeriod]), 0).Truncate(time.Second)
	case "server_remove":
		s.Status = "deleted"
	}
}

func (f *Fake) listServers(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	list := []map[string]any{}
	for _, s := range f.servers {
		if f.HideFromSearch {
			break
		}
		if search != "" && !strings.Contains(s.Comment, search) && strconv.FormatInt(s.ID, 10) != search && s.IP != search {
			continue
		}
		list = append(list, serverJSON(s, true))
	}
	writeJSON(w, 200, map[string]any{"servers": list, "pagination": map[string]any{
		"page": 1, "per_page": 100, "previous_page": nil, "next_page": nil, "last_page": 1, "total_entries": len(list)}})
}

func (f *Fake) serverOp(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	s, ok := f.servers[id]
	if err != nil || !ok || (s.Status == "deleted" && r.Method != http.MethodGet) {
		apiErr(w, 404, "not_found", fmt.Sprintf("Virtual server with id %s is not found.", parts[0]))
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	// Опрос сервера двигает его создание так же, как опрос действия.
	for _, a := range f.actions {
		if a.ResourceID == id && a.Status == "running" && sub == "" && r.Method == http.MethodGet {
			f.advance(a)
		}
	}
	switch {
	case r.Method == http.MethodGet && sub == "":
		if s.Status == "deleted" {
			apiErr(w, 404, "not_found", fmt.Sprintf("Virtual server with id %d is not found.", id))
			return
		}
		writeJSON(w, 200, serverJSON(s, false))
	case r.Method == http.MethodGet && sub == "networks":
		v4 := []map[string]string{}
		if s.Status == "active" {
			v4 = append(v4, map[string]string{"ip_address": s.IP, "netmask": "255.255.255.0", "gateway": "10.77.0.1"})
		}
		writeJSON(w, 200, map[string]any{"v4": v4})
	case r.Method == http.MethodGet && sub == "cost":
		writeJSON(w, 200, map[string]any{"cost_rub": s.Cost, "payment_period": s.PaymentPeriod})
	case r.Method == http.MethodGet && sub == "paid_till":
		writeJSON(w, 200, map[string]any{"virtual_server_id": s.ID, "paid_till": s.PaidTill.Format(time.RFC3339)})
	case r.Method == http.MethodDelete && sub == "":
		a := &action{ID: id + 200000 + int64(len(f.actions)), Type: "server_remove", Status: "running", ResourceID: id}
		f.actions[a.ID] = a
		writeJSON(w, 200, actionJSON(a))
	default:
		apiErr(w, 404, "not_found", "Not found.")
	}
}

func serverJSON(s *server, withNet bool) map[string]any {
	m := map[string]any{
		"virtual_server_id": s.ID, "status": s.Status, "create_progress": s.Progress,
		"datacenter": s.Datacenter, "tariff_id": s.TariffID, "payment_period": s.PaymentPeriod,
		"os_id": 52, "template_id": nil, "cpu": 2, "ram": 2, "drive": 20, "drive_tariff_id": 1,
		"ip": 1, "user_comment": s.Comment,
	}
	if withNet {
		v4 := []map[string]string{}
		if s.Status == "active" {
			v4 = append(v4, map[string]string{"ip_address": s.IP, "netmask": "255.255.255.0", "gateway": "10.77.0.1"})
		}
		m["network_v4"] = v4
	}
	if !s.PaidTill.IsZero() {
		m["paid_till"] = s.PaidTill.Format(time.RFC3339)
	}
	return m
}

func actionJSON(a *action) map[string]any {
	m := map[string]any{"id": a.ID, "type": a.Type, "status": a.Status, "progress": a.Progress,
		"started": time.Now().UTC().Format(time.RFC3339), "finished": nil,
		"resource_id": a.ResourceID, "resource_type": "server"}
	if a.ErrID != "" {
		m["action_error"] = map[string]string{"id": a.ErrID, "message": a.ErrMsg}
	}
	return m
}

func apiErr(w http.ResponseWriter, status int, id, msg string) {
	writeJSON(w, status, map[string]string{"id": id, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
