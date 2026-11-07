package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/analyst"
	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Исследование продукта (change add-product-discovery, api-contract):
// интервью, анализ Аналитиком, бэклог и его утверждение, зависимости
// Epic'ов. Действия — владельцу проекта, чтение — участникам.

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// maxInterviewBody — предел тела загрузки: 200 000 символов по 4 байта и
// запас на JSON или multipart.
const maxInterviewBody = 1 << 20 * 4

// discErr — ошибка в общем формате с полем и дополнительными сведениями
// (run_id, backlog_rev) внутри объекта error.
func discErr(w http.ResponseWriter, status int, code, msg, field string, extra map[string]any) {
	e := map[string]any{"code": code, "message": msg}
	if field != "" {
		e["field"] = field
	}
	for k, v := range extra {
		e[k] = v
	}
	writeJSON(w, status, map[string]any{"error": e})
}

// writeDiscoveryErr переводит ошибки исследования в коды контракта.
func writeDiscoveryErr(w http.ResponseWriter, err error) {
	var field *store.DiscoveryFieldError
	var running *store.AnalysisRunningError
	var conflict *store.BacklogConflictError
	switch {
	case errors.As(err, &field):
		discErr(w, http.StatusBadRequest, "invalid", field.Msg, field.Field, nil)
	case errors.Is(err, store.ErrInterviewTooLarge):
		discErr(w, http.StatusRequestEntityTooLarge, "interview_too_large", err.Error(), "text",
			map[string]any{"limit": store.MaxInterviewChars})
	case errors.Is(err, store.ErrInterviewsLimit):
		discErr(w, http.StatusConflict, "interviews_limit", err.Error(), "", map[string]any{"limit": store.MaxInterviews})
	case errors.Is(err, store.ErrNoInterviews):
		discErr(w, http.StatusBadRequest, "no_interviews", err.Error(), "", nil)
	case errors.Is(err, store.ErrFeedbackLimit):
		discErr(w, http.StatusConflict, "feedback_limit", err.Error(), "", map[string]any{"limit": store.MaxFeedback})
	case errors.As(err, &running):
		discErr(w, http.StatusConflict, "analysis_running", "анализ проекта уже идёт", "", map[string]any{"run_id": running.RunID})
	case errors.As(err, &conflict):
		discErr(w, http.StatusConflict, "backlog_conflict", "бэклог уже изменён: перечитайте его", "", map[string]any{"backlog_rev": conflict.Rev})
	case errors.Is(err, store.ErrRunNotEditable):
		discErr(w, http.StatusConflict, "run_not_editable", err.Error(), "", nil)
	case errors.Is(err, store.ErrRunNotApprovable):
		discErr(w, http.StatusConflict, "run_not_approvable", err.Error(), "", nil)
	case errors.Is(err, store.ErrEpicClosed):
		discErr(w, http.StatusConflict, "epic_closed", err.Error(), "", nil)
	default:
		writeErr(w, err)
	}
}

// pathIDs проверяет идентификаторы пути: не UUID — 404, как для чужого.
func pathIDs(w http.ResponseWriter, ids ...string) bool {
	for _, id := range ids {
		if !uuidRe.MatchString(id) {
			writeErr(w, store.ErrNotFound)
			return false
		}
	}
	return true
}

// ─── DTO ─────────────────────────────────────────────────────────────────

