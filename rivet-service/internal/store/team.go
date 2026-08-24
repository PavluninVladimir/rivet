package store

import (
	"context"
	"strings"
	"time"
)

// Командная видимость (change add-team-visibility, спека team-visibility):
// реестр активных сессий проекта, поиск по истории, пересечения работ.

// SessionEntry — запись реестра/истории сессий проекта (api-contract).
type SessionEntry struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"task_id"`
	TaskNum       int64      `json:"task_num"`
	TaskTitle     string     `json:"task_title"`
	EpicID        string     `json:"epic_id"`
	Stage         string     `json:"stage"`
	DriverKind    string     `json:"driver_kind"`
	DriverID      string     `json:"driver_id"`
	Agent         string     `json:"agent"`
	Model         string     `json:"model"`
	Depth         string     `json:"depth"`
	Prompt        string     `json:"prompt"`
	Outcome       string     `json:"outcome"`
	LastStep      string     `json:"last_step"`
	Files         []string   `json:"files"`
	Tokens        *int64     `json:"tokens"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
	HasTranscript bool       `json:"has_transcript"`
	// Overlaps — только у активных: nil — пересечения недоступны для
	// способа подключения (минимальная глубина), [] — пересечений нет.
	Overlaps []Overlap `json:"overlaps"`
}

// Overlap — пересечение работ с другой задачей (общие затронутые файлы).
type Overlap struct {
	TaskID    string   `json:"task_id"`
	TaskNum   int64    `json:"task_num"`
	TaskTitle string   `json:"task_title"`
	Files     []string `json:"files"`
}

const sessionEntryCols = `
	ss.id::text, ss.task_id::text, t.num, t.title, t.epic_id::text,
	COALESCE(ss.scope,''), ss.driver_kind, ss.driver_id, ss.agent, ss.model, ss.depth,
	ss.prompt, ss.outcome, ss.last_step, ss.files, ss.tokens,
	ss.started_at, ss.ended_at, COALESCE(ss.transcript_ref,'') <> ''`

func scanSessionEntries(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]SessionEntry, error) {
	defer rows.Close()
	out := []SessionEntry{}
	for rows.Next() {
		var e SessionEntry
		if err := rows.Scan(&e.ID, &e.TaskID, &e.TaskNum, &e.TaskTitle, &e.EpicID,
			&e.Stage, &e.DriverKind, &e.DriverID, &e.Agent, &e.Model, &e.Depth,
			&e.Prompt, &e.Outcome, &e.LastStep, &e.Files, &e.Tokens,
			&e.StartedAt, &e.EndedAt, &e.HasTranscript); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActiveProjectSessions — реестр активных сессий проекта (по возрастанию
// started_at) с пересечениями по затронутым файлам между активными.
func (s *Store) ActiveProjectSessions(ctx context.Context, projectID string) ([]SessionEntry, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+sessionEntryCols+`
		FROM sessions ss
		JOIN tasks t ON t.id = ss.task_id
		JOIN epics e ON e.id = t.epic_id
		WHERE e.project_id = $1 AND ss.ended_at IS NULL
		ORDER BY ss.started_at`, projectID)
	if err != nil {
		return nil, err
	}
	entries, err := scanSessionEntries(rows)
	if err != nil {
		return nil, err
	}
	// Пересечения между активными считаются по уже выбранным записям:
	// активных сессий единицы (по числу runner'ов), пары дешевле в памяти.
	for i := range entries {
		if entries[i].Files == nil {
			continue // минимальная глубина: пересечения недоступны (nil)
		}
		entries[i].Overlaps = []Overlap{}
		for j := range entries {
			if i == j || entries[j].Files == nil || entries[i].TaskID == entries[j].TaskID {
				continue
			}
			common := intersect(entries[i].Files, entries[j].Files, 20)
			if len(common) == 0 {
				continue
			}
			entries[i].Overlaps = append(entries[i].Overlaps, Overlap{
				TaskID: entries[j].TaskID, TaskNum: entries[j].TaskNum,
				TaskTitle: entries[j].TaskTitle, Files: common,
			})
		}
	}
	return entries, nil
}

// SearchProjectSessions — история сессий проекта по ключевым словам:
// FTS (russian) по запросу и итогу плюс название задачи (design «Поиск»).
func (s *Store) SearchProjectSessions(ctx context.Context, projectID, q string, limit int) ([]SessionEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+sessionEntryCols+`
		FROM sessions ss
		JOIN tasks t ON t.id = ss.task_id
		JOIN epics e ON e.id = t.epic_id
		WHERE e.project_id = $1
		  AND (to_tsvector('russian', ss.prompt || ' ' || ss.outcome) @@ websearch_to_tsquery('russian', $2)
		       OR t.title ILIKE '%' || $3 || '%' ESCAPE '\')
		ORDER BY ss.started_at DESC
		LIMIT $4`, projectID, q, escapeLike(q), limit)
	if err != nil {
		return nil, err
	}
	return scanSessionEntries(rows)
}

// OverlappingSessions — активные сессии других задач проекта, чьи
// затронутые файлы пересекаются с paths (пересечения работ: событие обеим
// сторонам пишет вызывающий).
type OverlapHit struct {
	SessionID string
	TaskID    string
	TaskNum   int64
	Files     []string
}

func (s *Store) OverlappingSessions(ctx context.Context, sessionID string, paths []string) (self OverlapHit, hits []OverlapHit, err error) {
	if len(paths) == 0 {
		return self, nil, nil
	}
	// Сессия-источник: задача и проект.
	var projectID string
	if err := s.Pool.QueryRow(ctx, `
		SELECT ss.id::text, ss.task_id::text, t.num, e.project_id::text
		FROM sessions ss JOIN tasks t ON t.id = ss.task_id JOIN epics e ON e.id = t.epic_id
		WHERE ss.id = $1`, sessionID).
		Scan(&self.SessionID, &self.TaskID, &self.TaskNum, &projectID); err != nil {
		return self, nil, nf(err)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT ss.id::text, ss.task_id::text, t.num, ss.files
		FROM sessions ss
		JOIN tasks t ON t.id = ss.task_id
		JOIN epics e ON e.id = t.epic_id
		WHERE e.project_id = $1 AND ss.ended_at IS NULL
		  AND ss.task_id <> $2::uuid AND ss.files && $3::text[]`,
		projectID, self.TaskID, paths)
	if err != nil {
		return self, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h OverlapHit
		var files []string
		if err := rows.Scan(&h.SessionID, &h.TaskID, &h.TaskNum, &files); err != nil {
			return self, nil, err
		}
		h.Files = intersect(files, paths, 20)
		hits = append(hits, h)
	}
	return self, hits, rows.Err()
}

// escapeLike экранирует метасимволы LIKE в пользовательском запросе:
// «%»/«_» в q — буквальные символы, а не шаблон.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// intersect — общие элементы a и b в порядке a, не больше limit.
func intersect(a, b []string, limit int) []string {
	set := make(map[string]bool, len(b))
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if set[x] && len(out) < limit {
			out = append(out, x)
		}
	}
	return out
}

// SessionProjectEpic — проект и epic задачи сессии (атрибуция события).
func (s *Store) SessionProjectEpic(ctx context.Context, sessionID string) (projectID, epicID, taskID string, err error) {
	err = nf(s.Pool.QueryRow(ctx, `
		SELECT e.project_id::text, t.epic_id::text, t.id::text
		FROM sessions ss JOIN tasks t ON t.id = ss.task_id JOIN epics e ON e.id = t.epic_id
		WHERE ss.id = $1`, sessionID).Scan(&projectID, &epicID, &taskID))
	return
}
