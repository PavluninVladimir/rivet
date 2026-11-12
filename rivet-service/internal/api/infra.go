package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Провайдеры инфраструктуры и хосты окружений (api-contract
// add-environment-provisioning): /system/infra-connections/* для
// администратора, /environments/{id}/host/{action} для участников проекта.

type moneyView struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type quoteView struct {
	Amount      float64 `json:"amount"`
	Currency    string  `json:"currency"`
	Period      int     `json:"period"`
	PeriodLabel string  `json:"period_label"`
}

type infraConnView struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	HasToken    bool       `json:"has_token"`
	Enabled     bool       `json:"enabled"`
	State       string     `json:"state"`
	CheckDetail string     `json:"check_detail"`
	CheckedAt   *time.Time `json:"checked_at"`
	Balance     *moneyView `json:"balance"`
	UpdatedAt   time.Time  `json:"updated_at"`
	UpdatedBy   string     `json:"updated_by"`
}

func infraConnDTO(c domain.InfraConnection) infraConnView {
	v := infraConnView{ID: c.ID, Provider: c.Provider, Name: c.Name, TokenPrefix: c.TokenPrefix,
		HasToken: c.HasToken, Enabled: c.Enabled, State: string(c.State), CheckDetail: c.CheckDetail,
		CheckedAt: c.CheckedAt, UpdatedAt: c.UpdatedAt, UpdatedBy: c.UpdatedBy}
	if c.BalanceAmount != nil {
		v.Balance = &moneyView{Amount: *c.BalanceAmount, Currency: c.BalanceCurrency}
	}
	return v
}

type envHostView struct {
	ID                string                 `json:"id"`
	Provider          string                 `json:"provider"`
	State             string                 `json:"state"`
	Live              bool                   `json:"live"`
	Spec              domain.ProvisionConfig `json:"spec"`
	SpecLabels        map[string]string      `json:"spec_labels"`
	Address           string                 `json:"address"`
	ServerID          string                 `json:"server_id"`
	Quote             *quoteView             `json:"quote"`
	Balance           *moneyView             `json:"balance"`
	ApprovedBy        string                 `json:"approved_by"`
	ApprovedAt        *time.Time             `json:"approved_at"`
	RequestedBy       string                 `json:"requested_by"`
	WaitingPayment    bool                   `json:"waiting_payment"`
	PaidTill          *time.Time             `json:"paid_till"`
	Cost              *quoteView             `json:"cost"`
	CostCheckedAt     *time.Time             `json:"cost_checked_at"`
	Attempts          int                    `json:"attempts"`
	NeedsRetry        bool                   `json:"needs_retry"`
	RemoveVolumes     bool                   `json:"remove_volumes"`
	DeleteRequestedBy string                 `json:"delete_requested_by"`
	Detail            string                 `json:"detail"`
	Failure           string                 `json:"failure"`
	ComposeProject    string                 `json:"compose_project"`
	Port              int                    `json:"port"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
}

func hostDTO(h domain.EnvHost) *envHostView {
	v := &envHostView{ID: h.ID, Provider: h.Provider, State: h.State, Live: !domain.HostTerminal(h.State),
		Spec: h.Spec, SpecLabels: h.SpecLabels, Address: h.Address, ServerID: h.ServerID, ApprovedBy: h.ApprovedBy, ApprovedAt: h.ApprovedAt,
		RequestedBy: h.RequestedBy, WaitingPayment: h.WaitingPayment, PaidTill: h.PaidTill,
		CostCheckedAt: h.CostCheckedAt, Attempts: h.Attempts, NeedsRetry: h.NeedsRetry,
		RemoveVolumes: h.RemoveVolumes, DeleteRequestedBy: h.DeleteRequestedBy, Detail: h.Detail,
		Failure: h.Failure, CreatedAt: h.Created, UpdatedAt: h.Updated}
	// Публичный ключ в DTO остаётся: он не секрет и нужен форме. Скрипт
	// bootstrap видят участники, как команды доставки.
	if h.QuoteAmount != nil {
		v.Quote = &quoteView{Amount: *h.QuoteAmount, Currency: h.QuoteCurrency, Period: h.QuotePeriod,
			PeriodLabel: ruvds.PeriodLabel(h.QuotePeriod)}
	}
	if h.BalanceAmount != nil {
		v.Balance = &moneyView{Amount: *h.BalanceAmount, Currency: h.BalanceCurrency}
	}
	if h.CostAmount != nil {
		v.Cost = &quoteView{Amount: *h.CostAmount, Currency: h.CostCurrency, Period: h.CostPeriod,
			PeriodLabel: ruvds.PeriodLabel(h.CostPeriod)}
	}
	if h.Provider == domain.ProvisionLocal {
		v.ComposeProject = domain.ComposeProject(h.ProjectID, h.EnvID)
		v.Port = h.Spec.Port
	}
	return v
}

// infraFactory: фабрика провайдеров установки.
func (s *Server) infraFactory() *infra.Factory {
	if s.Engine != nil && s.Engine.Infra != nil {
		return s.Engine.Infra
	}
	return &infra.Factory{}
}

func (s *Server) listInfraConnections(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list, err := s.St.ListInfraConnections(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]infraConnView, 0, len(list))
	for _, c := range list {
		out = append(out, infraConnDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// putInfraConnection: создать или изменить подключение; токен только из
// сессии консоли (design, решение 11). После сохранения: проверка.
func (s *Server) putInfraConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Provider string  `json:"provider"`
		Name     string  `json:"name"`
		Token    *string `json:"token"`
		Enabled  *bool   `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	if in.Token != nil && !requireSession(w, r) {
		return
	}
	if in.Provider == "" {
		in.Provider = domain.ProvisionRUVDS
	}
	u := currentUser(r)
	c, created, err := s.St.UpsertInfraConnection(r.Context(), store.InfraConnInput{ID: r.PathValue("id"),
		Provider: in.Provider, Name: in.Name, Token: in.Token, Enabled: in.Enabled}, s.Secrets, u.ID, u.Login)
	if err != nil {
		s.writeInfraErr(w, err)
		return
	}
	if in.Token != nil {
		if c, err = s.checkInfraConnection(r.Context(), c.ID, u.Login); err != nil {
			s.writeInfraErr(w, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"connection": infraConnDTO(c)})
}

// checkInfraConnection: баланс у провайдера: 401: invalid, сеть:
// unchecked с пояснением.
func (s *Server) checkInfraConnection(ctx context.Context, id, actor string) (domain.InfraConnection, error) {
	provider, token, err := s.St.InfraConnectionToken(ctx, id, s.Secrets)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.InfraConnection{}, err
		}
		return s.St.SetInfraConnectionCheck(ctx, id, domain.LLMStateInvalid, err.Error(), nil, "", actor)
	}
	p, err := s.infraFactory().Provider(provider, token)
	if err != nil {
		return s.St.SetInfraConnectionCheck(ctx, id, domain.LLMStateInvalid, err.Error(), nil, "", actor)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bal, err := p.Balance(cctx)
	switch {
	case err == nil:
		var amount *float64
		cur := ""
		if bal != nil {
			amount, cur = &bal.Amount, bal.Currency
		}
		return s.St.SetInfraConnectionCheck(ctx, id, domain.LLMStateOK, "", amount, cur, actor)
	case errors.Is(err, infra.ErrUnauthorized), errors.Is(err, infra.ErrForbidden):
		return s.St.SetInfraConnectionCheck(ctx, id, domain.LLMStateInvalid, err.Error(), nil, "", actor)
	default:
		return s.St.SetInfraConnectionCheck(ctx, id, domain.LLMStateUnchecked,
			"проверка не дошла до провайдера: "+err.Error(), nil, "", actor)
	}
}

