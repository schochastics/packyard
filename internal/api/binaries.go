package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/store"
)

// Binaries are built by CI once per R minor in matrix.yaml. A publish
// carries whichever builds succeeded; the rest get attached later —
// after a failed build is retried, or when a new R version is added
// to the matrix and existing packages are backfilled.

// AttachBinaryResponse is the body returned by the attach endpoint.
type AttachBinaryResponse struct {
	Channel        string `json:"channel"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Cell           string `json:"cell"`
	SHA256         string `json:"binary_sha256"`
	Size           int64  `json:"size"`
	AlreadyExisted bool   `json:"already_existed"`
	Overwritten    bool   `json:"overwritten"`
}

// handleAttachBinary serves
// POST /api/v1/packages/{channel}/{name}/{version}/binaries/{cell}.
// The body is multipart with one part named "binary". Adding a cell
// to an existing version is allowed on immutable channels; replacing
// a cell's bytes is not.
func handleAttachBinary(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		name := r.PathValue("name")
		version := r.PathValue("version")
		cell := r.PathValue("cell")

		if !packageNameRE.MatchString(name) || !validVersion(version) {
			writeError(w, r, http.StatusBadRequest, CodeBadRequest,
				"invalid package name or version", "")
			return
		}
		if !requireScope(w, r, "publish:"+channel) {
			return
		}
		if deps.Matrix == nil || deps.Matrix.Lookup(cell) == nil {
			writeError(w, r, http.StatusBadRequest, CodeBadRequest,
				fmt.Sprintf("cell %q is not declared in matrix.yaml", cell),
				"see /api/v1/cells for the list of configured cells")
			return
		}
		policy, herr := writableChannel(r.Context(), deps, channel, "binary upload")
		if herr != nil {
			herr.write(w, r)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		blob, herr := readSinglePart(r, deps, "binary")
		if herr != nil {
			herr.write(w, r)
			return
		}

		id, _ := IdentityFromContext(r.Context())
		res, err := storeService(deps).AttachBinary(r.Context(), store.AttachInput{
			Channel:   channel,
			Name:      name,
			Version:   version,
			Policy:    policy,
			Cell:      cell,
			Binary:    blob,
			Actor:     id.Label,
			EventType: "binary_attach",
		})
		switch {
		case errors.Is(err, store.ErrSourceRowMissing):
			writeError(w, r, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("%s@%s is not published on channel %s", name, version, channel),
				"publish the source first; binaries attach to an existing version")
			return
		case errors.Is(err, store.ErrImmutableConflict):
			writeError(w, r, http.StatusConflict, CodeVersionImmutable,
				fmt.Sprintf("%s@%s on immutable channel %s already has a different binary for cell %s", name, version, channel, cell),
				"bump the version to ship a new build")
			return
		case err != nil:
			internalErr("attach binary", err).write(w, r)
			return
		}

		if !res.AlreadyExisted && deps.Index != nil {
			deps.Index.InvalidateChannel(channel)
		}
		if deps.Metrics != nil {
			result := "binary_attached"
			switch {
			case res.AlreadyExisted:
				result = "binary_already_existed"
			case res.Overwritten:
				result = "binary_overwrote"
			}
			deps.Metrics.PublishTotal.WithLabelValues(channel, result).Inc()
		}
		refreshCASBytes(r.Context(), deps)

		status := http.StatusCreated
		if res.AlreadyExisted || res.Overwritten {
			status = http.StatusOK
		}
		writeJSON(w, r, status, AttachBinaryResponse{
			Channel:        res.Channel,
			Name:           res.Name,
			Version:        res.Version,
			Cell:           res.Cell,
			SHA256:         res.Binary.SHA256,
			Size:           res.Binary.Size,
			AlreadyExisted: res.AlreadyExisted,
			Overwritten:    res.Overwritten,
		})
	}
}

// readSinglePart streams the one multipart part named want into CAS.
func readSinglePart(r *http.Request, deps Deps, want string) (store.BlobRef, *httpError) {
	mr, err := r.MultipartReader()
	if err != nil {
		return store.BlobRef{}, &httpError{
			status: http.StatusBadRequest, code: CodeBadRequest,
			msg:  "request must be multipart/form-data",
			hint: fmt.Sprintf("send one part named %q", want),
		}
	}
	var (
		blob store.BlobRef
		seen bool
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return store.BlobRef{}, multipartErr(err)
		}
		if part.FormName() != want || seen {
			_ = part.Close()
			return store.BlobRef{}, &httpError{
				status: http.StatusBadRequest, code: CodeBadRequest,
				msg:  fmt.Sprintf("expected exactly one multipart part named %q", want),
				hint: fmt.Sprintf("got part %q", part.FormName()),
			}
		}
		sum, size, cerr := deps.CAS.Write(part)
		_ = part.Close()
		if cerr != nil {
			var mbErr *http.MaxBytesError
			if errors.As(cerr, &mbErr) {
				return store.BlobRef{}, multipartErr(cerr)
			}
			return store.BlobRef{}, casWriteErr(cerr)
		}
		blob, seen = store.BlobRef{SHA256: sum, Size: size}, true
	}
	if !seen {
		return store.BlobRef{}, &httpError{
			status: http.StatusBadRequest, code: CodeBadRequest,
			msg: fmt.Sprintf("missing multipart part %q", want),
		}
	}
	return blob, nil
}

// MissingBinary is one binary CI still has to build.
type MissingBinary struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Cell    string `json:"cell"`
}

// ListMissingBinariesResponse wraps the list.
type ListMissingBinariesResponse struct {
	Channel string          `json:"channel"`
	Missing []MissingBinary `json:"missing"`
}

// ErrUnknownCell is returned by [MissingBinaries] for a cell filter
// that matrix.yaml doesn't declare.
var ErrUnknownCell = errors.New("cell is not declared in matrix.yaml")

// MissingBinaries lists, for the current version of every package in
// channel (the one PACKAGES serves), each matrix cell without a
// binary. cell restricts the result to one cell ("" = all). Archived
// versions are never listed: backfills target what clients install.
func MissingBinaries(ctx context.Context, deps Deps, channel, cell string) ([]MissingBinary, error) {
	if deps.Matrix == nil {
		return nil, errors.New("matrix.yaml not loaded")
	}
	cells := deps.Matrix.Cells
	if cell != "" {
		c := deps.Matrix.Lookup(cell)
		if c == nil {
			return nil, fmt.Errorf("%w: %q", ErrUnknownCell, cell)
		}
		cells = []config.Cell{*c}
	}
	idx := deps.Index
	if idx == nil {
		idx = NewIndex(deps.DB.DB)
	}
	out := []MissingBinary{}
	for _, c := range cells {
		rows, err := idx.latestRows(ctx, channel, c.Name)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if !row.HasBinary {
				out = append(out, MissingBinary{Name: row.Name, Version: row.Version, Cell: c.Name})
			}
		}
	}
	return out, nil
}

// handleListMissingBinaries serves
// GET /api/v1/channels/{channel}/missing-binaries[?cell=<cell>].
// Readable with the channel's publish token, so a CI backfill job
// needs nothing more than what it publishes with.
func handleListMissingBinaries(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		id, ok := IdentityFromContext(r.Context())
		if !ok || (!id.Scopes.Has("publish:"+channel) && !id.Scopes.Has(auth.ScopeAdmin)) {
			requireScope(w, r, "publish:"+channel)
			return
		}
		if herr := requireChannel(r.Context(), deps, channel); herr != nil {
			herr.write(w, r)
			return
		}
		if lookupChannelMeta(r.Context(), deps, channel).IsProxy() {
			writeError(w, r, http.StatusConflict, CodeChannelIsProxy,
				fmt.Sprintf("channel %q is a proxy; binaries come from upstream", channel), "")
			return
		}
		missing, err := MissingBinaries(r.Context(), deps, channel, r.URL.Query().Get("cell"))
		switch {
		case errors.Is(err, ErrUnknownCell):
			writeError(w, r, http.StatusBadRequest, CodeBadRequest, err.Error(),
				"see /api/v1/cells for the list of configured cells")
			return
		case err != nil:
			internalErr("list missing binaries", err).write(w, r)
			return
		}
		writeJSON(w, r, http.StatusOK, ListMissingBinariesResponse{Channel: channel, Missing: missing})
	}
}
