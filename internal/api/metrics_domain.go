package api

import "context"

// refreshCASBytes recomputes the pakman_cas_bytes gauge from the DB.
// Called after any mutation that adds or removes package/binary rows.
//
// The query is a single scan of two small tables plus aggregate sums;
// cheap at v1 scale. A pathological-scale installation (100k+ packages)
// could move this to a periodic goroutine instead; we deliberately
// haven't until there's evidence it matters.
//
// Errors are swallowed on purpose. Metrics are best-effort and a
// mid-request DB blip shouldn't fail the user-facing operation.
func refreshCASBytes(ctx context.Context, deps Deps) {
	if deps.Metrics == nil || deps.DB == nil {
		return
	}
	var total int64
	err := deps.DB.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT SUM(source_size) FROM packages), 0) +
		       COALESCE((SELECT SUM(size) FROM binaries), 0)
	`).Scan(&total)
	if err == nil {
		deps.Metrics.CASBytes.Set(float64(total))
	}
}
