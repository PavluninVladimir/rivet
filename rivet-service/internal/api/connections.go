package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Подключения к моделям и модель планировщика (add-model-connections,
// api-contract): /system/connections/*, /system/planner. Администратор.

type connectionView struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Kind        string              `json:"kind"`
	API         string              `json:"api"`
	BaseURL     string              `json:"base_url"`
	KeyPrefix   string              `json:"key_prefix"`
	HasKey      bool                `json:"has_key"`
	Headers     []domain.ConnHeader `json:"headers"`
	Models      []domain.ModelEntry `json:"models"`
	Enabled     bool                `json:"enabled"`
	State       string              `json:"state"`
	CheckDetail string              `json:"check_detail"`
	CheckedAt   *time.Time          `json:"checked_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
	UpdatedBy   string              `json:"updated_by"`
}

func connDTO(c domain.ModelConnection) connectionView {
	return connectionView{
		ID: c.ID, Name: c.Name, Kind: c.Kind, API: c.API, BaseURL: c.BaseURL, KeyPrefix: c.KeyPrefix, HasKey: c.HasKey,
		Headers: c.Headers, Models: c.Models, Enabled: c.Enabled, State: string(c.State), CheckDetail: c.CheckDetail,
		CheckedAt: c.CheckedAt, UpdatedAt: c.UpdatedAt, UpdatedBy: c.UpdatedBy,
	}
}

// listModels — вызов провайдера за списком моделей; в тестах подменяется.
var listModels = func(ctx context.Context, c llm.Client) ([]llm.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.ListModels(ctx)
}

// checkState переводит результат обращения к провайдеру в состояние
// подключения: отказ ключа — invalid, сеть — unchecked с пояснением.
func checkState(err error) (domain.LLMProviderState, string) {
	switch {
	case err == nil:
		return domain.LLMStateOK, ""
	case errors.Is(err, llm.ErrUnauthorized):
		return domain.LLMStateInvalid, err.Error()
	default:
		return domain.LLMStateUnchecked, "проверка не дошла до провайдера: " + err.Error()
	}
}

// writeFieldErr — 422 с полем формы, 503 без ключа шифрования, 409 при ссылках.
func (s *Server) writeConnErr(w http.ResponseWriter, err error) {
	var fe *store.FieldError
	var inUse *store.ErrInUse
	switch {
	case errors.As(err, &fe):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": apiError{Code: "invalid", Message: fe.Msg}, "field": fe.Field,
		})
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": apiError{Code: "in_use", Message: "подключение используется: отключите его вместо удаления"},
			"refs":  inUse.Refs,
		})
	case errors.Is(err, secretbox.ErrNoKey):
		s.noSecretKey(w)
	case errors.Is(err, store.ErrInvalid):
		unprocessable(w, err.Error())
	default:
		writeErr(w, err)
	}
}

func (s *Server) noSecretKey(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
		Code: "no_secret_key", Message: "ключ шифрования не настроен: секрет подключения не сохранить"}})
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list, err := s.St.ListConnections(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]connectionView, 0, len(list))
	for _, c := range list {
		out = append(out, connDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type headerIn struct {
	Name   string  `json:"name"`
	Value  *string `json:"value"`
	Secret bool    `json:"secret"`
}

// putConnection — создать или изменить подключение, затем проверить его у
// провайдера (design: сохранение не зависит от доступности провайдера).
func (s *Server) putConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Name    string      `json:"name"`
		Kind    string      `json:"kind"`
		API     string      `json:"api"`
		BaseURL string      `json:"base_url"`
		Key     *string     `json:"key"`
		Headers *[]headerIn `json:"headers"`
		Enabled *bool       `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	inp := store.ConnectionInput{ID: r.PathValue("id"), Name: in.Name, Kind: in.Kind, API: in.API, BaseURL: in.BaseURL, Key: in.Key, Enabled: in.Enabled}
	if in.Headers != nil {
		hs := make([]store.HeaderInput, 0, len(*in.Headers))
		for _, h := range *in.Headers {
			hs = append(hs, store.HeaderInput{Name: h.Name, Value: h.Value, Secret: h.Secret})
		}
		inp.Headers = &hs
	}
	u := currentUser(r)
	c, created, err := s.St.UpsertConnection(r.Context(), inp, s.Secrets, u.ID, u.Login)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	c, err = s.checkConnection(r.Context(), c.ID, u.Login)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	if err := s.ReloadPlanner(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, connDTO(c))
}

