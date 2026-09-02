package api

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/storage"
)

// --- HTML partial handlers for htmx ---

// partialFuncs are the template helpers shared by every partial. They close over
// nothing request- or server-specific, so the whole set is built once and the
// templates below can be parsed at package init.
var partialFuncs = template.FuncMap{
	"timeAgo": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds ago", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		default:
			return fmt.Sprintf("%dd ago", int(d.Hours()/24))
		}
	},
	"scorePercent": func(s float32) string {
		return fmt.Sprintf("%.0f%%", s*100)
	},
	"toFloat32": func(f float64) float32 { return float32(f) },
	"formatTime": func(t time.Time) template.HTML {
		iso := t.UTC().Format(time.RFC3339)
		display := t.UTC().Format("2006-01-02 15:04:05 UTC")
		return template.HTML(fmt.Sprintf(`<time datetime="%s">%s</time>`, iso, display))
	},
	"formatBytes": formatBytes,
	"displayName": displayName,
	"eventDuration": func(e camera.Event) string {
		if e.EndTime.IsZero() {
			return ""
		}
		d := e.EndTime.Sub(e.Timestamp)
		if d < time.Second {
			return ""
		}
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	},
}

// activityFuncs render an activity. Like partialFuncs they are static, so the
// two activity templates carry them from init.
var activityFuncs = template.FuncMap{
	"activityTitle":    activityTitle,
	"activityDuration": activityDuration,
	"activityFacets":   activityFacets,
	"activityStateLabel": func(state storage.ActivityState) string {
		if state == storage.ActivityStateOpen {
			return "Collecting evidence"
		}
		return "Finalized"
	},
}

// statUnavailable is what a stat card shows when the query behind it failed. A
// count that could not be read is not a count of zero.
const statUnavailable = "n/a"

func (s *Server) handleCameraGridPartial(w http.ResponseWriter, _ *http.Request) {
	statuses := s.cameraStatuses()

	w.Header().Set("Content-Type", "text/html")

	if len(statuses) == 0 {
		const emptyHTML = `<div class="empty-hero">
  <div class="empty-add-glyph" aria-hidden="true">+</div>
  <div class="empty-title">No cameras yet</div>
  <div class="empty-desc">Vedetta can find cameras on your network automatically.</div>
  <button type="button" class="btn btn-primary empty-add-btn" data-action-click="openAddCameraModal()" aria-haspopup="dialog">
    <span aria-hidden="true">+</span><span>Add your first camera</span>
  </button>
</div>`
		fmt.Fprint(w, emptyHTML)
		return
	}

	type cameraCard struct {
		Name        string
		DisplayName string
		Online      bool
		HasMotion   bool
		Stopped     bool
		Sleeping    bool
		Doorbell    bool
		LastSeen    string // RFC3339, empty when no frame has ever been seen
	}

	doorbellCameras := make(map[string]bool, len(s.cameraConfigs))
	for _, cfg := range s.cameraConfigs {
		if cfg.Doorbell.Enabled {
			doorbellCameras[cfg.Name] = true
		}
	}

	cards := make([]cameraCard, 0, len(statuses))
	for _, st := range statuses {
		lastSeen := ""
		if !st.LastSeen.IsZero() {
			lastSeen = st.LastSeen.UTC().Format(time.RFC3339)
		}
		cards = append(cards, cameraCard{
			Name:        st.Name,
			DisplayName: displayName(st.Name),
			Online:      st.Online,
			HasMotion:   st.HasMotion,
			Stopped:     st.Stopped,
			Sleeping:    st.Sleeping,
			Doorbell:    doorbellCameras[st.Name],
			LastSeen:    lastSeen,
		})
	}

	if err := cameraGridTmpl.Execute(w, cards); err != nil {
		slog.Error("template error", "error", err)
	}
}

func (s *Server) handleDashboardStatsPartial(w http.ResponseWriter, _ *http.Request) {
	statuses := s.cameraStatuses()
	onlineCount := 0
	for _, st := range statuses {
		if st.Online {
			onlineCount++
		}
	}

	// A count the database could not answer is rendered as unavailable. Falling
	// back to 0 would claim the day had no events, which is a different fact.
	eventsToday := statUnavailable
	if count, err := s.db.CountEventsToday(""); err == nil {
		eventsToday = strconv.Itoa(count)
	} else {
		slog.Error("dashboard stats: events today", "error", err)
	}
	stats := s.recorder.StorageStats()

	type dashData struct {
		CameraCount int
		OnlineCount int
		EventsToday string
		Storage     string
	}

	data := dashData{
		CameraCount: len(statuses),
		OnlineCount: onlineCount,
		EventsToday: eventsToday,
		Storage:     formatBytes(stats.TotalBytes),
	}

	w.Header().Set("Content-Type", "text/html")
	if err := dashboardStatsTmpl.Execute(w, data); err != nil {
		slog.Error("template error", "error", err)
	}
}

