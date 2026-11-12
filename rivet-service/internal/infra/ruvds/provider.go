package ruvds

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/PavluninVladimir/rivet/internal/infra"
)

func init() {
	infra.Register(infra.KindRUVDS, func(baseURL, token string, hc *http.Client) infra.Provider {
		return &Provider{C: New(baseURL, token, hc)}
	})
}

// Provider: провайдер инфраструктуры RUVDS: платный удалённый сервер.
type Provider struct {
	C *Client
}

func (p *Provider) Kind() string { return infra.KindRUVDS }
func (p *Provider) Remote() bool { return true }
func (p *Provider) Paid() bool   { return true }

func (p *Provider) Quote(ctx context.Context, s infra.Spec) (infra.Quote, error) {
	return p.C.Price(ctx, s)
}

func (p *Provider) Balance(ctx context.Context) (*infra.Money, error) {
	m, err := p.C.Balance(ctx)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Find: неудалённый сервер с меткой в комментарии.
func (p *Provider) Find(ctx context.Context, marker string) (*infra.Server, error) {
	list, err := p.C.FindServers(ctx, marker)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		srv := fromInfo(s)
		if srv.State == infra.ServerDeleted {
			continue
		}
		return &srv, nil
	}
	return nil, nil
}

// Create: ключ SSH в аккаунте и создание сервера с метками.
func (p *Provider) Create(ctx context.Context, s infra.Spec, l infra.Labels) (infra.Server, error) {
	keyID := ""
	if s.SSHPublicKey != "" {
		id, err := p.C.EnsureSSHKey(ctx, s.SSHPublicKey, l.ComputerName)
		if err != nil {
			// Ключ не получен: создание сервера ещё не отправлено, исход
			// ясен при любой ошибке: отказ провайдера остаётся отказом, а
			// сбой сети или ответа становится «не отправлено» (повтор безопасен).
			if infra.Definitive(err) {
				return infra.Server{}, err
			}
			return infra.Server{}, &infra.Error{Kind: infra.ErrNotSent, Message: "ключ SSH: " + err.Error()}
		}
		keyID = id
	}
	resp, err := p.C.Create(ctx, s, l, keyID)
	if err != nil {
		return infra.Server{}, err
	}
	srv := infra.Server{ID: strconv.FormatInt(resp.ID, 10), State: infra.ServerPending}
	if resp.Action != nil && resp.Action.ID > 0 {
		srv.ActionID = strconv.FormatInt(resp.Action.ID, 10)
		applyAction(&srv, *resp.Action)
	}
	return srv, nil
}

// Poll: сначала действие (ошибка, ожидание оплаты), затем сервер и адрес.
func (p *Provider) Poll(ctx context.Context, srv infra.Server) (infra.Server, error) {
	out := infra.Server{ID: srv.ID, ActionID: srv.ActionID, State: infra.ServerPending}
	if srv.ActionID != "" {
		a, err := p.C.Action(ctx, srv.ActionID)
		if err != nil && !errors.Is(err, infra.ErrNotFound) {
			return srv, err
		}
		if err == nil {
			applyAction(&out, a)
			if out.State == infra.ServerFailed || out.State == infra.ServerWaitingPayment {
				return out, nil
			}
		}
	}
	info, err := p.C.Server(ctx, srv.ID)
	if errors.Is(err, infra.ErrNotFound) {
		out.State = infra.ServerDeleted
		return out, nil
	}
	if err != nil {
		return srv, err
	}
	s := fromInfo(info)
	s.ActionID, s.Failure, s.Detail = out.ActionID, out.Failure, out.Detail
	if s.State == infra.ServerActive && s.Address == "" {
		nets, err := p.C.Networks(ctx, srv.ID)
		if err != nil {
			return srv, err
		}
		s.Address = ipv4(nets)
		if s.Address == "" {
			s.State = infra.ServerPending // адреса ещё нет
		}
	}
	return s, nil
}

