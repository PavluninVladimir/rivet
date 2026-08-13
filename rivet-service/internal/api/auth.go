package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Аутентификация и первые два слоя авторизации (спека access-policy,
// design add-users-and-access, решения 4, 9–12).

const sessionCookie = "rivet_session"

type ctxKey int

const userKey ctxKey = iota

// currentUser — идентичность запроса, положенная middleware.
func currentUser(r *http.Request) domain.User {
	u, _ := r.Context().Value(userKey).(domain.User)
	return u
}

// user — login текущего пользователя для атрибуции событий.
func user(r *http.Request) string { return currentUser(r).Login }

func unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized,
		map[string]apiError{"error": {Code: "unauthorized", Message: "требуется аутентификация"}})
}

func forbidden(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]apiError{"error": {Code: "forbidden", Message: msg}})
}

// authExempt — endpoints, живущие до идентичности: health, вход и webhook
// (у него собственная HMAC-подпись, спека scm-integration).
func authExempt(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/health", "/api/v1/auth/login", "/api/v1/webhooks/github":
		return true
	}
	return false
}

// withAuth аутентифицирует запрос: cookie сессии, затем Bearer-PAT.
// Мутации с cookie дополнительно проверяются на Origin (CSRF, решение 12).
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		var u domain.User
		var err = store.ErrNotFound
		viaCookie := false
		if c, cerr := r.Cookie(sessionCookie); cerr == nil && c.Value != "" {
			u, err = s.St.UserBySession(r.Context(), c.Value)
			viaCookie = err == nil
		}
		if err != nil {
			if secret := bearerSecret(r.Header.Get("Authorization")); secret != "" {
				u, err = s.St.UserByAccessToken(r.Context(), secret)
			}
		}
		if err != nil {
			unauthorized(w)
			return
		}
		if viaCookie && !crossSiteSafe(r) {
			forbidden(w, "запрос отклонён CSRF-защитой (Origin не совпадает)")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// bearerSecret извлекает секрет из "Authorization: Bearer <секрет>".
// Возвращает пустую строку на любой другой формы заголовок.
func bearerSecret(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// crossSiteSafe — валидация происхождения мутаций с cookie-аутентификацией:
// браузер шлёт Origin на POST; чужой origin отклоняется. Запросы без Origin
// (curl и пр.) пропускаются — cookie у них не берётся из браузерного контекста.
func crossSiteSafe(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
	}
	return ok && rest == r.Host
}

// ─── защита от перебора (design, решение 10) ─────────────────────────────

type loginThrottle struct {
	mu    sync.Mutex
	fails map[string]*throttleEntry
}

type throttleEntry struct {
	fails int
	until time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{fails: map[string]*throttleEntry{}}
}

const throttleFreeAttempts = 5

// locked — активна ли задержка по ключу (login или ip).
func (t *loginThrottle) locked(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fails[key]
	return e != nil && time.Now().Before(e.until)
}

// fail фиксирует неверную попытку: после пяти ошибок — экспоненциальное
// окно 2^n секунд с потолком в минуту.
func (t *loginThrottle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fails[key]
	if e == nil {
		e = &throttleEntry{}
		t.fails[key] = e
	}
	e.fails++
	if over := e.fails - throttleFreeAttempts; over >= 0 {
		delay := min(time.Duration(1<<min(over, 6))*time.Second, time.Minute)
		e.until = time.Now().Add(delay)
	}
}

func (t *loginThrottle) reset(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		delete(t.fails, k)
	}
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}

