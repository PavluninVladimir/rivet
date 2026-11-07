package store

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/discovery"
)

// Отзывы пользователей (add-user-feedback, спека product-discovery
// «Обратная связь пользователей»): материал исследования вида feedback.
// Хранится, нормализуется и нумеруется как интервью, источник лежит в
// respondent_role, название — «Отзыв · <источник>».

// NewFeedback — добавляемый отзыв: текст и источник.
type NewFeedback struct {
	Text   string
	Source string
}

// FeedbackTitle — название отзыва по источнику.
func FeedbackTitle(source string) string { return "Отзыв · " + source }

// AddFeedback добавляет отзыв участника проекта. Текст нормализуется как
// у интервью, источник сводится к одной строке. Число отзывов считается
// под замком проекта, что и у интервью: параллельные добавления не
// превысят лимит, а лимит интервью отзывы не трогают.
func (s *Store) AddFeedback(ctx context.Context, projectID, userID string, in NewFeedback) (DiscoveryInterview, error) {
	// Поля проверяются по порядку формы: сначала текст, затем источник.
	for _, f := range []struct{ field, v string }{{"text", in.Text}, {"source", in.Source}} {
		if strings.ContainsRune(f.v, 0) {
			return DiscoveryInterview{}, &DiscoveryFieldError{Field: f.field, Msg: "текст содержит нулевые символы"}
		}
		if !utf8.ValidString(f.v) {
			return DiscoveryInterview{}, &DiscoveryFieldError{Field: f.field, Msg: "текст не в кодировке UTF-8"}
		}
	}
	// Текст хранится как есть после нормализации, как у интервью.
	text := discovery.NormalizeText(in.Text)
	if strings.TrimSpace(text) == "" {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "text", Msg: "пустой отзыв"}
	}
	chars := utf8.RuneCountInString(text)
	if chars > MaxFeedbackChars {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "text", Msg: "отзыв длиннее 2000 символов"}
	}
	source := strings.Join(strings.Fields(discovery.NormalizeText(in.Source)), " ")
	if source == "" {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "source", Msg: "укажите источник отзыва, например «менеджер Анна, чат поддержки»"}
	}
	if utf8.RuneCountInString(source) > MaxFeedbackSource {
		return DiscoveryInterview{}, &DiscoveryFieldError{Field: "source", Msg: "источник длиннее 200 символов"}
	}
	var out DiscoveryInterview
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := lockProjectDiscovery(ctx, tx, projectID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM discovery_interviews
			WHERE project_id=$1 AND deleted_at IS NULL AND kind='feedback'`, projectID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxFeedback {
			return ErrFeedbackLimit
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO discovery_interviews (project_id, kind, title, respondent_role, text, sha256, chars, lines, created_by)
			VALUES ($1, 'feedback', $2, $3, $4, $5, $6, $7, NULLIF($8,'')::uuid) RETURNING id`,
			projectID, FeedbackTitle(source), source, text, discovery.TextSHA256(text), chars, discovery.CountLines(text), userID).Scan(&id); err != nil {
			return err
		}
		var err error
		out, err = scanInterview(tx.QueryRow(ctx, `SELECT `+interviewCols+`, i.text FROM discovery_interviews i
			LEFT JOIN users u ON u.id = i.created_by WHERE i.id=$1`, id), true)
		return err
	})
	return out, err
}

// UnaccountedFeedback — число отзывов проекта, которых нет в наборе
// последней готовой версии исследования (последней версии с результатом
// анализа). Так же считает элементы фазы «Исследование» агрегат цикла.
func (s *Store) UnaccountedFeedback(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		WITH last AS (
			SELECT interviews FROM discovery_runs
			WHERE project_id=$1 AND result IS NOT NULL ORDER BY version DESC LIMIT 1)
		SELECT count(*) FROM discovery_interviews i
		WHERE i.project_id=$1 AND i.deleted_at IS NULL AND i.kind='feedback'
		  AND NOT EXISTS (SELECT 1 FROM last WHERE last.interviews @> jsonb_build_array(jsonb_build_object('id', i.id::text)))`,
		projectID).Scan(&n)
	return n, err
}

// HasMaterials — есть ли в проекте неудалённые интервью или отзывы: анализ
// идёт по всем материалам проекта.
func (s *Store) HasMaterials(ctx context.Context, projectID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM discovery_interviews WHERE project_id=$1 AND deleted_at IS NULL)`,
		projectID).Scan(&ok)
	return ok, err
}

// SetRunFeedbackCoverage — сколько отзывов в версии, сколько из них вошло
// во вход синтеза и сколько не дали подтверждённых находок.
func (s *Store) SetRunFeedbackCoverage(ctx context.Context, runID string, total, synthesized, withoutFindings int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE discovery_runs SET feedback_total=$2, feedback_synthesized=$3, feedback_without_findings=$4
		WHERE id=$1`, runID, total, synthesized, withoutFindings)
	return err
}

// FeedbackCoverageStat — отзывы последней готовой версии исследования.
type FeedbackCoverageStat struct {
	Total           int `json:"total"`
	Synthesized     int `json:"synthesized"`
	WithoutFindings int `json:"without_findings"`
	Trimmed         int `json:"trimmed"`
}

// FeedbackCoverage — отзывы последней готовой версии исследования: сколько
// их в версии, сколько вошло во вход синтеза, сколько без находок и
// сколько убрано сокращением входа. ok=false — готовой версии нет или она
// построена без этих сведений.
func (s *Store) FeedbackCoverage(ctx context.Context, projectID string) (FeedbackCoverageStat, bool, error) {
	var t, n, k *int
	err := s.Pool.QueryRow(ctx, `
		SELECT feedback_total, feedback_synthesized, feedback_without_findings FROM discovery_runs
		WHERE project_id=$1 AND result IS NOT NULL ORDER BY version DESC LIMIT 1`, projectID).Scan(&t, &n, &k)
	if errors.Is(err, pgx.ErrNoRows) {
		return FeedbackCoverageStat{}, false, nil
	}
	if err != nil || t == nil || n == nil {
		return FeedbackCoverageStat{}, false, err
	}
	c := FeedbackCoverageStat{Total: *t, Synthesized: *n}
	if k != nil {
		c.WithoutFindings = *k
	}
	c.Trimmed = max(c.Total-c.Synthesized-c.WithoutFindings, 0)
	return c, true, nil
}
