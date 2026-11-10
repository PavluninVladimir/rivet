package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
	"strings"
)

// Результаты review хранятся с поколением шага; внешняя правка плана их не принимает.
type CriterionResult struct {
	Index int
	OK    bool
	Note  string
}
type CriteriaReport struct {
	Pass    int
	By      string
	Results []CriterionResult
}

func (s *Store) RecordCriteriaResults(ctx context.Context, taskID string, report CriteriaReport) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return recordCriteriaResultsTx(ctx, tx, taskID, report) })
}

func recordCriteriaResultsTx(ctx context.Context, tx pgx.Tx, taskID string, report CriteriaReport) error {
	var raw []byte
	var gen, closed int
	if err := tx.QueryRow(ctx, `SELECT criteria,step_gen,step_closed_gen FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&raw, &gen, &closed); err != nil {
		return nf(err)
	}
	if report.Pass != gen || closed >= report.Pass {
		return nil
	}
	var criteria []domain.Criterion
	if err := json.Unmarshal(raw, &criteria); err != nil {
		return err
	}
	// Новый проход заменяет весь отчёт, включая критерии без новых отметок.
	for i := range criteria {
		if criteria[i].Pass < report.Pass {
			criteria[i] = domain.Criterion{Text: criteria[i].Text}
		}
	}
	for _, r := range report.Results {
		if r.Index < 1 || r.Index > len(criteria) {
			continue
		}
		c := &criteria[r.Index-1]
		if c.Pass > report.Pass {
			continue
		}
		if c.Pass < report.Pass {
			c.OK = false
			c.Status = ""
			c.Note = ""
			c.By = ""
		}
		if r.OK && c.Status == "fail" {
			continue
		}
		c.OK, c.By, c.Pass = r.OK, report.By, report.Pass
		c.Status, c.Note = "ok", ""
		if !r.OK {
			c.Status, c.Note = "fail", r.Note
		}
	}
	raw, err := json.Marshal(criteria)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET criteria=$2,updated_at=now() WHERE id=$1`, taskID, raw)
	return err
}

// ClaimReviewOutcome сверяет FAIL и закрепляет исход под одной блокировкой.
// RecordVerdict с отчётом берёт ту же блокировку и не пишет закрытый проход.
type ReviewOutcome struct {
	Claimed bool
	Outcome string
	Detail  string
}

func (s *Store) ClaimReviewOutcome(ctx context.Context, taskID string, pass int, outcome string) (ReviewOutcome, error) {
	result := ReviewOutcome{Outcome: outcome}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var raw []byte
		var gen, closed int
		if err := tx.QueryRow(ctx, `SELECT criteria,step_gen,step_closed_gen FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&raw, &gen, &closed); err != nil {
			return nf(err)
		}
		if gen != pass || closed >= pass {
			return nil
		}
		var criteria []domain.Criterion
		if err := json.Unmarshal(raw, &criteria); err != nil {
			return err
		}
		var failed []string
		for i, c := range criteria {
			if c.Pass == pass && c.Status == "fail" {
				failed = append(failed, fmt.Sprintf("%d — %s", i+1, c.Note))
			}
		}
		if len(failed) > 0 {
			result.Detail = "Не выполнены критерии: " + strings.Join(failed, "; ")
			if result.Outcome == "ok" {
				result.Outcome = "changes"
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET step_closed_gen=$2 WHERE id=$1`, taskID, pass); err != nil {
			return err
		}
		result.Claimed = true
		return nil
	})
	return result, err
}