// checkConnection проверяет подключение списком моделей и записывает состояние.
func (s *Server) checkConnection(ctx context.Context, id, actor string) (domain.ModelConnection, error) {
	cl, _, err := s.St.ConnectionClient(ctx, id, s.Secrets)
	if err != nil {
		// Ключ есть, но не расшифровать (сменили RIVET_SECRET_KEY) — состояние
		// invalid; остальные ошибки (база, нет ключа шифрования) — наверх.
		if errors.Is(err, store.ErrSecret) {
			return s.St.SetConnectionCheck(ctx, id, domain.LLMStateInvalid, err.Error(), actor)
		}
		return domain.ModelConnection{}, err
	}
	_, perr := listModels(ctx, cl)
	state, detail := checkState(perr)
	return s.St.SetConnectionCheck(ctx, id, state, detail, actor)
}

func (s *Server) checkConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, err := s.checkConnection(r.Context(), r.PathValue("id"), currentUser(r).Login)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	if err := s.ReloadPlanner(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, connDTO(c))
}

func (s *Server) discoverConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	cl, _, err := s.St.ConnectionClient(r.Context(), id, s.Secrets)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	found, err := listModels(r.Context(), cl)
	if err != nil {
		state, detail := checkState(err)
		if state != domain.LLMStateUnchecked {
			_, _ = s.St.SetConnectionCheck(r.Context(), id, state, detail, currentUser(r).Login)
		}
		writeJSON(w, http.StatusBadGateway, map[string]apiError{"error": {
			Code: "provider_error", Message: "провайдер не отдал список моделей: " + err.Error()}})
		return
	}
	c, added, missing, err := s.St.MergeDiscoveredModels(r.Context(), id, found, currentUser(r).Login)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	if c.State != domain.LLMStateOK {
		if c, err = s.St.SetConnectionCheck(r.Context(), id, domain.LLMStateOK, "", currentUser(r).Login); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := s.ReloadPlanner(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	if added == nil {
		added = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connDTO(c), "added": added, "missing": missing})
}

func (s *Server) putConnectionModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Models []domain.ModelEntry `json:"models"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	u := currentUser(r)
	c, err := s.St.SetConnectionModels(r.Context(), r.PathValue("id"), in.Models, u.ID, u.Login)
	if err != nil {
		s.writeConnErr(w, err)
		return
	}
	if err := s.ReloadPlanner(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, connDTO(c))
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.St.DeleteConnection(r.Context(), r.PathValue("id"), currentUser(r).Login); err != nil {
		s.writeConnErr(w, err)
		return
	}
	if err := s.ReloadPlanner(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── модель планировщика ─────────────────────────────────────────────────

type plannerView struct {
	Source       string `json:"source"`
	ConnectionID string `json:"connection_id,omitempty"`
	Model        string `json:"model,omitempty"`
	State        string `json:"state,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

func (s *Server) plannerView() plannerView {
	_, st := s.plannerStatus()
	v := plannerView{Source: string(st.Source), Model: st.Model, State: st.State, Detail: st.Detail}
	if st.Source == planner.SourceCatalog {
		v.ConnectionID = st.ConnectionID
	}
	return v
}

func (s *Server) getPlanner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.plannerView())
}

func (s *Server) putPlanner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		ConnectionID string `json:"connection_id"`
		Model        string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	u := currentUser(r)
	var pm *domain.PlannerModel
	if in.ConnectionID != "" || in.Model != "" {
		if in.ConnectionID == "" || in.Model == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": apiError{Code: "invalid", Message: "укажите подключение и модель"}, "field": "model"})
			return
		}
		pm = &domain.PlannerModel{ConnectionID: in.ConnectionID, Model: in.Model}
	}
	if err := s.St.SetPlannerModel(r.Context(), pm, u.ID, u.Login); err != nil {
		s.writeConnErr(w, err)
		return
	}
	if err := s.ReloadPlanner(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.plannerView())
}
