package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The since filter must be applied by the database, not after paging. Filtering
// in memory drops rows the page already paid for, so a page comes back short
// while total (and therefore has_more) still counts the excluded events.
func TestListEvents_SinceFiltersBeforePaging(t *testing.T) {
	s, db := newTestServer(t)
	base := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "old", "cam", "person", 0.9, base.Add(-3*time.Hour))
	seedEvent(t, db, "mid", "cam", "person", 0.9, base.Add(-2*time.Hour))
	seedEvent(t, db, "new", "cam", "person", 0.9, base.Add(-1*time.Hour))

	since := base.Add(-150 * time.Minute) // between "old" and "mid"
	limit := 2
	params := ListEventsParams{Since: &since, Limit: &limit}

	req := httptest.NewRequest(http.MethodGet, "/api/events?since=x&limit=2", nil)
	w := httptest.NewRecorder()
	s.ListEvents(w, req, params)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	gotIDs := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		gotIDs = append(gotIDs, it.ID)
	}
	want := []string{"new", "mid"}
	if len(gotIDs) != len(want) {
		t.Fatalf("items=%v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("items=%v, want %v", gotIDs, want)
		}
	}
	if out.Total != 2 {
		t.Errorf("total=%d, want 2 (only events at or after since)", out.Total)
	}
	if out.HasMore {
		t.Error("has_more=true but both matching events are on this page")
	}
}

// A second page must not be shortened by rows the filter should never have
// loaded: with since applied in SQL, offset counts only matching rows.
func TestListEvents_SincePaginatesWithoutGaps(t *testing.T) {
	s, db := newTestServer(t)
	base := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "old-1", "cam", "person", 0.9, base.Add(-5*time.Hour))
	seedEvent(t, db, "old-2", "cam", "person", 0.9, base.Add(-4*time.Hour))
	seedEvent(t, db, "keep-1", "cam", "person", 0.9, base.Add(-2*time.Hour))
	seedEvent(t, db, "keep-2", "cam", "person", 0.9, base.Add(-1*time.Hour))

	since := base.Add(-3 * time.Hour)
	limit := 2
	offset := 0
	params := ListEventsParams{Since: &since, Limit: &limit, Offset: &offset}

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	s.ListEvents(w, req, params)

	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 2 || out.Items[0].ID != "keep-2" || out.Items[1].ID != "keep-1" {
		t.Errorf("first page = %+v, want keep-2, keep-1", out.Items)
	}
	if out.Total != 2 {
		t.Errorf("total=%d, want 2", out.Total)
	}
}
