package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	// База часовых поясов внутри бинаря: в образе alpine системной нет, и
	// без неё любой пояс, кроме UTC, давал бы 422.
	_ "time/tzdata"

	"github.com/PavluninVladimir/rivet/internal/overview"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Обзор проекта для руководителя и еженедельный отчёт (add-project-lead-view,
// api-contract «GET /projects/{id}/overview» и «GET /projects/{id}/report»).

// fieldInvalid — 422 с указанием поля (форма ошибок как у ролей и политик).
func fieldInvalid(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error": map[string]any{"code": "invalid", "message": msg, "field": field}, "field": field,
	})
}

// overviewTZ — пояс запроса: имя IANA, пусто — UTC. «Local» не принимается:
// это пояс сервера, а не клиента.
func overviewTZ(w http.ResponseWriter, r *http.Request) (*time.Location, bool) {
	name := strings.TrimSpace(r.URL.Query().Get("tz"))
	if name == "" {
		return time.UTC, true
	}
	if name == "Local" {
		fieldInvalid(w, "tz", "неизвестный часовой пояс: укажите имя IANA, например Europe/Moscow")
		return nil, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		fieldInvalid(w, "tz", "неизвестный часовой пояс "+name)
		return nil, false
	}
	return loc, true
}

func (s *Server) getProjectOverview(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	loc, ok := overviewTZ(w, r)
	if !ok {
		return
	}
	now := time.Now()
	per, err := overview.NewPeriod(r.URL.Query().Get("period"), now, loc)
	if err != nil {
		fieldInvalid(w, "period", err.Error())
		return
	}
	u := currentUser(r)
	in, err := s.St.OverviewInput(r.Context(), store.OverviewQuery{
		ProjectID: projectID, UserID: u.ID, Login: u.Login, Now: now, Period: per, Loc: loc,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, overview.Build(in))
}

func (s *Server) getProjectReport(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	loc, ok := overviewTZ(w, r)
	if !ok {
		return
	}
	// now берётся один раз: от него считается to по умолчанию и признак
	// текущего отчёта (design, решение 7).
	now := time.Now()
	q := r.URL.Query()
	to := now.Truncate(time.Minute)
	live := true
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fieldInvalid(w, "to", "to: ожидается время RFC 3339")
			return
		}
		to, live = t, !t.Before(now)
	}
	from := to.Add(-7 * 24 * time.Hour)
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fieldInvalid(w, "from", "from: ожидается время RFC 3339")
			return
		}
		from = t
	}
	per, err := overview.CustomPeriod(from, to, loc)
	if err != nil {
		field := "period"
		if errors.Is(err, overview.ErrPeriod) && !from.Before(to) {
			field = "from"
		}
		fieldInvalid(w, field, err.Error())
		return
	}
	in, err := s.St.OverviewInput(r.Context(), store.OverviewQuery{
		ProjectID: projectID, Now: now, Period: per, Loc: loc, Report: true, Live: live,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	body := overview.Report(in)
	name := fmt.Sprintf("rivet-%s-%s-%s.md", projectSlug(in.ProjectName, projectID),
		per.From.In(loc).Format("2006-01-02"), per.To.Add(-time.Nanosecond).In(loc).Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// translit — латиница для русских букв в имени файла отчёта.
var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z",
	'и': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r",
	'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// projectSlug — имя проекта для имени файла: строчная латиница, цифры и
// дефисы. Пустой результат заменяется началом идентификатора проекта.
func projectSlug(name, id string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		var part string
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			part = string(r)
		default:
			if t, ok := translit[r]; ok {
				part = t
			}
		}
		if part == "" {
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
			continue
		}
		b.WriteString(part)
		dash = false
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = id
		if len(slug) > 8 {
			slug = slug[:8]
		}
	}
	return slug
}
