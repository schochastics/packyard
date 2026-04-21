package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

func TestListEventsAfterPublishYankDelete(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	rec := doYank(t, fx, "dev", "alpha", "1.0.0", fx.token, `{"reason":"oops"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("yank: %d %s", rec.Code, rec.Body.String())
	}
	delRec := doDelete(t, fx, "dev", "alpha", "1.0.0", fx.token)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", delRec.Code, delRec.Body.String())
	}

	listRec := doGet(t, fx, "/api/v1/events", fx.token)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: %d body %s", listRec.Code, listRec.Body.String())
	}
	if got := listRec.Header().Get("X-Total-Count"); got == "" || got == "0" {
		t.Errorf("X-Total-Count = %q, want positive", got)
	}

	var resp ListEventsResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// ASC order, so the oldest event comes first.
	types := make([]string, 0, len(resp.Events))
	for _, e := range resp.Events {
		types = append(types, e.Type)
	}
	// Expect at least one publish, one yank, one delete (other event
	// types from the fixture's reconcile may interleave).
	wantSubstrings := []string{"publish", "yank", "delete"}
	for _, want := range wantSubstrings {
		found := false
		for _, got := range types {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected event type %q in %v", want, types)
		}
	}

	// Rows with channel/package/version populated should have the
	// pointer fields non-nil.
	for _, e := range resp.Events {
		if e.Type == "publish" {
			if e.Package == nil || *e.Package != "alpha" {
				t.Errorf("publish event package = %v, want alpha", e.Package)
			}
			if e.Channel == nil || *e.Channel != "dev" {
				t.Errorf("publish event channel = %v", e.Channel)
			}
		}
	}
}

func TestListEventsSinceIDCursor(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	for i := 0; i < 5; i++ {
		publishSource(t, fx, "dev", fmt.Sprintf("pkg%d", i), "1.0.0", []byte("x"))
	}

	// First page: grab 2 oldest.
	rec := doGet(t, fx, "/api/v1/events?limit=2", fx.token)
	var first ListEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if len(first.Events) != 2 {
		t.Fatalf("first page got %d, want 2", len(first.Events))
	}
	// Ascending id order.
	if first.Events[0].ID >= first.Events[1].ID {
		t.Errorf("events not in ascending id order: %+v", first.Events)
	}

	lastID := first.Events[1].ID
	rec = doGet(t, fx, "/api/v1/events?since_id="+strconv.FormatInt(lastID, 10)+"&limit=2", fx.token)
	var second ListEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	// Every returned event must have id > lastID.
	for _, e := range second.Events {
		if e.ID <= lastID {
			t.Errorf("cursor violation: got id %d, want > %d", e.ID, lastID)
		}
	}
}

func TestListEventsFilterByType(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	publishSource(t, fx, "dev", "beta", "1.0.0", []byte("b"))
	doYank(t, fx, "dev", "alpha", "1.0.0", fx.token, "")

	rec := doGet(t, fx, "/api/v1/events?type=yank", fx.token)
	var resp ListEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if len(resp.Events) == 0 {
		t.Fatal("no yank events returned")
	}
	for _, e := range resp.Events {
		if e.Type != "yank" {
			t.Errorf("type filter bypassed: got %q", e.Type)
		}
	}
}

func TestListEventsFilterByChannelAndPackage(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	publishSource(t, fx, "prod", "gamma", "1.0.0", []byte("g"))

	rec := doGet(t, fx, "/api/v1/events?channel=prod&package=gamma", fx.token)
	var resp ListEventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	for _, e := range resp.Events {
		if e.Channel == nil || *e.Channel != "prod" {
			t.Errorf("channel filter bypassed: %+v", e)
		}
		if e.Package == nil || *e.Package != "gamma" {
			t.Errorf("package filter bypassed: %+v", e)
		}
	}
}

func TestListEventsBadSinceID(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := doGet(t, fx, "/api/v1/events?since_id=-1", fx.token)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	rec = doGet(t, fx, "/api/v1/events?since_id=abc", fx.token)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestListEventsRequiresAdmin(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	rec := doGet(t, fx, "/api/v1/events", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: %d", rec.Code)
	}

	tok := seedScopedToken(t, fx, "reader", "read:*")
	rec = doGet(t, fx, "/api/v1/events", tok)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d", rec.Code)
	}
}