// ─── auth endpoints ──────────────────────────────────────────────────────

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil || in.Login == "" || in.Password == "" {
		unprocessable(w, "нужны login и password")
		return
	}
	lKey, ipKey := "l:"+in.Login, "ip:"+clientIP(r)
	if s.throttle.locked(lKey) || s.throttle.locked(ipKey) {
		// Окно задержки: не тратим bcrypt, ответ неотличим от неверного пароля.
		unauthorized(w)
		return
	}
	u, err := s.St.Authenticate(r.Context(), in.Login, in.Password)
	if err != nil {
		s.throttle.fail(lKey)
		s.throttle.fail(ipKey)
		unauthorized(w)
		return
	}
	s.throttle.reset(lKey, ipKey)
	secret, err := s.St.CreateAuthSession(r.Context(), u.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	http.SetCookie(w, s.sessionCookie(r, secret, int(store.SessionTTL.Seconds())))
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.St.DeleteAuthSession(r.Context(), c.Value); err != nil {
			writeErr(w, err)
			return
		}
	}
	http.SetCookie(w, s.sessionCookie(r, "", -1))
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

// sessionCookie строит cookie сессии; Secure — за TLS или доверенным прокси
// с X-Forwarded-Proto: https (design, решение 12).
func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	secure := r.TLS != nil || (s.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
	return &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge,
	}
}

// ─── users (только администратор) ────────────────────────────────────────

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !currentUser(r).Admin {
		forbidden(w, "требуются права администратора")
		return false
	}
	return true
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out, err := s.St.ListUsers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Login    string `json:"login"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	if err := decode(r, &in); err != nil || in.Login == "" || in.Password == "" {
		unprocessable(w, "нужны login и password")
		return
	}
	u, err := s.St.CreateUser(r.Context(), in.Login, in.Name, in.Password, in.Admin)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "user.created",
		Text: "создан пользователь " + u.Login,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Name     *string `json:"name"`
		Disabled *bool   `json:"disabled"`
	}
	if err := decode(r, &in); err != nil || (in.Name == nil && in.Disabled == nil) {
		unprocessable(w, "нужны name и/или disabled")
		return
	}
	u, err := s.St.SetUserState(r.Context(), r.PathValue("id"), in.Name, in.Disabled)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// ─── участники проекта ───────────────────────────────────────────────────

// requireMember — слой видимости: не-участник получает 404, существование
// проекта не раскрывается. Обхода для администратора нет (спека domain-model).
func (s *Server) requireMember(w http.ResponseWriter, r *http.Request, projectID string) bool {
	ok, err := s.St.IsMember(r.Context(), projectID, currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if !ok {
		writeErr(w, store.ErrNotFound)
		return false
	}
	return true
}

// requireEpicMember — членство через проект Epic'а.
func (s *Server) requireEpicMember(w http.ResponseWriter, r *http.Request, epicID string) bool {
	e, err := s.St.GetEpic(r.Context(), epicID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	return s.requireMember(w, r, e.ProjectID)
}

// requireTaskMember — членство через проект задачи.
func (s *Server) requireTaskMember(w http.ResponseWriter, r *http.Request, taskID string) bool {
	projectID, _, err := s.St.TaskRefs(r.Context(), taskID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	return s.requireMember(w, r, projectID)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	out, err := s.St.ListMembers(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	var in struct {
		Login string `json:"login"`
	}
	if err := decode(r, &in); err != nil || in.Login == "" {
		unprocessable(w, "нужен login")
		return
	}
	if err := s.St.AddMember(r.Context(), projectID, in.Login); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "project.member_added",
		ProjectID: projectID, Text: "добавлен участник " + in.Login,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "added"})
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	login := r.PathValue("login")
	if err := s.St.RemoveMember(r.Context(), projectID, login); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "project.member_removed",
		ProjectID: projectID, Text: "удалён участник " + login,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// ─── personal access tokens ──────────────────────────────────────────────

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	out, err := s.St.ListAccessTokens(r.Context(), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		unprocessable(w, "нужен name")
		return
	}
	t, secret, err := s.St.CreateAccessToken(r.Context(), currentUser(r).ID, in.Name, in.ExpiresAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Секрет существует только в этом ответе (api-contract).
	writeJSON(w, http.StatusCreated, map[string]any{"token": t, "secret": secret})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.St.DeleteAccessToken(r.Context(), currentUser(r).ID, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
