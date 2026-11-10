package store

import (
	"context"
	"testing"
)

// Запоздалая 0038 должна расширять CHECK, сохраняя причины и предметы
// более новых изменений, уже применённых на стенде.
func TestCredentialMigrationPreservesHostSubject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `ALTER TABLE attention ADD COLUMN host_id uuid;
 ALTER TABLE attention DROP CONSTRAINT attention_subject;
 ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK (
 (task_id IS NOT NULL AND deployment_id IS NULL) OR
 (task_id IS NULL AND deployment_id IS NOT NULL) OR
 (task_id IS NULL AND deployment_id IS NULL AND host_id IS NOT NULL));
 ALTER TABLE scm_credentials DROP COLUMN state_reason, DROP COLUMN state_message, DROP COLUMN recheck_at;
 ALTER TABLE tasks DROP COLUMN wait_target,DROP COLUMN wait_entry,DROP COLUMN wait_claimed_at,DROP COLUMN wait_failures;`); err != nil {
		t.Fatal(err)
	}
	sql, err := migrationsFS.ReadFile("migrations/0038_scm_credential_state.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "migration", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO attention(project_id,host_id,reason,message) VALUES($1,gen_random_uuid(),'HOST_FAILED','x')`, p.ID); err != nil {
		t.Fatalf("предмет хоста потерян: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO attention(project_id,reason,message) VALUES($1,'SCM_CREDENTIALS','x')`, p.ID); err != nil {
		t.Fatalf("причина учётных данных потеряна: %v", err)
	}
}
