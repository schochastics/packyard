package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/schochastics/packyard/internal/rpkg"
)

// indexFieldsJSON reads the DESCRIPTION from the source tarball at
// sum and returns the PACKAGES-relevant fields as a JSON object.
// Extraction is best-effort: a blob that isn't a readable R tarball
// yields "{}" and a warning, and the package still materializes —
// its PACKAGES stanza just carries Package and Version only.
func (s *Service) indexFieldsJSON(sum, name string) string {
	desc, err := s.readDescription(sum, name)
	if err != nil {
		slog.Default().Warn("store: no DESCRIPTION metadata; PACKAGES stanza will lack dependency fields",
			"package", name, "sha256", sum, "err", err)
		return "{}"
	}
	// No HTML escaping: the value is only ever read back by packyard,
	// and "R (>= 4.1)" stays readable in the sqlite3 CLI.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rpkg.SelectIndexFields(desc)); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// builtField returns the Built field of the binary tarball at sum, or
// "" when it can't be read.
func (s *Service) builtField(sum, name string) string {
	desc, err := s.readDescription(sum, name)
	if err != nil {
		slog.Default().Warn("store: no DESCRIPTION in binary tarball",
			"package", name, "sha256", sum, "err", err)
		return ""
	}
	return desc["Built"]
}

func (s *Service) readDescription(sum, name string) (map[string]string, error) {
	rc, err := s.cas.Read(sum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return rpkg.ReadDescription(rc, name)
}

// BackfillResult counts rows updated by [Service.BackfillMetadata].
type BackfillResult struct {
	Packages int
	Binaries int
}

// BackfillMetadata extracts DESCRIPTION metadata for rows stored
// before it was recorded (index_fields / built still NULL). Safe to
// run repeatedly and against a live server: each row is updated
// independently and only while still NULL.
func (s *Service) BackfillMetadata(ctx context.Context) (BackfillResult, error) {
	var res BackfillResult

	type pending struct {
		id        int64
		name, sum string
	}
	collect := func(query string) ([]pending, error) {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var out []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.name, &p.sum); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	}

	pkgs, err := collect(`SELECT id, name, source_sha256 FROM packages WHERE index_fields IS NULL`)
	if err != nil {
		return res, fmt.Errorf("backfill: list packages: %w", err)
	}
	for _, p := range pkgs {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE packages SET index_fields = ? WHERE id = ? AND index_fields IS NULL`,
			s.indexFieldsJSON(p.sum, p.name), p.id); err != nil {
			return res, fmt.Errorf("backfill: package %d: %w", p.id, err)
		}
		res.Packages++
	}

	bins, err := collect(`
		SELECT b.id, p.name, b.binary_sha256
		FROM binaries b JOIN packages p ON p.id = b.package_id
		WHERE b.built IS NULL`)
	if err != nil {
		return res, fmt.Errorf("backfill: list binaries: %w", err)
	}
	for _, b := range bins {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE binaries SET built = ? WHERE id = ? AND built IS NULL`,
			s.builtField(b.sum, b.name), b.id); err != nil {
			return res, fmt.Errorf("backfill: binary %d: %w", b.id, err)
		}
		res.Binaries++
	}
	return res, nil
}
