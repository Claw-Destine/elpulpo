package usage

import (
	"context"
	"time"
)

// Prune deletes usage rows older than cutoff (unix ms UTC), in batches of
// 5000 so it never holds one long write lock. It returns how many rows were
// removed. Reference prices, configuration and exported CSVs are not part of
// this database's reach — pruning here only ever touches rows.
func (r *Repo) Prune(ctx context.Context, cutoffMs int64) (int64, error) {
	var total int64
	for {
		res, err := r.db.ExecContext(ctx,
			`DELETE FROM requests WHERE id IN (SELECT id FROM requests WHERE ts_ms < ? LIMIT 5000)`, cutoffMs)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return total, nil
		}
		total += n
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// PruneOlderThan prunes rows older than n days from now.
func (r *Repo) PruneOlderThan(ctx context.Context, days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
	return r.Prune(ctx, cutoff)
}