// Delete: удаление; сервера уже нет: удалён.
func (p *Provider) Delete(ctx context.Context, serverID string) (infra.Server, error) {
	a, err := p.C.Delete(ctx, serverID)
	if errors.Is(err, infra.ErrNotFound) {
		return infra.Server{ID: serverID, State: infra.ServerDeleted}, nil
	}
	if err != nil {
		return infra.Server{}, err
	}
	srv := infra.Server{ID: serverID, State: infra.ServerPending}
	if a.ID > 0 {
		srv.ActionID = strconv.FormatInt(a.ID, 10)
	}
	return srv, nil
}

func (p *Provider) Billing(ctx context.Context, serverID string) (infra.Billing, error) {
	cost, err := p.C.Cost(ctx, serverID)
	if err != nil {
		return infra.Billing{}, err
	}
	till, err := p.C.PaidTill(ctx, serverID)
	if err != nil {
		return infra.Billing{}, err
	}
	return infra.Billing{Cost: &cost, PaidTill: till}, nil
}

// Describe: названия из справочника аккаунта для решения человека.
func (p *Provider) Describe(ctx context.Context, s infra.Spec) (map[string]string, error) {
	cat, err := p.C.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range cat.Datacenters {
		if d.ID == s.Datacenter {
			out["datacenter"] = d.Name
		}
	}
	for _, t := range cat.Tariffs {
		if t.ID == s.TariffID {
			out["tariff"] = t.Name
		}
	}
	for _, t := range cat.DriveTariffs {
		if t.ID == s.DriveTariffID {
			out["drive_tariff"] = t.Name
		}
	}
	for _, o := range cat.OS {
		if o.ID == s.OSID {
			out["os"] = o.Name
		}
	}
	return out, nil
}

// DefaultBootstrap: первичная настройка нового сервера: Docker с compose.
const DefaultBootstrap = `set -e
command -v docker >/dev/null 2>&1 || curl -fsSL https://get.docker.com | sh
docker compose version`

func (p *Provider) BootstrapScript(s infra.Spec) string {
	if s.Bootstrap != "" {
		return s.Bootstrap
	}
	return DefaultBootstrap
}

// TeardownScript: пусто: удаление сервера уносит всё.
func (p *Provider) TeardownScript(infra.Spec, bool) string { return "" }

// applyAction: состояние по действию RUVDS.
func applyAction(srv *infra.Server, a Action) {
	srv.Progress = clampProgress(a.Progress)
	switch a.Status {
	case "success":
		// итог решает сервер
	case "running":
		srv.State = infra.ServerPending
	case "wait_user_action":
		srv.State = infra.ServerWaitingPayment
	case "error":
		srv.State = infra.ServerFailed
		if a.Error != nil {
			srv.Failure, srv.Detail = clip(a.Error.ID), clip(a.Error.Message)
			switch a.Error.ID {
			case "server_wait_for_payment":
				srv.State = infra.ServerWaitingPayment
			case "too_much_servers_wait_for_payment":
				srv.Failure = "insufficient_balance"
			}
		}
	default:
		srv.State = infra.ServerPending // неизвестный статус: не успех
	}
}

// fromInfo: состояние по серверу RUVDS.
func fromInfo(s ServerInfo) infra.Server {
	out := infra.Server{ID: strconv.FormatInt(s.ID, 10), Progress: clampProgress(s.CreateProgress),
		Comment: clip(s.UserComment), Address: ipv4(s.NetworkV4)}
	switch s.Status {
	case "active":
		out.State = infra.ServerActive
		if out.Progress < 100 {
			out.State = infra.ServerPending
		}
	case "notpaid":
		out.State = infra.ServerWaitingPayment
	case "blocked":
		out.State = infra.ServerBlocked
	case "deleted":
		out.State = infra.ServerDeleted
	default: // initializing и неизвестные
		out.State = infra.ServerPending
	}
	return out
}

func clampProgress(p int) int {
	return max(0, min(100, p))
}