func eventFiltersFromRequest(r *http.Request) storage.EventFilters {
	query := r.URL.Query()
	filters := storage.EventFilters{
		Camera:   query.Get("camera"),
		Label:    query.Get("label"),
		Zone:     query.Get("zone"),
		Object:   query.Get("object"),
		Category: query.Get("category"),
		Kind:     query.Get("kind"),
		Search:   query.Get("q"),
	}

	if after, err := time.Parse(time.RFC3339, query.Get("after")); err == nil {
		filters.After = after
	} else {
		now := time.Now()
		switch query.Get("range") {
		case "today":
			filters.After = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		case "24h":
			filters.After = now.Add(-24 * time.Hour)
		case "7d":
			filters.After = now.AddDate(0, 0, -7)
		}
	}
	if before, err := time.Parse(time.RFC3339, query.Get("before")); err == nil {
		filters.Before = before
	}

	return filters
}

func activityFiltersFromRequest(r *http.Request) storage.ActivityFilters {
	query := r.URL.Query()
	filters := storage.ActivityFilters{
		Camera:   query.Get("camera"),
		Label:    query.Get("label"),
		Zone:     query.Get("zone"),
		Object:   query.Get("object"),
		Category: query.Get("category"),
		Kind:     query.Get("kind"),
		State:    storage.ActivityState(query.Get("state")),
		Search:   query.Get("q"),
	}
	if after, err := time.Parse(time.RFC3339, query.Get("after")); err == nil {
		filters.After = after
	} else {
		now := time.Now()
		switch query.Get("range") {
		case "today":
			filters.After = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		case "24h":
			filters.After = now.Add(-24 * time.Hour)
		case "7d":
			filters.After = now.AddDate(0, 0, -7)
		}
	}
	if before, err := time.Parse(time.RFC3339, query.Get("before")); err == nil {
		filters.Before = before
	}
	return filters
}

func activityTitle(activity storage.Activity) string {
	if len(activity.RecognizedNames) > 0 {
		return strings.Join(activity.RecognizedNames, " and ")
	}
	if len(activity.Labels) == 0 {
		return "Camera activity"
	}
	labels := make([]string, len(activity.Labels))
	for i, label := range activity.Labels {
		labels[i] = displayName(label)
	}
	return strings.Join(labels, " and ")
}

