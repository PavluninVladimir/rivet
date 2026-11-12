package store

import (
	"context"
	"time"

	"github.com/PavluninVladimir/rivet/internal/cycle"
	"github.com/jackc/pgx/v5"
)

func loadCycleAssessments(ctx context.Context, tx pgx.Tx, pid string, from time.Time, in *cycle.Input) error {
	var count, done int
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='passed' AND finished_at>=$2) FROM epic_architecture_assessments WHERE project_id=$1`, pid, from).Scan(&count, &done); err != nil {
		return err
	}
	if count > 0 {
		in.Available[cycle.Architecture] = true
	}
	in.DonePeriod[cycle.Architecture] += done
	rows, err := tx.Query(ctx, `SELECT a.epic_id,e.title,a.finished_at FROM epic_architecture_assessments a JOIN epics e ON e.id=a.epic_id
		WHERE a.project_id=$1 AND a.status='passed' ORDER BY a.finished_at DESC LIMIT $2`, pid, cycle.MaxArtifacts)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		a := cycle.Artifact{Phase: cycle.Architecture, Kind: "assessment", Ref: cycle.Ref{View: "epic"}}
		if err = rows.Scan(&a.ID, &a.Title, &a.At); err != nil {
			return err
		}
		a.Title = "Оценка: " + a.Title
		a.Ref.ID = a.ID
		in.Artifacts = append(in.Artifacts, a)
	}
	return rows.Err()
}

func cycleAssessmentAgents(ctx context.Context, tx queryer) (map[string]*cycle.Work, error) {
	rows, err := tx.Query(ctx, `SELECT a.agent_id,a.epic_id,a.project_id,e.title FROM epic_architecture_assessments a JOIN epics e ON e.id=a.epic_id
		WHERE a.lease_until>now() AND a.agent_id<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*cycle.Work{}
	for rows.Next() {
		var id string
		w := &cycle.Work{Kind: "epic", Action: "оценивает архитектурное влияние", Phase: cycle.Architecture}
		if err = rows.Scan(&id, &w.ID, &w.ProjectID, &w.Title); err != nil {
			return nil, err
		}
		out[id] = w
	}
	return out, rows.Err()
}

func (s *Store) ActiveAssessmentWork(ctx context.Context) (CycleBusy, error) {
	work, err := cycleAssessmentAgents(ctx, s.Pool)
	return CycleBusy(work), err
}
