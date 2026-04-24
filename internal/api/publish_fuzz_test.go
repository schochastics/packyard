package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FuzzPublishManifest throws fuzzer-produced bytes at the manifest
// form field while keeping the rest of the multipart envelope
// well-formed. This exercises the JSON decoder and the manifest
// validator in handlePublish. We assert the server never panics
// and never returns 5xx — every hostile manifest should bounce at
// 4xx with a structured error envelope.
func FuzzPublishManifest(f *testing.F) {
	// Seed with manifests the server should accept (small sample of
	// the shapes publish_test.go uses) plus a couple of reliably
	// broken ones, so the fuzzer starts with a diverse corpus.
	seedOK, _ := json.Marshal(map[string]any{
		"binaries": []any{},
	})
	f.Add(seedOK)
	f.Add([]byte(`{"binaries":null}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"binaries":[{"cell":"ubuntu-22.04-amd64-r-4.4","part":"bin1"}]}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`not json`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, manifest []byte) {
		fx := newPublishFixture(t)

		body, ct := buildFuzzPublishBody(t, manifest, []byte("source bytes"))

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/packages/dev/fuzzpkg/0.0.1", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer "+fx.token)

		rec := httptest.NewRecorder()
		fx.mux.ServeHTTP(rec, req)

		if rec.Code >= 500 {
			t.Fatalf("handler returned 5xx on hostile manifest: code=%d body=%s",
				rec.Code, rec.Body.String())
		}
	})
}

// FuzzPublishMultipartBody throws fuzzer-produced bytes at the
// entire request body with a fixed boundary. Most inputs should
// fail at multipart parse with 400; the contract is simply no
// panics and no 5xx.
func FuzzPublishMultipartBody(f *testing.F) {
	// Seed with a couple of valid multipart bodies so the fuzzer has
	// a shape to mutate away from.
	goodManifest, _ := json.Marshal(map[string]any{"binaries": []any{}})
	goodBody, _ := buildFuzzPublishBodyRaw(goodManifest, []byte("source bytes"))
	f.Add(goodBody)
	f.Add([]byte("--boundary--"))
	f.Add([]byte(""))
	f.Add([]byte("--boundary\r\nContent-Disposition: form-data; name=\"manifest\"\r\n\r\nnope\r\n--boundary--"))

	f.Fuzz(func(t *testing.T, body []byte) {
		fx := newPublishFixture(t)

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/packages/dev/fuzzpkg/0.0.1", bytes.NewReader(body))
		req.Header.Set("Content-Type", `multipart/form-data; boundary="boundary"`)
		req.Header.Set("Authorization", "Bearer "+fx.token)

		rec := httptest.NewRecorder()
		fx.mux.ServeHTTP(rec, req)

		if rec.Code >= 500 {
			t.Fatalf("handler returned 5xx on hostile body: code=%d body=%s",
				rec.Code, rec.Body.String())
		}
	})
}

// buildFuzzPublishBody is a thinner version of buildPublishBody
// that takes raw manifest bytes (to let the fuzzer inject arbitrary
// JSON — or non-JSON — without the helper re-marshaling it). Also
// writes a single "source" part.
func buildFuzzPublishBody(t *testing.T, manifest, source []byte) (*bytes.Buffer, string) {
	t.Helper()
	body, ct := buildFuzzPublishBodyRaw(manifest, source)
	return bytes.NewBuffer(body), ct
}

func buildFuzzPublishBodyRaw(manifest, source []byte) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	mp, err := mw.CreateFormField("manifest")
	if err != nil {
		panic(err)
	}
	if _, err := io.Copy(mp, bytes.NewReader(manifest)); err != nil {
		panic(err)
	}

	sp, err := mw.CreateFormFile("source", "source.bin")
	if err != nil {
		panic(err)
	}
	if _, err := io.Copy(sp, bytes.NewReader(source)); err != nil {
		panic(err)
	}

	if err := mw.Close(); err != nil {
		panic(err)
	}
	// Guard against a fuzzer-provided manifest that contains the
	// boundary string and corrupts the envelope — not the bug we're
	// hunting and it swamps the signal.
	if strings.Contains(string(manifest), mw.Boundary()) {
		// Return an empty body so the handler bounces at parse with 400.
		return nil, mw.FormDataContentType()
	}
	return buf.Bytes(), mw.FormDataContentType()
}
