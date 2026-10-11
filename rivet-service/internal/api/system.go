package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Эксплуатация установки (change add-operations-management): состояние,
// токены регистрации runner'ов, провайдеры модели декомпозиции.

// EnvPlanner — настройки модели из окружения: запасной источник, когда в
// базе нет активного провайдера (спека epic-decomposition «Запасной
// источник из окружения»).
type EnvPlanner struct {
	Provider string
	Key      string
	Model    string
}

// ReloadPlanner пересобирает планировщик: активный провайдер из базы, иначе
// окружение, иначе «модель не настроена». Вызывается при старте и после
// каждого изменения провайдеров.
func (s *Server) ReloadPlanner(ctx context.Context) error {
	if s.Planner == nil {
		s.Planner = &planner.Holder{}
	}
	pm, ok, err := s.St.PlannerModel(ctx)
	if err != nil {
		return err
	}
	if ok {
		cl, c, err := s.St.ConnectionClient(ctx, pm.ConnectionID, s.Secrets)
		st := planner.Status{Source: planner.SourceCatalog, ConnectionID: pm.ConnectionID, Model: pm.Model}
		switch {
		case errors.Is(err, store.ErrNotFound):
			st.State, st.Detail = "invalid", "подключение "+pm.ConnectionID+" удалено"
			s.Planner.Set(nil, st)
		case errors.Is(err, store.ErrSecret), errors.Is(err, secretbox.ErrNoKey):
			// Ключ в базе есть, но его не расшифровать (сменили или сняли
			// RIVET_SECRET_KEY): декомпозиция отказывает с причиной, а не падает.
			st.State, st.Detail = "invalid", err.Error()
			s.Planner.Set(nil, st)
		case err != nil:
			return err
		case !c.Enabled:
			st.State, st.Detail = "invalid", "подключение "+c.ID+" отключено"
			s.Planner.Set(nil, st)
		case c.State == domain.LLMStateInvalid:
			st.State, st.Detail = "invalid", "ключ подключения "+c.ID+" отклонён: "+c.CheckDetail
			s.Planner.Set(nil, st)
		default:
			st.State, st.Detail = string(c.State), c.CheckDetail
			s.Planner.Set(planner.FromClient(cl, pm.Model), st)
		}
		return nil
	}
	if s.EnvPlanner.Provider != "" && s.EnvPlanner.Key != "" {
		pl, err := planner.Build(s.EnvPlanner.Provider, s.EnvPlanner.Key, s.EnvPlanner.Model)
		if err != nil {
			return err
		}
		model := s.EnvPlanner.Model
		if model == "" {
			model = planner.DefaultModel(s.EnvPlanner.Provider)
		}
		s.Planner.Set(pl, planner.Status{Source: planner.SourceEnv, ConnectionID: s.EnvPlanner.Provider, Model: model, State: "unchecked"})
		return nil
	}
	s.Planner.Set(nil, planner.Status{Source: planner.SourceNone, State: "unchecked"})
	return nil
}

// ─── health / status ─────────────────────────────────────────────────────

// health — публичная проверка живости: 200 только при ответе базы, деталей
// нет (спека observability «Состояние установки»).
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.St == nil || s.St.Pool.Ping(ctx) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type componentView struct {
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Detail string         `json:"detail"`
	Data   map[string]any `json:"data,omitempty"`
}

