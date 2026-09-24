package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
	"github.com/schochastics/packyard/internal/store"
)

func blob(t *testing.T, svc *store.Service, body string) store.BlobRef {
	t.Helper()
	ref, err := svc.WriteBlob(bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func binaryCells(t *testing.T, database *db.DB, name string) map[string]string {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), `
		SELECT b.cell, b.binary_sha256 FROM binaries b
		JOIN packages p ON p.id = b.package_id WHERE p.name = ?`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var c, s string
		if err := rows.Scan(&c, &s); err != nil {
			t.Fatal(err)
		}
		out[c] = s
	}
	return out
}

func TestMaterializeMatrix(t *testing.T) {
	database, svc := setup(t)
	ctx := context.Background()
	src := blob(t, svc, "src")
	in := store.Input{Channel: "dev", Name: "a", Version: "1.0", Policy: config.PolicyImmutable, Source: src}

	res, err := svc.Materialize(ctx, in)
	if err != nil || res.AlreadyExisted || res.Overwritten {
		t.Fatalf("first publish = %+v, %v", res, err)
	}

	// Immutable replay, same bytes: idempotent.
	res, err = svc.Materialize(ctx, in)
	if err != nil || !res.AlreadyExisted {
		t.Fatalf("replay = %+v, %v", res, err)
	}

	// Immutable, different bytes: conflict.
	diff := in
	diff.Source = blob(t, svc, "other src")
	if _, err := svc.Materialize(ctx, diff); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("different bytes: err = %v", err)
	}

	// Mutable: overwrite replaces binaries.
	mut := in
	mut.Policy = config.PolicyMutable
	mut.Source = blob(t, svc, "new src")
	mut.Binaries = []store.BinaryInput{{Cell: "r-4.4", Blob: blob(t, svc, "bin44")}}
	res, err = svc.Materialize(ctx, mut)
	if err != nil || !res.Overwritten {
		t.Fatalf("overwrite = %+v, %v", res, err)
	}
	mut.Binaries = []store.BinaryInput{{Cell: "r-4.5", Blob: blob(t, svc, "bin45")}}
	if _, err := svc.Materialize(ctx, mut); err != nil {
		t.Fatal(err)
	}
	if cells := binaryCells(t, database, "a"); len(cells) != 1 || cells["r-4.5"] == "" {
		t.Errorf("binaries after overwrite = %v", cells)
	}
}

// An immutable replay that carries a binary for a missing cell stores
// it, and one carrying different bytes for an existing cell is refused.
func TestMaterializeImmutableReplayAppliesBinaries(t *testing.T) {
	database, svc := setup(t)
	ctx := context.Background()
	in := store.Input{Channel: "dev", Name: "a", Version: "1.0", Policy: config.PolicyImmutable, Source: blob(t, svc, "src")}
	if _, err := svc.Materialize(ctx, in); err != nil {
		t.Fatal(err)
	}

	bin := blob(t, svc, "bin44")
	in.Binaries = []store.BinaryInput{{Cell: "r-4.4", Blob: bin}}
	res, err := svc.Materialize(ctx, in)
	if err != nil || !res.AlreadyExisted {
		t.Fatalf("replay with binary = %+v, %v", res, err)
	}
	if cells := binaryCells(t, database, "a"); cells["r-4.4"] != bin.SHA256 {
		t.Fatalf("binary not stored on replay: %v", cells)
	}

	// Same binary again: still fine.
	if _, err := svc.Materialize(ctx, in); err != nil {
		t.Fatalf("second replay: %v", err)
	}

	in.Binaries = []store.BinaryInput{{Cell: "r-4.4", Blob: blob(t, svc, "tampered")}}
	if _, err := svc.Materialize(ctx, in); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("replay with different binary: err = %v", err)
	}
	if cells := binaryCells(t, database, "a"); cells["r-4.4"] != bin.SHA256 {
		t.Errorf("binary changed by a rejected replay: %v", cells)
	}
}

func TestAttachBinaryMatrix(t *testing.T) {
	_, svc := setup(t)
	ctx := context.Background()
	attach := func(body string, policy string) (*store.AttachResult, error) {
		return svc.AttachBinary(ctx, store.AttachInput{
			Channel: "dev", Name: "a", Version: "1.0", Cell: "r-4.4",
			Policy: policy, Binary: blob(t, svc, body),
		})
	}

	if _, err := attach("bin", config.PolicyImmutable); !errors.Is(err, store.ErrSourceRowMissing) {
		t.Fatalf("attach before publish: err = %v", err)
	}
	if _, err := svc.Materialize(ctx, store.Input{Channel: "dev", Name: "a", Version: "1.0",
		Policy: config.PolicyImmutable, Source: blob(t, svc, "src")}); err != nil {
		t.Fatal(err)
	}
	if res, err := attach("bin", config.PolicyImmutable); err != nil || res.AlreadyExisted || res.Overwritten {
		t.Fatalf("first attach = %+v, %v", res, err)
	}
	if res, err := attach("bin", config.PolicyImmutable); err != nil || !res.AlreadyExisted {
		t.Fatalf("same attach = %+v, %v", res, err)
	}
	if _, err := attach("other", config.PolicyImmutable); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("different attach on immutable: err = %v", err)
	}
	if res, err := attach("other", config.PolicyMutable); err != nil || !res.Overwritten {
		t.Fatalf("different attach on mutable = %+v, %v", res, err)
	}
}
