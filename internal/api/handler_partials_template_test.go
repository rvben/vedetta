package api

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// Every htmx partial template is parsed at package init. A typo in one is then a
// panic at startup rather than a 500 the first time somebody opens the page, and
// the parse is not repeated on every poll. This table is the register of which
// templates that promise covers.
func TestPartialTemplatesParsedAtInit(t *testing.T) {
	for _, tc := range []struct {
		name string
		tmpl *template.Template
	}{
		{"grid", cameraGridTmpl},
		{"stats", dashboardStatsTmpl},
		{"activities", activitiesGalleryTmpl},
		{"activity-detail", activityDetailTmpl},
		{"gallery", eventsGalleryTmpl},
		{"detail", eventDetailTmpl},
		{"sysstatus", systemStatusTmpl},
		{"system", systemTmpl},
	} {
		if tc.tmpl == nil {
			t.Errorf("%s template is nil: it is not parsed at package init", tc.name)
			continue
		}
		if got := tc.tmpl.Name(); got != tc.name {
			t.Errorf("template name=%q, want %q", got, tc.name)
		}
		if tc.tmpl.Tree == nil || tc.tmpl.Tree.Root == nil {
			t.Errorf("%s template has no parse tree", tc.name)
		}
	}
}

// The activity-detail template needs one per-request helper. Cloning the parsed
// template must keep working, because the alternative is re-parsing per request.
func TestActivityDetailTemplateClonesPerRequest(t *testing.T) {
	clone, err := activityDetailTmpl.Clone()
	if err != nil {
		t.Fatalf("clone activity-detail: %v", err)
	}
	if clone.Lookup("activity-detail") == nil {
		t.Fatal("clone lost the activity-detail template")
	}
}

// htmx polls the dashboard partials every few seconds per open browser, so the
// handler must not re-parse its template on every request. Parsing allocates in
// the hundreds; rendering a four-card partial does not.
func TestDashboardStatsPartial_DoesNotReparsePerRequest(t *testing.T) {
	s, db := newTestServer(t)
	seedEvent(t, db, "e1", "cam", "person", 0.9, time.Now().UTC())

	render := func() {
		req := httptest.NewRequest(http.MethodGet, "/partials/dashboard-stats", nil)
		s.handleDashboardStatsPartial(httptest.NewRecorder(), req)
	}
	render() // warm any one-time state

	const maxAllocs = 120
	got := testing.AllocsPerRun(20, render)
	if got > maxAllocs {
		t.Errorf("dashboard-stats partial allocates %.0f objects per request, want <= %d "+
			"(a per-request template.Parse is the usual cause)", got, maxAllocs)
	}
}

func TestCameraGridPartial_DoesNotReparsePerRequest(t *testing.T) {
	s, _ := newTestServer(t)
	s.cameras.RegisterForTest(camera.NewTestCamera("front"))

	render := func() {
		req := httptest.NewRequest(http.MethodGet, "/partials/camera-grid", nil)
		s.handleCameraGridPartial(httptest.NewRecorder(), req)
	}
	render()

	const maxAllocs = 250
	got := testing.AllocsPerRun(20, render)
	if got > maxAllocs {
		t.Errorf("camera-grid partial allocates %.0f objects per request, want <= %d "+
			"(a per-request template.Parse is the usual cause)", got, maxAllocs)
	}
}

// Behaviour guard for the same change: the rendered markup must be unchanged by
// moving the parse out of the handler.
func TestDashboardStatsPartial_RendersAllCards(t *testing.T) {
	s, db := newTestServer(t)
	seedEvent(t, db, "e1", "cam", "person", 0.9, time.Now().UTC())

	req := httptest.NewRequest(http.MethodGet, "/partials/dashboard-stats", nil)
	w := httptest.NewRecorder()
	s.handleDashboardStatsPartial(w, req)

	body := w.Body.String()
	for _, want := range []string{
		`<div class="stat-card"><div class="stat-label">Cameras</div><div class="stat-value">0</div></div>`,
		`<div class="stat-card"><div class="stat-label">Online</div><div class="stat-value green">0</div></div>`,
		`<div class="stat-label">Events<span class="stat-label-q"> Today</span></div><div class="stat-value">1</div>`,
		`<div class="stat-card"><div class="stat-label">Storage</div><div class="stat-value">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard-stats body missing %q\ngot: %s", want, body)
		}
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type=%q, want text/html", ct)
	}
}

// A stat that cannot be read is not a stat of zero.
func TestDashboardStatsPartial_DatabaseErrorIsNotZero(t *testing.T) {
	s, db := newTestServer(t)
	seedEvent(t, db, "e1", "cam", "person", 0.9, time.Now().UTC())
	dropTable(t, db, "events")

	req := httptest.NewRequest(http.MethodGet, "/partials/dashboard-stats", nil)
	w := httptest.NewRecorder()
	s.handleDashboardStatsPartial(w, req)

	body := w.Body.String()
	if strings.Contains(body, `<div class="stat-value">0</div></div><div class="stat-card"><div class="stat-label">Storage`) {
		t.Fatalf("unreadable events table rendered as a count of zero: %s", body)
	}
	if !strings.Contains(body, statUnavailable) {
		t.Fatalf("expected an unavailable marker for the events count, got: %s", body)
	}
}