type discRunSummaryView struct {
	ID         string     `json:"id"`
	Version    int        `json:"version"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	AgentName  string     `json:"agent_name,omitempty"`
	Model      string     `json:"model"`
	Outdated   bool       `json:"outdated"`
}

func discRunSummaryDTO(r store.DiscoveryRun) discRunSummaryView {
	return discRunSummaryView{ID: r.ID, Version: r.Version, Status: r.Status, CreatedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, AgentName: r.AgentName, Model: r.Model, Outdated: r.Outdated}
}

// backlogRow — строка бэклога: Epic и, после утверждения, связь и ход
// декомпозиции (только чтение).
type backlogRow struct {
	discovery.BacklogEpic
	EpicID    *string               `json:"epic_id"`
	Decompose *store.DecomposeState `json:"decompose"`
}

type discRunView struct {
	discRunSummaryView
	Stage      string               `json:"stage"`
	Progress   map[string]int       `json:"progress"`
	Agent      map[string]string    `json:"agent"`
	Role       map[string]string    `json:"role"`
	Interviews []store.RunInterview `json:"interviews"`
	Personas   []discovery.Persona  `json:"personas"`
	Needs      []discovery.Need     `json:"needs"`
	Proposed   []backlogRow         `json:"proposed"`
	Backlog    []backlogRow         `json:"backlog"`
	BacklogRev int                  `json:"backlog_rev"`
	Dropped    []discovery.Dropped  `json:"dropped"`
	Defects    []string             `json:"defects"`
	Error      string               `json:"error,omitempty"`
	Usage      map[string]any       `json:"usage"`
	Approved   map[string]any       `json:"approved"`
	StartedAt  time.Time            `json:"started_at"`
}

func discRunDTO(r store.DiscoveryRun) discRunView {
	v := discRunView{
		discRunSummaryView: discRunSummaryDTO(r),
		Stage:              r.Stage,
		Progress:           map[string]int{"done": r.ProgressDone, "total": r.ProgressTotal},
		Role:               map[string]string{"id": domain.RoleAnalyst, "title": r.RoleTitle},
		Interviews:         r.Interviews,
		Personas:           []discovery.Persona{},
		Needs:              []discovery.Need{},
		Proposed:           []backlogRow{},
		Backlog:            []backlogRow{},
		BacklogRev:         r.BacklogRev,
		Dropped:            r.Dropped,
		Defects:            r.Defects,
		Error:              r.Error,
		StartedAt:          r.StartedAt,
	}
	if v.Role["title"] == "" {
		v.Role["title"] = "Аналитик"
	}
	if r.AgentID != "" {
		v.Agent = map[string]string{"id": r.AgentID, "name": r.AgentName}
	}
	if r.Result != nil {
		v.Personas, v.Needs = nonNilSlice(r.Result.Personas), nonNilSlice(r.Result.Needs)
		for _, e := range r.Result.Epics {
			v.Proposed = append(v.Proposed, backlogRow{BacklogEpic: e})
		}
	}
	ids := map[string]string{}
	for _, a := range r.ApprovedEpics {
		ids[a.Key] = a.EpicID
	}
	for _, e := range r.Backlog {
		row := backlogRow{BacklogEpic: e}
		if id, ok := ids[e.Key]; ok {
			row.EpicID = &id
		}
		if d, ok := r.Decompose[e.Key]; ok {
			row.Decompose = &d
		}
		v.Backlog = append(v.Backlog, row)
	}
	usage := map[string]any{"duration_s": r.DurationS}
	if r.TokensIn != nil {
		usage["tokens_in"] = *r.TokensIn
	}
	if r.TokensOut != nil {
		usage["tokens_out"] = *r.TokensOut
	}
	if r.CostUSD != nil {
		usage["cost_usd"] = *r.CostUSD
	}
	v.Usage = usage
	if r.ApprovedAt != nil {
		epics := r.ApprovedEpics
		if epics == nil {
			epics = []store.ApprovedEpic{}
		}
		v.Approved = map[string]any{"at": r.ApprovedAt, "by": r.ApprovedBy, "epics": epics}
	}
	return v
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ─── чтение ──────────────────────────────────────────────────────────────

// getDiscovery — GET /projects/{id}/discovery: интервью, отзывы с числом
// неучтённых, текущая версия и история.
func (s *Server) getDiscovery(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	if !pathIDs(w, pid) || !s.requireMember(w, r, pid) {
		return
	}
	ivs, err := s.St.ListInterviews(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	feedback, err := s.St.ListFeedback(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	unaccounted, err := s.St.UnaccountedFeedback(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Сколько отзывов последней готовой версии вошло во вход синтеза: при
	// сокращении входа отзыв в версии ещё не прочитан Аналитиком.
	// Сводная цифра не мешает открыть исследование: сбой чтения даёт null.
	var coverage any
	if c, ok, err := s.St.FeedbackCoverage(r.Context(), pid); err != nil {
		slog.Warn("отзывы во входе синтеза не прочитаны", "project", pid, "err", err)
	} else if ok && c.Total > 0 {
		coverage = c
	}
	cur, err := s.St.CurrentRun(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	runs, err := s.St.ListRunSummaries(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	versions := make([]discRunSummaryView, 0, len(runs))
	for _, run := range runs {
		versions = append(versions, discRunSummaryDTO(run))
	}
	var current any
	if cur != nil {
		current = discRunDTO(*cur)
	}
	writeJSON(w, http.StatusOK, map[string]any{"interviews": ivs, "feedback": feedback, "feedback_unaccounted": unaccounted,
		"feedback_coverage": coverage, "current": current, "versions": versions})
}

func (s *Server) getInterview(w http.ResponseWriter, r *http.Request) {
	pid, iid := r.PathValue("id"), r.PathValue("iid")
	if !pathIDs(w, pid, iid) || !s.requireMember(w, r, pid) {
		return
	}
	iv, err := s.St.GetInterview(r.Context(), pid, iid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, iv)
}

func (s *Server) getAnalysis(w http.ResponseWriter, r *http.Request) {
	pid, rid := r.PathValue("id"), r.PathValue("rid")
	if !pathIDs(w, pid, rid) || !s.requireMember(w, r, pid) {
		return
	}
	run, err := s.St.GetRun(r.Context(), pid, rid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, discRunDTO(run))
}

// ─── интервью ────────────────────────────────────────────────────────────

// addInterview — POST /projects/{id}/discovery/interviews: JSON или
// multipart с файлом .md/.txt.
func (s *Server) addInterview(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	if !pathIDs(w, pid) || !s.requireOwner(w, r, pid) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInterviewBody)
	var in store.NewInterview
	fromFile := false
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxInterviewBody); err != nil {
			s.bodyErr(w, err)
			return
		}
		in.Title, in.RespondentRole, in.HeldAt = r.FormValue("title"), r.FormValue("respondent_role"), r.FormValue("held_at")
		f, hdr, err := r.FormFile("file")
		if err != nil {
			discErr(w, http.StatusBadRequest, "invalid", "приложите файл интервью", "file", nil)
			return
		}
		defer f.Close()
		ext := strings.ToLower(filepath.Ext(hdr.Filename))
		if ext != ".md" && ext != ".txt" && ext != ".markdown" {
			discErr(w, http.StatusBadRequest, "invalid", "интервью загружается файлом .md или .txt", "file", nil)
			return
		}
		raw, err := io.ReadAll(f)
		if err != nil {
			s.bodyErr(w, err)
			return
		}
		in.Text = string(raw)
		if strings.TrimSpace(in.Title) == "" {
			in.Title = defaultInterviewTitle(in.Text, hdr.Filename)
		}
		fromFile = true
	} else {
		var body struct {
			Title          string `json:"title"`
			RespondentRole string `json:"respondent_role"`
			HeldAt         string `json:"held_at"`
			Text           string `json:"text"`
		}
		if err := decodeJSONLimit(r, &body); err != nil {
			s.bodyErr(w, err)
			return
		}
		in = store.NewInterview{Title: body.Title, RespondentRole: body.RespondentRole, HeldAt: body.HeldAt, Text: body.Text}
	}
	iv, err := s.St.AddInterview(r.Context(), pid, currentUser(r).ID, in)
	var field *store.DiscoveryFieldError
	if fromFile && errors.As(err, &field) && field.Field == "text" {
		field.Field = "file"
	}
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	s.discoveryEvent(r.Context(), r, pid, "discovery.interview_added",
		"добавлено интервью «"+iv.Title+"»", map[string]any{"interview_id": iv.ID, "title": iv.Title})
	writeJSON(w, http.StatusCreated, iv)
}

// defaultInterviewTitle — название файла без явного: первый заголовок
// Markdown в начале текста, иначе имя файла без расширения.
func defaultInterviewTitle(text, filename string) string {
	for i, line := range strings.Split(text, "\n") {
		if i >= 20 {
			break
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if h, ok := strings.CutPrefix(line, "# "); ok && strings.TrimSpace(h) != "" {
			if t := []rune(strings.TrimSpace(h)); len(t) > 200 {
				return string(t[:200])
			}
			return strings.TrimSpace(h)
		}
	}
	return strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
}

// decodeJSONLimit — JSON тела, уже ограниченного MaxBytesReader.
func decodeJSONLimit(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// bodyErr — тело запроса не прочитано: превышение размера — 413.
func (s *Server) bodyErr(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeDiscoveryErr(w, store.ErrInterviewTooLarge)
		return
	}
	discErr(w, http.StatusBadRequest, "invalid", "тело запроса не разобрано: "+err.Error(), "", nil)
}

func (s *Server) deleteInterview(w http.ResponseWriter, r *http.Request) {
	pid, iid := r.PathValue("id"), r.PathValue("iid")
	if !pathIDs(w, pid, iid) || !s.requireOwner(w, r, pid) {
		return
	}
	iv, err := s.St.DeleteInterview(r.Context(), pid, iid)
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	text := "удалено интервью «" + iv.Title + "»"
	if iv.Kind == store.MaterialFeedback {
		text = "удалён отзыв: " + iv.Source
	}
	s.discoveryEvent(r.Context(), r, pid, "discovery.interview_deleted", text,
		map[string]any{"interview_id": iv.ID, "title": iv.Title, "kind": iv.Kind})
	w.WriteHeader(http.StatusNoContent)
}

// maxFeedbackBody — предел тела отзыва: 2000 символов текста и 200
// источника по 4 байта и запас на JSON.
const maxFeedbackBody = 64 << 10

// addFeedback — POST /projects/{id}/discovery/feedback: отзыв пользователя
// добавляет любой участник проекта (спека product-discovery «Обратная
// связь пользователей»).
func (s *Server) addFeedback(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	if !pathIDs(w, pid) || !s.requireMember(w, r, pid) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackBody)
	var body struct {
		Text   string `json:"text"`
		Source string `json:"source"`
	}
	if err := decodeJSONLimit(r, &body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			discErr(w, http.StatusBadRequest, "invalid", "отзыв длиннее 2000 символов", "text", nil)
			return
		}
		discErr(w, http.StatusBadRequest, "invalid", "тело запроса не разобрано: "+err.Error(), "", nil)
		return
	}
	fb, err := s.St.AddFeedback(r.Context(), pid, currentUser(r).ID, store.NewFeedback{Text: body.Text, Source: body.Source})
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	s.discoveryEvent(r.Context(), r, pid, "feedback.created", "отзыв пользователя: "+fb.Source,
		map[string]any{"material_id": fb.ID, "source": fb.Source})
	writeJSON(w, http.StatusCreated, fb)
}

// discoveryEvent — событие исследования от имени пользователя запроса.
func (s *Server) discoveryEvent(ctx context.Context, r *http.Request, projectID, typ, text string, payload map[string]any) {
	if _, err := s.St.AppendEvent(context.WithoutCancel(ctx), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: typ, ProjectID: projectID, Text: text, Payload: payload,
	}); err != nil {
		slog.Error("событие исследования", "type", typ, "err", err)
	}
}

// ─── анализ ──────────────────────────────────────────────────────────────

// materialsText — состав анализа: «интервью 2, отзывы 3».
func materialsText(ms []store.DiscoveryInterview) string {
	interviews, feedback := 0, 0
	for _, m := range ms {
		if m.Kind == store.MaterialFeedback {
			feedback++
		} else {
			interviews++
		}
	}
	switch {
	case feedback == 0:
		return fmt.Sprintf("интервью %d", interviews)
	case interviews == 0:
		return fmt.Sprintf("отзывы %d", feedback)
	}
	return fmt.Sprintf("интервью %d, отзывы %d", interviews, feedback)
}

// analystModelError — модель Аналитика не настроена или недоступна (412).
type analystModelError struct {
	code, msg string
}

func (e *analystModelError) Error() string { return e.msg }

var errAnalystDisabled = errors.New("шаблон роли «Аналитик» отключён: включите его в разделе ролей")

// analystSetup — кто и на какой модели анализирует.
type analystSetup struct {
	agentID string
	who     discovery.Analyst
	model   analyst.Model
}

// resolveAnalyst выбирает агента и модель по спеке: первый включённый
// агент роли analyst с инструкцией шаблона и дополнением агента; модель
// агента, иначе шаблона, иначе Тимлида, иначе окружения установки.
func (s *Server) resolveAnalyst(ctx context.Context) (analystSetup, error) {
	var out analystSetup
	role, err := s.St.GetRole(ctx, domain.RoleAnalyst)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !role.Enabled) {
		return out, errAnalystDisabled
	}
	if err != nil {
		return out, err
	}
	out.who = discovery.Analyst{Title: role.Title, Instruction: role.Instruction}
	var ref *domain.AgentModelRef
	agent, ok, err := s.St.FirstTeamAgent(ctx, domain.RoleAnalyst)
	if err != nil {
		return out, err
	}
	if ok {
		out.agentID, out.who.Name, out.who.Extra = agent.ID, agent.Name, agent.InstructionExtra
		if agent.ConnectionID != "" && agent.Model != "" {
			ref = &domain.AgentModelRef{ConnectionID: agent.ConnectionID, Model: agent.Model}
		}
	}
	if ref == nil && role.DefaultModel != nil && role.DefaultModel.ConnectionID != "" && role.DefaultModel.Model != "" {
		ref = role.DefaultModel
	}
	if ref == nil {
		pm, ok, err := s.St.PlannerModel(ctx)
		if err != nil {
			return out, err
		}
		if ok {
			ref = &domain.AgentModelRef{ConnectionID: pm.ConnectionID, Model: pm.Model}
		}
	}
	if ref != nil {
		cl, c, err := s.St.ConnectionClient(ctx, ref.ConnectionID, s.Secrets)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return out, &analystModelError{"model_invalid", "подключение " + ref.ConnectionID + " удалено"}
		case errors.Is(err, store.ErrSecret), errors.Is(err, secretbox.ErrNoKey):
			return out, &analystModelError{"model_invalid", err.Error()}
		case err != nil:
			return out, err
		case !c.Enabled:
			return out, &analystModelError{"model_invalid", "подключение " + c.ID + " отключено"}
		case c.State == domain.LLMStateInvalid:
			return out, &analystModelError{"model_invalid", "ключ подключения " + c.ID + " отклонён: " + c.CheckDetail}
		}
		out.model = analyst.Model{Client: cl, Model: ref.Model, ConnectionID: c.ID}
		for _, m := range c.Models {
			if m.ID == ref.Model {
				out.model.InputPrice, out.model.OutputPrice = m.InputPrice, m.OutputPrice
			}
		}
		return out, nil
	}
	env := s.EnvPlanner
	if env.Provider != "" && env.Key != "" {
		model := env.Model
		if model == "" {
			model = planner.DefaultModel(env.Provider)
		}
		var cl llm.Client
		switch env.Provider {
		case "anthropic":
			cl = llm.Client{API: llm.APIAnthropic, BaseURL: llm.DefaultBaseURL(llm.APIAnthropic), Key: env.Key}
		case "deepseek":
			cl = llm.Client{API: llm.APIOpenAI, BaseURL: "https://api.deepseek.com", Key: env.Key}
		default:
			return out, &analystModelError{"model_invalid", "неизвестный провайдер модели " + env.Provider}
		}
		out.model = analyst.Model{Client: cl, Model: model, ConnectionID: env.Provider}
		return out, nil
	}
	return out, &analystModelError{"model_not_configured",
		"модель Аналитика не настроена: выберите модель агенту или роли, либо настройте модель Тимлида"}
}

// startAnalysis — POST /projects/{id}/discovery/analyses: версия в
// running, работа в фоне, ответ 202.
func (s *Server) startAnalysis(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	if !pathIDs(w, pid) || !s.requireOwner(w, r, pid) {
		return
	}
	ctx := r.Context()
	// Анализ идёт по всем материалам: интервью и отзывам пользователей.
	has, err := s.St.HasMaterials(ctx, pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !has {
		writeDiscoveryErr(w, store.ErrNoInterviews)
		return
	}
	// Запросы Аналитика расходуют дневной бюджет проекта: при исчерпанном
	// бюджете анализ не запускается, как не назначаются стадии задач.
	if b, err := s.St.ProjectBudget(ctx, pid, time.Now()); err != nil {
		writeErr(w, err)
		return
	} else if b.PausedUntil != nil {
		scope := "проекта"
		if b.PausedScope == store.PolicyScopeInstallation {
			scope = "установки"
		}
		discErr(w, http.StatusConflict, "budget_exceeded", "дневной бюджет токенов "+scope+" исчерпан: анализ можно запустить после "+
			b.PausedUntil.Format("02.01.2006 15:04")+" UTC", "", map[string]any{"paused_until": b.PausedUntil})
		return
	}
	setup, err := s.resolveAnalyst(ctx)
	var modelErr *analystModelError
	switch {
	case errors.Is(err, errAnalystDisabled):
		discErr(w, http.StatusConflict, "analyst_disabled", err.Error(), "", nil)
		return
	case errors.As(err, &modelErr):
		discErr(w, http.StatusPreconditionFailed, modelErr.code, modelErr.msg, "", nil)
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	// Набор интервью читается под замком проекта вместе с созданием
	// версии: удалённое одновременно интервью в версию не попадёт.
	run, texts, err := s.St.StartRun(ctx, pid, store.NewRun{
		AgentID: setup.agentID, AgentName: setup.who.Name, RoleTitle: setup.who.Title,
		Model: setup.model.Model, ConnectionID: setup.model.ConnectionID, InstructionSHA: setup.who.InstructionSHA(),
	})
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	who := setup.who.Title
	if setup.who.Name != "" {
		who += " " + setup.who.Name
	}
	if _, err := s.St.AppendEvent(context.WithoutCancel(ctx), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "discovery.analysis_started", ProjectID: pid,
		Text:    who + " начинает анализ: " + materialsText(texts),
		Payload: analyst.Payload(run, setup.agentID, setup.who),
	}); err != nil {
		slog.Error("событие старта анализа", "run", run.ID, "err", err)
	}
	job := analyst.Job{Run: run, AgentID: setup.agentID, Analyst: setup.who, Model: setup.model, Interviews: texts}
	go func() {
		bg := context.Background()
		// Паника до старта анализа (чтение README) тоже завершает версию:
		// running не залипает до перезапуска.
		defer func() {
			if p := recover(); p != nil {
				slog.Error("паника запуска анализа", "run", run.ID, "panic", p, "stack", string(debug.Stack()))
				if ok, err := s.St.FinishRunFailed(bg, run.ID, fmt.Sprintf("внутренняя ошибка анализа: %v", p), nil, nil); err == nil && ok {
					s.analysisFailedEvent(bg, run, setup.agentID, setup.who, fmt.Sprintf("внутренняя ошибка анализа: %v", p))
				}
			}
		}()
		job.Context = s.productContext(bg, pid)
		s.analystRunner().Run(bg, job)
	}()
	writeJSON(w, http.StatusAccepted, discRunDTO(run))
}

func (s *Server) analystRunner() *analyst.Runner {
	if s.Analyst != nil {
		return s.Analyst
	}
	return &analyst.Runner{St: s.St}
}

// productContext — README основной ветки и сводка снимка архитектуры для
// синтеза. Ошибка хостинга и отсутствие корректного описания не мешают
// анализу: задание явно называет, чего нет.
func (s *Server) productContext(ctx context.Context, projectID string) discovery.ProductContext {
	var pc discovery.ProductContext
	pc.Architecture = s.architectureContext(ctx, projectID)
	if s.Engine == nil {
		return pc
	}
	p, err := s.St.GetProject(ctx, projectID)
	if err != nil {
		return pc
	}
	adapter, err := s.Engine.SCMFor(ctx, p)
	if err != nil {
		slog.Warn("README для анализа: хостинг недоступен", "project", projectID, "err", err)
		return pc
	}
	ref := p.DefaultBranch
	if ref == "" {
		ref = s.Engine.BaseBranch
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, name := range []string{"README.md", "readme.md", "README"} {
		f, err := adapter.ReadFile(rctx, p.Repo(), ref, name)
		if err == nil {
			pc.README = f.Content
			break
		}
	}
	return pc
}

// architectureContext — сводка корректного снимка архитектуры проекта:
// при ok текущей версии, при invalid и error последней корректной, с
// пометкой, что текущее описание не прошло проверку или не прочитано.
// Снимка или корректной версии нет — пусто.
func (s *Server) architectureContext(ctx context.Context, projectID string) string {
	row, ok, err := s.St.GetProjectArchitecture(ctx, projectID)
	if err != nil {
		slog.Warn("снимок архитектуры для анализа", "project", projectID, "err", err)
		return ""
	}
	if !ok || len(row.ValidData) == 0 || string(row.ValidData) == "null" {
		return ""
	}
	var d architecture.SnapshotData
	if err := json.Unmarshal(row.ValidData, &d); err != nil {
		slog.Warn("снимок архитектуры для анализа не разобран", "project", projectID, "err", err)
		return ""
	}
	out := architecture.ContextSummary(d)
	if row.Status != "ok" {
		sha := row.ValidSHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		out = fmt.Sprintf("Последняя корректная версия описания (коммит %s): текущее описание на основной ветке не прошло проверку или не прочитано.\n\n", sha) + out
	}
	return out
}

// RecoverDiscovery — при старте rivetd: анализы, прерванные перезапуском,
// становятся failed с событием, незаконченные декомпозиции бэклога — тоже.
func (s *Server) RecoverDiscovery(ctx context.Context) error {
	runs, err := s.St.FailInterruptedRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		s.analysisFailedEvent(ctx, run, run.AgentID, discovery.Analyst{Title: run.RoleTitle, Name: run.AgentName},
			"анализ прерван перезапуском системы")
	}
	_, err = s.St.FailInterruptedDecompositions(ctx)
	return err
}

// analysisFailedEvent — событие сбоя анализа с агентом, ролью и моделью.
func (s *Server) analysisFailedEvent(ctx context.Context, run store.DiscoveryRun, agentID string, who discovery.Analyst, reason string) {
	payload := analyst.Payload(run, agentID, who)
	payload["error"] = reason
	if _, err := s.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "discovery.analysis_failed", ProjectID: run.ProjectID,
		Text: "анализ не завершён: " + reason, Payload: payload,
	}); err != nil {
		slog.Error("событие сбоя анализа", "run", run.ID, "err", err)
	}
}

// ─── бэклог ──────────────────────────────────────────────────────────────

func (s *Server) putBacklog(w http.ResponseWriter, r *http.Request) {
	pid, rid := r.PathValue("id"), r.PathValue("rid")
	if !pathIDs(w, pid, rid) || !s.requireOwner(w, r, pid) {
		return
	}
	var in struct {
		BacklogRev int                     `json:"backlog_rev"`
		Epics      []discovery.BacklogEpic `json:"epics"`
	}
	if err := decode(r, &in); err != nil {
		discErr(w, http.StatusBadRequest, "invalid", "тело запроса не разобрано: "+err.Error(), "", nil)
		return
	}
	run, err := s.St.SaveBacklog(r.Context(), pid, rid, in.BacklogRev, in.Epics)
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	s.discoveryEvent(r.Context(), r, pid, "discovery.backlog_edited",
		fmt.Sprintf("бэклог версии %d изменён", run.Version),
		map[string]any{"run_id": run.ID, "version": run.Version, "backlog_rev": run.BacklogRev})
	writeJSON(w, http.StatusOK, discRunDTO(run))
}

func (s *Server) approveBacklog(w http.ResponseWriter, r *http.Request) {
	pid, rid := r.PathValue("id"), r.PathValue("rid")
	if !pathIDs(w, pid, rid) || !s.requireOwner(w, r, pid) {
		return
	}
	var in struct {
		BacklogRev int  `json:"backlog_rev"`
		Decompose  bool `json:"decompose"`
	}
	if err := decode(r, &in); err != nil {
		discErr(w, http.StatusBadRequest, "invalid", "тело запроса не разобрано: "+err.Error(), "", nil)
		return
	}
	res, err := s.St.ApproveRun(r.Context(), pid, rid, currentUser(r).ID, in.BacklogRev, in.Decompose)
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	created := 0
	ids := map[string]string{}
	for _, e := range res.Epics {
		ids[e.Key] = e.EpicID
		if e.Action == "created" {
			created++
		}
	}
	s.discoveryEvent(r.Context(), r, pid, "discovery.approved",
		fmt.Sprintf("бэклог версии %d утверждён: создано Epic'ов %d, связано %d", res.Run.Version, created, len(res.Epics)-created),
		map[string]any{"run_id": res.Run.ID, "epics": res.Epics})
	// Связанным Epic'ам проекта утверждение добавило зависимости: правка
	// видна так же, как правка на дашборде.
	for _, id := range res.DepsChanged {
		s.depsChangedEvent(r, id)
	}
	if in.Decompose && len(res.Order) > 0 {
		go s.decomposeBacklog(res.Run.ID, res.Order, ids)
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": discRunDTO(res.Run), "epics": res.Epics})
}

// ─── зависимости Epic'а ──────────────────────────────────────────────────

func (s *Server) putEpicDeps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !pathIDs(w, id) {
		return
	}
	epic, err := s.St.EpicForViewer(r.Context(), id, currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !s.requireOwner(w, r, epic.ProjectID) {
		return
	}
	var in struct {
		DependsOn []string `json:"depends_on"`
	}
	if err := decode(r, &in); err != nil {
		discErr(w, http.StatusBadRequest, "invalid", "тело запроса не разобрано: "+err.Error(), "depends_on", nil)
		return
	}
	e, err := s.St.SetEpicDeps(r.Context(), id, in.DependsOn)
	if err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	list := []domain.Epic{e}
	if err := s.St.EnrichEpics(r.Context(), list); err != nil {
		writeErr(w, err)
		return
	}
	e = list[0]
	s.writeDepsChanged(r, e)
	writeJSON(w, http.StatusOK, e)
}

// depsChangedEvent — событие правки зависимостей Epic'а по его id.
func (s *Server) depsChangedEvent(r *http.Request, epicID string) {
	ctx := context.WithoutCancel(r.Context())
	e, err := s.St.GetEpic(ctx, epicID)
	if err != nil {
		slog.Error("событие зависимостей Epic'а", "epic", epicID, "err", err)
		return
	}
	list := []domain.Epic{e}
	if err := s.St.EnrichEpics(ctx, list); err != nil {
		slog.Error("событие зависимостей Epic'а", "epic", epicID, "err", err)
		return
	}
	s.writeDepsChanged(r, list[0])
}

func (s *Server) writeDepsChanged(r *http.Request, e domain.Epic) {
	waiting := make([]string, 0, len(e.WaitingFor))
	for _, ref := range e.WaitingFor {
		waiting = append(waiting, ref.ID)
	}
	text := "зависимости Epic'а сняты"
	if len(e.DependsOn) > 0 {
		text = fmt.Sprintf("зависимости Epic'а изменены: %d", len(e.DependsOn))
	}
	if _, err := s.St.AppendEvent(context.WithoutCancel(r.Context()), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "epic.deps_changed", ProjectID: e.ProjectID, EpicID: e.ID,
		Text: text, Payload: map[string]any{"epic_id": e.ID, "depends_on": e.DependsOn, "waiting_for": waiting},
	}); err != nil {
		slog.Error("событие зависимостей Epic'а", "epic", e.ID, "err", err)
	}
}
