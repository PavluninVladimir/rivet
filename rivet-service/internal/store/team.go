package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
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
	Private       bool       `json:"private"`
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
	ss.started_at, ss.ended_at, COALESCE(ss.transcript_ref,'') <> '', ss.private`

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
			&e.StartedAt, &e.EndedAt, &e.HasTranscript, &e.Private); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// maskPrivateEntry скрывает содержимое чужой приватной сессии: команде
// остаётся факт — водитель, задача, стадия, время (спека team-visibility
// «Видимость по умолчанию и приватность»).
func maskPrivateEntry(e *SessionEntry, viewerLogin string) {
	if !e.Private || e.DriverID == viewerLogin {
		return
	}
	e.Prompt, e.Outcome, e.LastStep = "", "", ""
	e.Files = nil
	e.HasTranscript = false
	e.Overlaps = nil
}

// ActiveProjectSessions — реестр активных сессий проекта (по возрастанию
// started_at) с пересечениями по затронутым файлам между активными; чужие
// приватные показываются фактом, их файлы в пересечениях не участвуют.
func (s *Store) ActiveProjectSessions(ctx context.Context, projectID, viewerLogin string) ([]SessionEntry, error) {
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
		if entries[i].Files == nil || entries[i].Private {
			continue // минимальная глубина: пересечения недоступны (nil); приватные не участвуют
		}
		entries[i].Overlaps = []Overlap{}
		for j := range entries {
			if i == j || entries[j].Files == nil || entries[j].Private || entries[i].TaskID == entries[j].TaskID {
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
	for i := range entries {
		maskPrivateEntry(&entries[i], viewerLogin)
	}
	return entries, nil
}

// SearchProjectSessions — история сессий проекта по ключевым словам:
// FTS (russian) по запросу и итогу плюс название задачи (design «Поиск»).
// Чужие приватные сессии поиск не возвращает вовсе: поиск по скрытому
// тексту был бы утечкой через подбор запросов.
func (s *Store) SearchProjectSessions(ctx context.Context, projectID, q, viewerLogin string, limit int) ([]SessionEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+sessionEntryCols+`
		FROM sessions ss
		JOIN tasks t ON t.id = ss.task_id
		JOIN epics e ON e.id = t.epic_id
		WHERE e.project_id = $1
		  AND (NOT ss.private OR ss.driver_id = $5)
		  AND (to_tsvector('russian', ss.prompt || ' ' || ss.outcome) @@ websearch_to_tsquery('russian', $2)
		       OR t.title ILIKE '%' || $3 || '%' ESCAPE '\')
		ORDER BY ss.started_at DESC
		LIMIT $4`, projectID, q, escapeLike(q), limit, viewerLogin)
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
		WHERE e.project_id = $1 AND ss.ended_at IS NULL AND NOT ss.private
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

// SessionPrivacy — приватность и водитель сессии (фильтрация шагов и
// live-потока; приватность неизменна после создания — кэшируемо).
func (s *Store) SessionPrivacy(ctx context.Context, sessionID string) (private bool, driverID string, err error) {
	err = nf(s.Pool.QueryRow(ctx,
		`SELECT private, driver_id FROM sessions WHERE id=$1`, sessionID).Scan(&private, &driverID))
	return
}

// SessionProjectEpic — проект и epic задачи сессии (атрибуция события).
func (s *Store) SessionProjectEpic(ctx context.Context, sessionID string) (projectID, epicID, taskID string, err error) {
	err = nf(s.Pool.QueryRow(ctx, `
		SELECT e.project_id::text, t.epic_id::text, t.id::text
		FROM sessions ss JOIN tasks t ON t.id = ss.task_id JOIN epics e ON e.id = t.epic_id
		WHERE ss.id = $1`, sessionID).Scan(&projectID, &epicID, &taskID))
	return
}

// SessionRunner — runner, выполняющий сессию, и поддержка им обратного
// канала контекста (спека agent-integration «Обратный канал контекста»).
// Runner выбирается по стадии сессии: review исполняет ревьюер, остальные
// стадии — исполнитель задачи (runner_id на задаче во время review ещё
// хранит прошлого исполнителя, и предупреждение ушло бы не тому агенту).
// Пустой runnerID — стадия завершилась или сессия уже закрыта: доставлять
// контекст некому.
func (s *Store) SessionRunner(ctx context.Context, sessionID string) (runnerID string, contextChannel bool, err error) {
	err = nf(s.Pool.QueryRow(ctx, `
		SELECT COALESCE(stage.runner_id, ''), COALESCE(r.context_channel, false)
		FROM sessions ss
		JOIN tasks t ON t.id = ss.task_id
		CROSS JOIN LATERAL (
			SELECT CASE WHEN ss.scope = 'REVIEW' THEN t.reviewer_id ELSE t.runner_id END AS runner_id
		) stage
		LEFT JOIN runners r ON r.id = stage.runner_id
		WHERE ss.id = $1 AND ss.ended_at IS NULL`, sessionID).Scan(&runnerID, &contextChannel))
	if errors.Is(err, ErrNotFound) {
		// Сессия закрыта или удалена — не ошибка обработки шага.
		return "", false, nil
	}
	return runnerID, contextChannel, err
}

// ─── сессия доработки (change add-user-sessions) ─────────────────────────

// ErrNoRunner — нет свободного runner'а с нужными capabilities: запуск
// сессии доработки отклоняется сразу, а не ждёт молча (спека
// agent-integration «Свободного runner'а нет»).
var ErrNoRunner = errors.New("нет свободного runner'а с нужными capabilities")

// StartUserSession атомарно готовит задачу к сессии доработки: статус
// blocked|failed|review → fixing, свободный runner захватывается
// (SKIP LOCKED), для blocked/failed счётчики сбрасываются и эскалации
// закрываются (вмешательство человека, семантика ResolveTask), событие
// пишется от пользователя. Возвращает назначение для dispatch.
func (s *Store) StartUserSession(ctx context.Context, taskID, login string, private bool) (Assignment, error) {
	var a Assignment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		var epicID, projectID string
		if err := tx.QueryRow(ctx, `
			SELECT t.status, t.epic_id, e.project_id FROM tasks t
			JOIN epics e ON e.id=t.epic_id WHERE t.id=$1 FOR UPDATE OF t`, taskID).
			Scan(&from, &epicID, &projectID); err != nil {
			return nf(err)
		}
		switch from {
		case domain.TaskBlocked, domain.TaskFailed, domain.TaskReview:
		default:
			return domain.ErrBadTransition{Entity: "task", From: string(from), To: string(domain.TaskFixing)}
		}
		var runnerID string
		err := tx.QueryRow(ctx, `
			SELECT r.id FROM runners r
			WHERE r.status='idle' AND NOT r.draining
			  AND r.capabilities @> (SELECT capabilities FROM tasks WHERE id=$1)
			ORDER BY cardinality(r.capabilities), r.last_seen DESC
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1`, taskID).Scan(&runnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoRunner
		}
		if err != nil {
			return err
		}
		if err := endOpenSessions(ctx, tx, taskID); err != nil {
			return err
		}
		// Вмешательство человека: из blocked/failed — как решение
		// (сброс счётчиков, закрытие эскалаций); из review счётчики не трогаем.
		reset := from == domain.TaskBlocked || from == domain.TaskFailed
		if reset {
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET attempt_used=0, review_rejections=0,
					block_reason=NULL, blocked_by=NULL WHERE id=$1`, taskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE attention SET status='resolved', resolved_at=now()
				WHERE task_id=$1 AND status <> 'resolved'`, taskID); err != nil {
				return err
			}
		}
		// Ревьюер, если review шёл прямо сейчас, освобождается — иначе он
		// навсегда остался бы занятым (reviewer_id обнуляется ниже).
		if _, err := tx.Exec(ctx, `
			UPDATE runners SET status='idle', task_id=NULL
			WHERE id = (SELECT reviewer_id FROM tasks WHERE id=$1)`, taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status='fixing', runner_id=$2, reviewer_id=NULL, updated_at=now()
			WHERE id=$1`, taskID, runnerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE runners SET status='running', task_id=$2, ctx_pct=NULL WHERE id=$1`, runnerID, taskID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: login, Type: "task.status",
			ProjectID: projectID, EpicID: epicID, TaskID: taskID,
			Text:    "сессия доработки запущена участником " + login,
			Payload: map[string]any{"status": "fixing", "session": "user", "private": private},
		}); err != nil {
			return err
		}
		a.Task.ID, a.Task.EpicID = taskID, epicID
		a.Runner.ID = runnerID
		return nil
	})
	if err != nil {
		return a, err
	}
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return a, err
	}
	a.Task = t
	return a, nil
}
