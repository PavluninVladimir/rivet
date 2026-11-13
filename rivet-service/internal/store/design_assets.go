package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/jackc/pgx/v5"
)

func (s *Store) DesignAssetReadURL(ctx context.Context, pid, actor, id string) (DesignAsset, error) {
	var a DesignAsset
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := designRoleTx(ctx, tx, pid, actor); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id::text,sha256,mime,size_bytes,object_ref FROM design_assets WHERE project_id=$1 AND id=$2`, pid, id).Scan(&a.ID, &a.SHA256, &a.MIME, &a.SizeBytes, &a.ObjectRef); err != nil {
			return nf(err)
		}
		// Expired capabilities are purged in bounded batches, not on a timer.
		if _, err := tx.Exec(ctx, `DELETE FROM design_asset_reads WHERE token_sha IN (SELECT token_sha FROM design_asset_reads WHERE expires_at<now() LIMIT 100)`); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM design_asset_reads WHERE project_id=$1 AND expires_at>now()`, pid).Scan(&count); err != nil {
			return err
		}
		if count >= 10000 {
			return design.Invalid("rate_limit", "assets", "слишком много активных ссылок")
		}
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		plain := hex.EncodeToString(token)
		sum := sha256.Sum256([]byte(plain))
		if _, err := tx.Exec(ctx, `INSERT INTO design_asset_reads(token_sha,asset_id,project_id,actor_id,expires_at) VALUES($1,$2,$3,$4,$5)`, hex.EncodeToString(sum[:]), id, pid, actor, time.Now().Add(5*time.Minute)); err != nil {
			return err
		}
		a.URL = "/api/v1/design-assets/" + plain
		return nil
	})
	return a, err
}
func (s *Store) DesignResolveAssetRead(ctx context.Context, token string) (DesignAsset, error) {
	var a DesignAsset
	if len(token) != 64 {
		return a, ErrNotFound
	}
	if _, err := hex.DecodeString(token); err != nil {
		return a, ErrNotFound
	}
	sum := sha256.Sum256([]byte(token))
	err := s.Pool.QueryRow(ctx, `SELECT a.id::text,a.sha256,a.mime,a.size_bytes,a.object_ref FROM design_asset_reads c JOIN design_assets a ON a.id=c.asset_id AND a.project_id=c.project_id JOIN project_members m ON m.project_id=c.project_id AND m.user_id=c.actor_id JOIN users u ON u.id=c.actor_id WHERE c.token_sha=$1 AND c.expires_at>now() AND NOT u.disabled`, hex.EncodeToString(sum[:])).Scan(&a.ID, &a.SHA256, &a.MIME, &a.SizeBytes, &a.ObjectRef)
	return a, nf(err)
}
