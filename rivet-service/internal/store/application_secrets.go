package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/jackc/pgx/v5"
)

const LocalDockerSecretsFeature = "local_docker_secrets_v1"

// ApplicationSecret exposes no prefix, length, digest or encrypted value.
type ApplicationSecret struct {
	Name      string    `json:"name"`
	Masked    string    `json:"masked"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ApplicationSecretPatch struct {
	Set    map[string]string `json:"set"`
	Delete []string          `json:"delete"`
}

func (s *Store) ApplicationSecrets(ctx context.Context, envID string) ([]ApplicationSecret, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name,updated_at FROM environment_secrets WHERE env_id=$1 ORDER BY name`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApplicationSecret{}
	for rows.Next() {
		var v ApplicationSecret
		if err := rows.Scan(&v.Name, &v.UpdatedAt); err != nil {
			return nil, err
		}
		v.Masked = "***"
		out = append(out, v)
	}
	return out, rows.Err()
}

func secretField(msg string) error { return &domain.EnvFieldError{Field: "secrets", Msg: msg} }

func (s *Store) PatchApplicationSecrets(ctx context.Context, envID string, in ApplicationSecretPatch, box *secretbox.Box, actorID string) error {
	if len(in.Set) > 64 || len(in.Delete) > 64 {
		return secretField("допустимо не больше 64 секретов")
	}
	for k, v := range in.Set {
		if !domain.ValidApplicationEnvName(k) {
			return secretField("недопустимое или зарезервированное имя переменной")
		}
		if len(v) == 0 || len(v) > 4096 || strings.ContainsAny(v, "\x00\r\n") {
			return secretField("нужно непустое значение одной строкой до 4096 байт")
		}
	}
	seen := map[string]bool{}
	for _, k := range in.Delete {
		if !domain.ValidApplicationEnvName(k) || seen[k] {
			return secretField("недопустимое или повторное имя удаления")
		}
		if _, ok := in.Set[k]; ok {
			return secretField("нельзя одновременно записать и удалить одно имя")
		}
		seen[k] = true
	}
	if len(in.Set) > 0 && !box.Enabled() {
		return secretbox.ErrNoKey
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		env, err := scanEnv(tx.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id=$1 FOR UPDATE`, envID))
		if err != nil {
			return nf(err)
		}
		if env.ExecType != domain.ExecLocalDocker {
			return ErrNotFound
		}
		for k := range in.Set {
			if _, ok := env.Config.LocalDocker.Env[k]; ok {
				return secretField("имя уже используется публичной переменной")
			}
		}
		for _, k := range in.Delete {
			if _, err = tx.Exec(ctx, `DELETE FROM environment_secrets WHERE env_id=$1 AND name=$2`, envID, k); err != nil {
				return err
			}
		}
		for k, v := range in.Set {
			enc, err := box.Seal(v)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO environment_secrets(env_id,name,value_enc) VALUES($1,$2,$3)
 ON CONFLICT(env_id,name) DO UPDATE SET value_enc=EXCLUDED.value_enc,updated_at=now()`, envID, k, enc); err != nil {
				return err
			}
		}
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM environment_secrets WHERE env_id=$1`, envID).Scan(&count); err != nil {
			return err
		}
		if count > 64 {
			return secretField("допустимо не больше 64 секретов")
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorID, Type: "environment.secrets.changed", ProjectID: env.ProjectID,
			Text: "секреты приложения изменены", Payload: map[string]any{"environment_id": envID}})
		return err
	})
}

// localSecretCollision serializes public-config writes with secret writes and snapshots.
func localSecretCollision(ctx context.Context, tx pgx.Tx, env domain.Environment) error {
	if env.Config.LocalDocker == nil {
		return nil
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM environments WHERE id=$1 FOR UPDATE`, env.ID).Scan(&id); err != nil {
		return nf(err)
	}
	keys := make([]string, 0, len(env.Config.LocalDocker.Env))
	for k := range env.Config.LocalDocker.Env {
		keys = append(keys, k)
	}
	var collision bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_secrets WHERE env_id=$1 AND name=ANY($2::text[]))`, env.ID, keys).Scan(&collision); err != nil {
		return err
	}
	if collision {
		return secretField("имя уже используется секретной переменной")
	}
	return nil
}

var ErrSecretAssignment = errors.New("секреты доступны только назначенному защищённому Docker runner-у")

// LocalSecretsForRunner is the only decryption path. Ownership and live lease
// are checked again immediately before delivery, never in a public read DTO.
func (s *Store) LocalSecretsForRunner(ctx context.Context, depID, runnerID string, box *secretbox.Box, includeValues bool) (map[string]string, bool, error) {
	var out map[string]string
	sensitive := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var pubID string
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT p.id::text, r.secure AND r.capabilities @> ARRAY['deploy','docker']
 AND cardinality(r.capabilities)=2 AND r.features @> ARRAY['local_docker_secrets_v1']
 FROM local_publications p JOIN deployments d ON d.id=p.active_deployment_id
 JOIN runners r ON r.id=d.runner_id AND r.deployment_id=d.id
 WHERE d.id=$1 AND d.runner_id=$2 AND (p.runner_id='' OR p.runner_id=$2) AND d.status='deploying'
 FOR SHARE OF d,r,p`, depID, runnerID).Scan(&pubID, &allowed)
		if err != nil {
			return nf(err)
		}
		rows, err := tx.Query(ctx, `SELECT name,value_enc FROM local_publication_secrets WHERE publication_id=$1 ORDER BY name`, pubID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = map[string]string{}
		for rows.Next() {
			sensitive = true
			if !allowed {
				return ErrSecretAssignment
			}
			if !includeValues {
				continue
			}
			var name string
			var enc []byte
			if err := rows.Scan(&name, &enc); err != nil {
				return err
			}
			v, err := box.Open(enc)
			if err != nil {
				return errors.New("секрет публикации не расшифрован")
			}
			out[name] = v
		}
		return rows.Err()
	})
	if err != nil {
		return nil, sensitive, err
	}
	return out, sensitive, nil
}

// DeploymentHasApplicationSecrets needs no key and fails closed at output boundaries.
func (s *Store) DeploymentHasApplicationSecrets(ctx context.Context, depID string) (bool, error) {
	var yes bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_publication_secrets ps JOIN local_publications p ON p.id=ps.publication_id WHERE p.active_deployment_id=$1 OR p.deployment_id=$1)`, depID).Scan(&yes)
	return yes, err
}