func (s *Server) checkInfraConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, err := s.checkInfraConnection(r.Context(), r.PathValue("id"), user(r))
	if err != nil {
		s.writeInfraErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": infraConnDTO(c)})
}

func (s *Server) deleteInfraConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.St.DeleteInfraConnection(r.Context(), r.PathValue("id"), user(r)); err != nil {
		s.writeInfraErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// infraCatalog: справочники провайдера для формы окружения.
func (s *Server) infraCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	provider, token, err := s.St.InfraConnectionToken(r.Context(), r.PathValue("id"), s.Secrets)
	if err != nil {
		s.writeInfraErr(w, err)
		return
	}
	if provider != domain.ProvisionRUVDS {
		unprocessable(w, "у провайдера нет справочников")
		return
	}
	base := s.infraFactory().RUVDSBaseURL
	if base == "" {
		base = infra.DefaultRUVDSURL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cat, err := ruvds.New(base, token, s.infraFactory().HTTP).Catalog(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]apiError{"error": {Code: "provider_error",
			Message: "провайдер не отдал справочники: " + err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

// writeInfraErr: ошибки подключений инфраструктуры.
func (s *Server) writeInfraErr(w http.ResponseWriter, err error) {
	var inUse *store.ErrInUse
	if errors.As(err, &inUse) {
		names := make([]string, 0, len(inUse.Refs))
		for _, r := range inUse.Refs {
			names = append(names, r.ID)
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": apiError{Code: "in_use", Message: "подключение используется окружениями: " + strings.Join(names, ", ")},
			"refs":  inUse.Refs,
		})
		return
	}
	if errors.Is(err, secretbox.ErrNoKey) {
		s.noSecretKey(w)
		return
	}
	s.writeConnErr(w, err)
}

// hostAction: POST /environments/{id}/host/{action}.
func (s *Server) hostAction(w http.ResponseWriter, r *http.Request) {
	env, err := s.St.EnvironmentForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	action := r.PathValue("action")
	switch action {
	case orchestrator.HostActRequest, orchestrator.HostActApprove, orchestrator.HostActReject,
		orchestrator.HostActDelete, orchestrator.HostActConfirmDelete, orchestrator.HostActCancelDelete,
		orchestrator.HostActRetry, orchestrator.HostActRefresh:
	default:
		writeErr(w, store.ErrNotFound)
		return
	}
	if action != orchestrator.HostActRefresh && !s.requireOwner(w, r, env.ProjectID) {
		return
	}
	// Деньги и необратимое удаление подтверждает человек в консоли.
	if (action == orchestrator.HostActApprove || action == orchestrator.HostActConfirmDelete) && !requireSession(w, r) {
		return
	}
	var in struct {
		Amount        *float64 `json:"amount"`
		RemoveVolumes bool     `json:"remove_volumes"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			unprocessable(w, "невалидное тело запроса")
			return
		}
	}
	h, err := s.Engine.HostAction(r.Context(), env, orchestrator.HostActionInput{Action: action, Actor: user(r),
		Amount: in.Amount, RemoveVolumes: in.RemoveVolumes})
	if err != nil {
		var bad *store.ErrHostState
		switch {
		case errors.As(err, &bad):
			writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "bad_state", Message: err.Error()}})
		case errors.Is(err, store.ErrPriceChanged):
			writeJSON(w, http.StatusConflict, map[string]any{"error": apiError{Code: "price_changed", Message: err.Error()},
				"host": hostDTO(h)})
		default:
			writeErr(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": hostDTO(h)})
}