func activityDuration(activity storage.Activity) string {
	seconds := activity.DurationSeconds
	if seconds < 1 {
		return "Momentary"
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds %= 60
	if seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dm %ds", minutes, seconds)
}

func activityFacets(values []string) string {
	if len(values) == 0 {
		return ""
	}
	display := make([]string, len(values))
	for i, value := range values {
		display[i] = displayName(value)
	}
	return strings.Join(display, ", ")
}

func (s *Server) handleActivitiesGalleryPartial(w http.ResponseWriter, r *http.Request) {
	filters := activityFiltersFromRequest(r)
	limit := 50
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 {
		limit = min(value, maxActivityPageSize)
	}
	offset := 0
	if value, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && value >= 0 {
		offset = value
	}

	activities, err := s.db.QueryActivitiesFiltered(filters, limit, offset)
	if err != nil {
		s.serverErrorText(w, r, err)
		return
	}
	total, err := s.db.CountActivitiesFiltered(filters)
	if err != nil {
		s.serverErrorText(w, r, err)
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("Content-Type", "text/html")

	if offset == 0 && len(activities) == 0 {
		_, _ = fmt.Fprint(w, `<div class="empty-state activity-empty"><h2>No activity in this view</h2><p>Try a wider time range or clear a filter.</p></div>`)
		return
	}

	if err := activitiesGalleryTmpl.Execute(w, activities); err != nil {
		slog.Error("activity gallery template error", "error", err)
		return
	}

	if len(activities) == limit {
		params := r.URL.Query()
		params.Set("limit", strconv.Itoa(limit))
		params.Set("offset", strconv.Itoa(offset+limit))
		nextURL := "/partials/activities-gallery?" + params.Encode()
		_, _ = fmt.Fprintf(w, `<div id="load-more-trigger" class="scroll-sentinel" hx-get="%s" hx-trigger="revealed" hx-swap="outerHTML" role="status" aria-label="Loading more activity"><span class="loading-spinner" aria-hidden="true"></span></div>`, template.HTMLEscapeString(nextURL))
	}
}

func (s *Server) handleActivityDetailPartial(w http.ResponseWriter, r *http.Request) {
	activity, err := s.db.GetActivityByID(r.PathValue("id"))
	if err != nil {
		s.serverErrorText(w, r, err)
		return
	}
	if activity == nil {
		http.Error(w, "activity not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	// activityEvidenceURL is the one helper that depends on this request, so the
	// parsed template is cloned and the real function bound over the placeholder.
	// Clone copies the already-parsed tree; it never re-parses.
	tmpl, err := activityDetailTmpl.Clone()
	if err != nil {
		s.serverErrorText(w, r, err)
		return
	}
	tmpl = tmpl.Funcs(template.FuncMap{
		"activityEvidenceURL": func(eventID string) string {
			params := r.URL.Query()
			params.Set("activity", activity.ID)
			return "/event.html?id=" + url.QueryEscape(eventID) + "&" + params.Encode()
		},
	})
	if err := tmpl.Execute(w, activity); err != nil {
		slog.Error("activity detail template error", "error", err)
	}
}

func eventReviewQuery(query url.Values) string {
	allowed := []string{"activity", "camera", "label", "object", "category", "state", "q", "range", "after", "before"}
	params := url.Values{}
	for _, key := range allowed {
		if value := query.Get(key); value != "" {
			params.Set(key, value)
		}
	}
	return params.Encode()
}

func (s *Server) handleEventsGalleryPartial(w http.ResponseWriter, r *http.Request) {
	filters := eventFiltersFromRequest(r)
	embed := r.URL.Query().Get("embed") == "1"
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	events, err := s.db.QueryEventsFiltered(filters, limit, offset)
	if err != nil {
		s.serverErrorText(w, r, err)
		return
	}

	if total, err := s.db.CountEventsFiltered(filters); err == nil {
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
	}

	w.Header().Set("Content-Type", "text/html")

	if offset == 0 && len(events) == 0 {
		_, _ = fmt.Fprint(w, `<div class="empty-state"><p>No events recorded yet.</p></div>`)
		return
	}

	if err := eventsGalleryTmpl.Execute(w, events); err != nil {
		slog.Error("template error", "error", err)
	}

	// If we got a full page of results, append a sentinel for infinite scroll.
	// embed=1 suppresses pagination so the gallery can be hosted inside another
	// page (e.g. the per-camera detail view) without ballooning into thousands
	// of cards as the user scrolls.
	if !embed && len(events) == limit {
		nextOffset := offset + limit
		params := r.URL.Query()
		params.Set("limit", strconv.Itoa(limit))
		params.Set("offset", strconv.Itoa(nextOffset))
		nextURL := "/partials/events-gallery?" + params.Encode()
		_, _ = fmt.Fprintf(w, `<div id="load-more-trigger" class="scroll-sentinel" hx-get="%s" hx-trigger="revealed" hx-swap="outerHTML" role="status" aria-label="Loading more events"><span class="loading-spinner" aria-hidden="true"></span></div>`, template.HTMLEscapeString(nextURL))
	}
}

func (s *Server) handleEventDetailPartial(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	event, err := s.db.GetEventByID(id)
	if err != nil {
		s.serverErrorText(w, r, err)
		return
	}

	if event == nil {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}

	filters := eventFiltersFromRequest(r)
	prevID, nextID, _ := s.db.GetAdjacentEventsFiltered(id, filters)

	sightings, _ := s.db.GetEventSightings(id)
	knownObjects, _ := s.db.ListKnownObjectsByLabel(event.Label)

	type namedPerson struct {
		ID   int64
		Name string
	}

	// Load named people for person events
	var namedPeople []namedPerson
	if event.Label == "person" {
		if people, err := s.db.ListPeople(); err == nil {
			for _, p := range people {
				if p.Name != "" && !p.Ignore {
					namedPeople = append(namedPeople, namedPerson{ID: p.ID, Name: p.Name})
				}
			}
		}
	}

	type eventDetailData struct {
		camera.Event
		PrevID       string
		NextID       string
		RecordingURL string
		HasRecording bool
		Duration     string
		Sightings    []storage.ObjectSighting
		KnownObjects []storage.KnownObject
		NamedPeople  []namedPerson
		PrevURL      string
		NextURL      string
	}

	recURL := fmt.Sprintf("/camera.html?name=%s&t=%s",
		event.CameraName,
		event.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
	)

	var duration string
	if !event.EndTime.IsZero() {
		d := event.EndTime.Sub(event.Timestamp).Round(time.Second)
		duration = d.String()
	}

	hasRecording := s.recorder.HasSegments(event.CameraName, event.Timestamp)

	data := eventDetailData{
		Event:        *event,
		PrevID:       prevID,
		NextID:       nextID,
		RecordingURL: recURL,
		HasRecording: hasRecording,
		Duration:     duration,
		Sightings:    sightings,
		KnownObjects: knownObjects,
		NamedPeople:  namedPeople,
	}
	reviewQuery := eventReviewQuery(r.URL.Query())
	if prevID != "" {
		data.PrevURL = "/event.html?id=" + url.QueryEscape(prevID)
		if reviewQuery != "" {
			data.PrevURL += "&" + reviewQuery
		}
	}
	if nextID != "" {
		data.NextURL = "/event.html?id=" + url.QueryEscape(nextID)
		if reviewQuery != "" {
			data.NextURL += "&" + reviewQuery
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := eventDetailTmpl.Execute(w, data); err != nil {
		slog.Error("template error", "error", err)
	}
}

func (s *Server) handleSystemStatusPartial(w http.ResponseWriter, _ *http.Request) {
	statuses := s.cameraStatuses()
	onlineCount := 0
	for _, st := range statuses {
		if st.Online {
			onlineCount++
		}
	}

	type topnavData struct {
		Total  int
		Online int
	}

	data := topnavData{Total: len(statuses), Online: onlineCount}

	w.Header().Set("Content-Type", "text/html")
	if err := systemStatusTmpl.Execute(w, data); err != nil {
		slog.Error("template error", "error", err)
	}
}

func (s *Server) handleSystemPartial(w http.ResponseWriter, _ *http.Request) {
	statuses := s.cameraStatuses()
	onlineCount := 0
	for _, st := range statuses {
		if st.Online {
			onlineCount++
		}
	}

	openH264 := openH264StatusInfo()
	decoderName := "native Go"
	if openH264.Available {
		decoderName = "native Go + OpenH264"
	}

	uptime := time.Since(startTime)
	uptimeStr := formatDuration(uptime)

	stats := s.recorder.StorageStats()
	totalBytes := stats.TotalBytes

	type storageEntry struct {
		Camera  string
		Bytes   int64
		Display string
		Percent float64
	}

	storageEntries := make([]storageEntry, 0, len(stats.CameraStats))
	for cam, bytes := range stats.CameraStats {
		pct := float64(0)
		if totalBytes > 0 {
			pct = float64(bytes) / float64(totalBytes) * 100
		}
		storageEntries = append(storageEntries, storageEntry{
			Camera:  cam,
			Bytes:   bytes,
			Display: formatBytes(bytes),
			Percent: pct,
		})
	}

	type sysData struct {
		Version             string
		Uptime              string
		Decoder             string
		GoVersion           string
		CameraCount         int
		OnlineCount         int
		Statuses            []camera.CameraStatus
		TotalBytes          int64
		TotalStr            string
		SegCount            int
		Storage             []storageEntry
		UpdateAvailable     bool
		UpdateVersion       string
		UpdateURL           string
		OpenH264            media.OpenH264Status
		OpenH264UI          openH264Presentation
		OpenH264SourceLabel string
	}

	data := sysData{
		Version:             s.version,
		Uptime:              uptimeStr,
		Decoder:             decoderName,
		GoVersion:           runtime.Version(),
		CameraCount:         len(statuses),
		OnlineCount:         onlineCount,
		Statuses:            statuses,
		TotalBytes:          totalBytes,
		TotalStr:            formatBytes(totalBytes),
		SegCount:            stats.SegmentCount,
		Storage:             storageEntries,
		OpenH264:            openH264,
		OpenH264UI:          describeOpenH264Status(openH264),
		OpenH264SourceLabel: formatOpenH264Source(openH264.Source),
	}

	if s.updateChecker != nil {
		us := s.updateChecker.Status()
		if us.UpdateAvailable && !us.Dismissed {
			data.UpdateAvailable = true
			data.UpdateVersion = us.Latest
			data.UpdateURL = us.URL
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := systemTmpl.Execute(w, data); err != nil {
		slog.Error("template error", "error", err)
	}
}

func formatOpenH264Source(source string) string {
	switch source {
	case "environment":
		return "Environment override"
	case "system":
		return "System library"
	case "installed":
		return "Vedetta installed"
	default:
		return ""
	}
}

const systemPartialTemplate = `<div class="sys-card">
  <div class="sys-card-header">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="3"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>
    System Info
  </div>
  <div class="sys-card-body">
    <div class="sys-row"><span class="key">Version</span><span class="val">{{.Version}}{{if .UpdateAvailable}} <a href="/settings.html" style="color:var(--accent);font-size:0.85em;margin-left:0.5em">{{.UpdateVersion}} available</a>{{end}}</span></div>
    <div class="sys-row"><span class="key">Uptime</span><span class="val">{{.Uptime}}</span></div>
    <div class="sys-row"><span class="key">Decoder</span><span class="val">{{.Decoder}}</span></div>
    <div class="sys-row"><span class="key">Go</span><span class="val">{{.GoVersion}}</span></div>
    <div class="sys-row"><span class="key">Cameras</span><span class="val">{{.CameraCount}} ({{.OnlineCount}} online)</span></div>
  </div>
</div>
<div class="sys-card">
  <div class="sys-card-header">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M5 5h14v14H5z"/><path d="M9 9h6v6H9z"/><path d="M9 1v4M15 1v4M9 19v4M15 19v4M19 9h4M19 14h4M1 9h4M1 14h4"/></svg>
    Codec Status
  </div>
  <div class="sys-card-body">
    <div class="sys-row"><span class="key">OpenH264</span><span class="val">{{if eq .OpenH264UI.BadgeTone "success"}}<span class="green">{{.OpenH264UI.Badge}}</span>{{else if eq .OpenH264UI.BadgeTone "error"}}<span class="red">{{.OpenH264UI.Badge}}</span>{{else}}{{.OpenH264UI.Badge}}{{end}}</span></div>
    <div class="sys-note">
      <div>{{.OpenH264UI.Headline}}</div>
      {{if .OpenH264UI.Detail}}<div class="sys-note-detail">{{.OpenH264UI.Detail}}</div>{{end}}
    </div>
    {{if .OpenH264SourceLabel}}<div class="sys-row"><span class="key">Source</span><span class="val">{{.OpenH264SourceLabel}}</span></div>{{end}}
    {{if .OpenH264.Version}}<div class="sys-row"><span class="key">Version</span><span class="val">{{.OpenH264.Version}}</span></div>{{end}}
    {{if .OpenH264.Path}}<div class="sys-row"><span class="key">Path</span><span class="val sys-path">{{.OpenH264.Path}}</span></div>{{end}}
    {{if .OpenH264UI.ShowDiagnostics}}<div class="sys-card-section"><details style="width:100%"><summary style="cursor:pointer;color:var(--text-secondary)">Technical details</summary><div class="sys-note-detail" style="word-break:break-word">{{.OpenH264UI.Diagnostic}}</div></details></div>{{end}}
    {{if .OpenH264UI.ShowInstall}}
    <div class="sys-card-section">
      {{if .OpenH264.Installing}}
      <button class="btn btn-sm" disabled>Installing…</button>
      {{else}}
      <button class="btn btn-sm" data-action-click="installOpenH264FromSystem(this)">{{.OpenH264UI.ActionLabel}}</button>
      {{end}}
    </div>
    {{end}}
  </div>
</div>
<div class="sys-card">
  <div class="sys-card-header">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 19a2 2 0 0 1-2 2H3a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4l2-3h6l2 3h4a2 2 0 0 1 2 2z"/><circle cx="12" cy="13" r="4"/></svg>
    Camera Status
  </div>
  <div class="sys-card-body">
    <table style="width:100%">
      <thead><tr><th style="text-align:left">Camera</th><th style="text-align:left">Status</th></tr></thead>
      <tbody>
      {{range .Statuses}}<tr>
        <td>{{displayName .Name}}</td>
        <td>{{if .Online}}<span class="green">Online</span>{{else}}<span class="red">Offline</span>{{end}}</td>
      </tr>{{end}}
      </tbody>
    </table>
  </div>
</div>
<div class="sys-card">
  <div class="sys-card-header">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/></svg>
    Storage
  </div>
  <div class="sys-card-body">
    <div class="sys-row"><span class="key">Total</span><span class="val">{{.TotalStr}}</span></div>
    <div class="sys-row"><span class="key">Segments</span><span class="val">{{.SegCount}}</span></div>
    {{range .Storage}}<div style="margin-top: 0.5rem">
      <div class="sys-row"><span class="key">{{displayName .Camera}}</span><span class="val">{{.Display}}</span></div>
      <div class="storage-bar"><div class="storage-bar-fill" style="width: {{printf "%.0f" .Percent}}%"></div></div>
    </div>{{end}}
  </div>
</div>`
