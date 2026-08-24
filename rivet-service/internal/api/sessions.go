// Сессии задачи и транскрипты (api-contract add-session-visibility).
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/store"
)

// sessionView — DTO Session контракта: stage нормализована, tokens nullable
// (null = источник не сообщил), длительность клиент считает сам.
type sessionView struct {
	ID         string `json:"id"`
	Attempt    int    `json:"attempt"`
	Stage      string `json:"stage"`
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	DriverKind string `json:"driver_kind"`
	// Depth — глубина данных подключения; Files — затронутые файлы:
	// null = недоступно для этого способа подключения, [] = полная
	// глубина без файлов (api-contract add-claude-code-adapter).
	Depth         string     `json:"depth"`
	Files         []string   `json:"files"`
	DriverID      string     `json:"driver_id"`
	Private       bool       `json:"private"`
	Prompt        string     `json:"prompt"`
	Outcome       string     `json:"outcome"`
	LastStep      string     `json:"last_step"`
	Tokens        *int64     `json:"tokens"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
	HasTranscript bool       `json:"has_transcript"`
}

// stageName нормализует внутреннее представление стадии (protobuf-enum в
// верхнем регистре из sessions.scope) в значения контракта: FIXING → fix,
// неизвестное значение — как есть в нижнем регистре.
func stageName(scope string) string {
	switch scope {
	case "CODING":
		return "coding"
	case "TESTING":
		return "testing"
	case "REVIEW":
		return "review"
	case "FIXING":
		return "fix"
	}
	return strings.ToLower(scope)
}

// GET /api/v1/tasks/{id}/sessions — история сессий задачи (дельта
// observability «Просмотр сохранённых транскриптов»).
func (s *Server) listTaskSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	sessions, err := s.St.ListTaskSessions(r.Context(), r.PathValue("id"), user(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]sessionView, 0, len(sessions)) // пустая история — [], не null
	for _, v := range sessions {
		out = append(out, sessionView{
			ID: v.ID, Attempt: v.Attempt, Stage: stageName(v.Scope),
			Agent: v.Agent, Model: v.Model, DriverKind: v.DriverKind,
			Depth: string(v.Depth), Files: v.Files,
			DriverID: v.DriverID, Private: v.Private,
			Prompt: v.Prompt, Outcome: v.Outcome, LastStep: v.LastStep,
			Tokens: v.Tokens, StartedAt: v.Started, EndedAt: v.Ended,
			HasTranscript: v.TranscriptRef != "",
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/v1/projects/{id}/sessions — реестр активных сессий проекта и
// поиск по истории (api-contract add-team-visibility): без q — активные с
// пересечениями, с q — история по ключевым словам. Стадия нормализуется
// как в sessionView.
func (s *Server) projectSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var limit int
	_, _ = fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	var (
		out []store.SessionEntry
		err error
	)
	if q == "" {
		out, err = s.St.ActiveProjectSessions(r.Context(), r.PathValue("id"), user(r))
	} else {
		out, err = s.St.SearchProjectSessions(r.Context(), r.PathValue("id"), q, user(r), limit)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	for i := range out {
		out[i].Stage = stageName(out[i].Stage)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/v1/tasks/{id}/sessions — сессия доработки с промптом участника
// (api-contract add-user-sessions): blocked/failed/review → fixing на
// свободном runner'е, попытка не расходуется.
func (s *Server) startTaskSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireTaskMember(w, r, r.PathValue("id")) {
		return
	}
	var in struct {
		Prompt  string `json:"prompt"`
		Private bool   `json:"private"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Prompt) == "" {
		unprocessable(w, "нужен prompt сессии")
		return
	}
	sessionID, err := s.Engine.StartUserSession(r.Context(), r.PathValue("id"), strings.TrimSpace(in.Prompt), user(r), in.Private)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "fixing", "session_id": sessionID})
}

// GET /api/v1/sessions/{id}/transcript — сохранённый транскрипт сессии.
// 404 не различает: нет сессии, viewer не участник, транскрипт не сохранён,
// объект недоступен, blob-хранилище выключено (api-contract).
func (s *Server) sessionTranscript(w http.ResponseWriter, r *http.Request) {
	ref, err := s.St.SessionTranscriptForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ref == "" || s.Blob == nil {
		writeErr(w, store.ErrNotFound)
		return
	}
	data, err := s.Blob.Get(r.Context(), ref)
	if err != nil {
		slog.Error("transcript get", "session", r.PathValue("id"), "ref", ref, "err", err)
		writeErr(w, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