// systemStatus — GET /system/status: компоненты установки администратору.
// Endpoint за аутентификацией, а она ходит в базу: при полном отказе базы
// сюда не дойти (ответ 401), и «database: down» здесь видно только при
// деградации (медленный Ping). Полный отказ показывает публичный /health.
func (s *Server) systemStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var comps []componentView

	db := componentView{Name: "database", Status: "ok", Detail: "PostgreSQL отвечает"}
	if err := s.St.Pool.Ping(ctx); err != nil {
		db.Status, db.Detail = "down", "PostgreSQL не отвечает: "+err.Error()
	}
	comps = append(comps, db)

	bl := componentView{Name: "blob", Status: "ok", Detail: "хранилище транскриптов доступно"}
	if s.Blob == nil {
		bl.Status, bl.Detail = "degraded", "хранилище не подключено при старте: транскрипты не сохраняются, нужен перезапуск после восстановления"
	} else {
		bl.Data = map[string]any{"bucket": s.Blob.Bucket()}
		if err := s.Blob.Ping(ctx); err != nil {
			bl.Status, bl.Detail = "degraded", "хранилище недоступно: "+err.Error()
		}
	}
	comps = append(comps, bl)

	sec := componentView{Name: "secrets", Status: "ok", Detail: "ключ шифрования задан"}
	if !s.Secrets.Enabled() {
		sec.Status, sec.Detail = "degraded", "RIVET_SECRET_KEY не задан: учётные данные хостингов и ключи моделей не сохранить"
	}
	comps = append(comps, sec)

	pl := componentView{Name: "planner", Status: "ok"}
	_, st := s.plannerStatus()
	pl.Data = map[string]any{"source": string(st.Source), "connection_id": st.ConnectionID, "model": st.Model}
	switch {
	case st.Source == planner.SourceNone:
		pl.Status, pl.Detail = "degraded", "модель для декомпозиции не настроена"
	case st.State == string(domain.LLMStateInvalid):
		pl.Status, pl.Detail = "degraded", "модель декомпозиции недоступна: "+st.Detail
	case st.Source == planner.SourceEnv:
		pl.Detail = "модель из окружения установки (" + st.ConnectionID + ")"
	default:
		pl.Detail = "модель из каталога (" + st.ConnectionID + "/" + st.Model + ")"
	}
	comps = append(comps, pl)

	// Движок политик: недоступность — деградация, а не отказ установки.
	// Автоматика при этом стоит (fail-closed), люди продолжают работать.
	pol := componentView{Name: "policy", Status: "ok"}
	ev := s.engineView(r)
	pol.Data = map[string]any{"mode": ev.Mode}
	pol.Detail = ev.Detail
	if ev.State != "ok" {
		pol.Status = "degraded"
		pol.Detail = "движок политик (" + ev.Mode + ") не отвечает: " + ev.Detail
	}
	comps = append(comps, pol)

	rn := componentView{Name: "runners", Status: "ok"}
	runners, err := s.St.ListRunners(ctx)
	if err != nil {
		rn.Status, rn.Detail = "down", err.Error()
	} else {
		online := 0
		for _, r := range runners {
			if r.Status != domain.RunnerOffline {
				online++
			}
		}
		rn.Data = map[string]any{"online": online, "total": len(runners), "grpc_addr": s.GRPCAddr, "tls": s.GRPCTLS}
		switch {
		case len(runners) == 0:
			rn.Status, rn.Detail = "degraded", "ни один runner не зарегистрирован"
		case online == 0:
			rn.Status, rn.Detail = "degraded", "все runner'ы offline"
		default:
			rn.Detail = "runner'ы в сети"
		}
	}
	comps = append(comps, rn)

	overall := "ok"
	for _, c := range comps {
		if c.Status == "down" {
			overall = "down"
			break
		}
		if c.Status == "degraded" {
			overall = "degraded"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": overall, "version": s.Version, "protocol_version": s.ProtocolVersion,
		"started_at": s.StartedAt, "components": comps,
	})
}

// plannerStatus — планировщик и его статус; без Holder — «не настроен».
func (s *Server) plannerStatus() (*planner.Planner, planner.Status) {
	if s.Planner == nil {
		return nil, planner.Status{Source: planner.SourceNone, State: "unchecked"}
	}
	return s.Planner.Get()
}

// ─── runner tokens ────────────────────────────────────────────────────────

type runnerTokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func runnerTokenDTO(t domain.RunnerToken) runnerTokenView {
	return runnerTokenView{
		ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.Created, CreatedBy: t.CreatedBy,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsed, RevokedAt: t.RevokedAt,
	}
}

func (s *Server) listRunnerTokens(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list, err := s.St.ListRunnerTokens(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]runnerTokenView, 0, len(list))
	for _, t := range list {
		out = append(out, runnerTokenDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createRunnerToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		unprocessable(w, "нужно имя токена")
		return
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		unprocessable(w, "срок действия уже истёк")
		return
	}
	u := currentUser(r)
	t, secret, err := s.St.CreateRunnerToken(r.Context(), in.Name, in.ExpiresAt, u.ID, u.Login)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": runnerTokenDTO(t), "secret": secret})
}

func (s *Server) revokeRunnerToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.St.RevokeRunnerToken(r.Context(), r.PathValue("id"), currentUser(r).Login); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
