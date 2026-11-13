package store

import (
	"context"
)

// PendingLocalTerminals recovers a crash between accepted terminal result and
// deployment finalization. Unknown/progress results are never synthesized.
func (s *Store) PendingLocalTerminals(ctx context.Context) ([]LocalPublication, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+localCols+` FROM local_publications p WHERE EXISTS(SELECT 1 FROM deployments d WHERE d.id=p.active_deployment_id AND d.ended_at IS NULL) AND p.completed_operation_id=p.active_deployment_id AND p.operation='publish' AND p.status IN('failed','running') ORDER BY p.finished_at,p.id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalPublication
	for rows.Next() {
		p, err := scanLocal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
