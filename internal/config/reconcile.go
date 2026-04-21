package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// ReconcileResult summarizes the effect of applying a channels config to
// the channels table. Obsolete names are preserved in the DB (they may
// still have packages attached) so the caller can warn rather than act.
type ReconcileResult struct {
	Created   []string
	Updated   []string
	Unchanged []string
	Obsolete  []string
}

// dbChannel mirrors one row of the channels table for the reconciliation
// diff. Kept private to this file.
type dbChannel struct {
	name            string
	overwritePolicy string
	isDefault       bool
}

// ReconcileChannels applies cfg to the channels table. Semantics:
//
//   - Channels in cfg but not in DB are INSERTed.
//   - Channels in both are UPDATEd when overwrite_policy or is_default
//     differs.
//   - Channels in DB but not in cfg are NOT deleted. They stay as-is
//     (minus any default flag — see below) and are reported via
//     Obsolete so the caller can surface a warning. Auto-deleting
//     would lose packages; manual cleanup is safer at v1.
//
// The whole thing runs in one transaction. Default-channel changes are
// done in two steps — clear all is_default flags, then set the one —
// to avoid tripping the channels_one_default partial unique index while
// the diff is in flight.
func ReconcileChannels(ctx context.Context, db *sql.DB, cfg *ChannelsConfig) (ReconcileResult, error) {
	var result ReconcileResult
	if cfg == nil {
		return result, errors.New("reconcile: nil config")
	}
	desiredDefault := cfg.Default()
	if desiredDefault == nil {
		// Shouldn't happen if cfg was produced by DecodeChannels, which
		// enforces exactly-one-default, but guard anyway.
		return result, errors.New("reconcile: config has no default channel")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("reconcile: begin tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			_ = rbErr // caller already has the primary error
		}
	}()

	existing, err := loadChannels(ctx, tx)
	if err != nil {
		return result, err
	}

	// Step 1: clear every is_default flag so later inserts/updates never
	// collide on the partial unique index. We set exactly one at the end.
	if _, err := tx.ExecContext(ctx,
		`UPDATE channels SET is_default = 0 WHERE is_default = 1`); err != nil {
		return result, fmt.Errorf("reconcile: clear defaults: %w", err)
	}

	// Step 2: insert missing, update changed.
	for _, ch := range cfg.Channels {
		cur, present := existing[ch.Name]
		switch {
		case !present:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO channels(name, overwrite_policy, is_default)
				 VALUES (?, ?, 0)`,
				ch.Name, ch.OverwritePolicy,
			); err != nil {
				return result, fmt.Errorf("reconcile: insert %q: %w", ch.Name, err)
			}
			result.Created = append(result.Created, ch.Name)
		case cur.overwritePolicy != ch.OverwritePolicy:
			if _, err := tx.ExecContext(ctx,
				`UPDATE channels SET overwrite_policy = ? WHERE name = ?`,
				ch.OverwritePolicy, ch.Name,
			); err != nil {
				return result, fmt.Errorf("reconcile: update %q: %w", ch.Name, err)
			}
			result.Updated = append(result.Updated, ch.Name)
		default:
			// Same policy. Whether the default flag is changing is
			// decided globally in Step 3; we don't diff it here.
			result.Unchanged = append(result.Unchanged, ch.Name)
		}
	}

	// Step 3: set the single default.
	if _, err := tx.ExecContext(ctx,
		`UPDATE channels SET is_default = 1 WHERE name = ?`,
		desiredDefault.Name,
	); err != nil {
		return result, fmt.Errorf("reconcile: set default: %w", err)
	}

	// Step 4: collect obsolete (in DB but not in cfg). They stay in place.
	desiredNames := map[string]struct{}{}
	for _, ch := range cfg.Channels {
		desiredNames[ch.Name] = struct{}{}
	}
	for name := range existing {
		if _, wanted := desiredNames[name]; !wanted {
			result.Obsolete = append(result.Obsolete, name)
		}
	}

	// Step 5: if the pre-tx row counted as "Unchanged" but the effective
	// is_default flag just changed for it, reclassify as Updated. This
	// keeps the summary honest for the common case of swapping which
	// channel is the default.
	promoteIfDefaultMoved(&result, existing, desiredDefault.Name)

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("reconcile: commit: %w", err)
	}

	sort.Strings(result.Created)
	sort.Strings(result.Updated)
	sort.Strings(result.Unchanged)
	sort.Strings(result.Obsolete)
	return result, nil
}

func loadChannels(ctx context.Context, tx *sql.Tx) (map[string]dbChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, overwrite_policy, is_default FROM channels`)
	if err != nil {
		return nil, fmt.Errorf("reconcile: read channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]dbChannel{}
	for rows.Next() {
		var c dbChannel
		var isDefault int
		if err := rows.Scan(&c.name, &c.overwritePolicy, &isDefault); err != nil {
			return nil, fmt.Errorf("reconcile: scan channel: %w", err)
		}
		c.isDefault = isDefault == 1
		out[c.name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile: iterate channels: %w", err)
	}
	return out, nil
}

// promoteIfDefaultMoved moves a channel name from Unchanged to Updated
// when the default flag moved to or from it. This only inspects the
// classification; the actual flip was done as part of Step 1/3.
func promoteIfDefaultMoved(result *ReconcileResult, existing map[string]dbChannel, newDefault string) {
	isMoveRelevant := func(name string) bool {
		cur, ok := existing[name]
		if !ok {
			return false // handled by Created branch
		}
		// The default flag moved if it WAS default and we're making it
		// not-default, or it WAS NOT default and we're making it default.
		return cur.isDefault != (name == newDefault)
	}

	remaining := result.Unchanged[:0]
	for _, name := range result.Unchanged {
		if isMoveRelevant(name) {
			result.Updated = append(result.Updated, name)
		} else {
			remaining = append(remaining, name)
		}
	}
	result.Unchanged = remaining
}
