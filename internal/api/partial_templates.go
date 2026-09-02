package api

import (
	"html/template"
)

// Every htmx partial is parsed once here, at package init, rather than on each
// request. htmx polls these endpoints continuously, so a per-request parse was
// paid on every poll; parsing at init also turns a typo in a template into a
// startup failure instead of a 500 the first time a page is opened.
//
// activityEvidenceURLPlaceholder exists only so the activity-detail template can
// be parsed without a request. That one helper needs the current request and the
// activity being rendered, so the handler clones the parsed template and binds
// the real function; the placeholder is never executed.
var activityEvidenceURLPlaceholder = template.FuncMap{
	"activityEvidenceURL": func(string) string { return "" },
}

var cameraGridTmpl = template.Must(template.New("grid").Parse(`{{range .}}<div class="cam-card{{if .Stopped}} cam-stopped{{end}}" data-camera-name="{{.Name}}" data-action-click="location.href='/camera.html?name={{.Name}}'" role="listitem" aria-label="{{.DisplayName}} camera">
  <div class="cam-preview">
    <img src="/api/cameras/{{.Name}}/snapshot" alt="{{.DisplayName}} camera" loading="lazy">
    <span class="cam-last-seen" data-ts="{{.LastSeen}}"></span>
    <div class="cam-live-badge">
      {{if .Stopped}}<span class="cam-live-dot stopped"></span>STOPPED{{else if .Online}}<span class="cam-live-dot"></span>LIVE{{else if .Sleeping}}<span class="cam-live-dot sleeping"></span>SLEEPING{{else}}<span class="cam-live-dot offline"></span>OFFLINE{{end}}
    </div>
    <button class="cam-toggle-btn" data-action-click="event.stopPropagation(); toggleCamera('{{.Name}}', {{.Stopped}}, this)" title="{{if .Stopped}}Start camera{{else}}Stop camera{{end}}" aria-label="{{if .Stopped}}Start{{else}}Stop{{end}} {{.DisplayName}}">
      {{if .Stopped}}<svg viewBox="0 0 24 24" fill="currentColor" width="16" height="16"><polygon points="5 3 19 12 5 21 5 3"/></svg>{{else}}<svg viewBox="0 0 24 24" fill="currentColor" width="16" height="16"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>{{end}}
    </button>
  </div>
  <div class="cam-footer">
    <span class="cam-name">{{.DisplayName}}</span>
    {{if .Doorbell}}{{if .Stopped}}<span class="cam-doorbell-action is-disabled" aria-disabled="true" title="Start camera to open doorbell">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4"/></svg>
      <span>Open doorbell</span>
    </span>{{else}}<a class="cam-doorbell-action" href="/doorbell-answer.html?camera={{.Name}}" data-action-click="event.stopPropagation()" aria-label="Open doorbell for {{.DisplayName}}">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4"/></svg>
      <span>Open doorbell</span>
    </a>{{end}}{{end}}
  </div>
</div>{{end}}`))

var dashboardStatsTmpl = template.Must(template.New("stats").Parse(
	`<div class="stat-card"><div class="stat-label">Cameras</div><div class="stat-value">{{.CameraCount}}</div></div>` +
		`<div class="stat-card"><div class="stat-label">Online</div><div class="stat-value green">{{.OnlineCount}}</div></div>` +
		`<div class="stat-card"><div class="stat-label">Events<span class="stat-label-q"> Today</span></div><div class="stat-value">{{.EventsToday}}</div></div>` +
		`<div class="stat-card"><div class="stat-label">Storage</div><div class="stat-value">{{.Storage}}</div></div>`))

var activitiesGalleryTmpl = template.Must(template.New("activities").Funcs(partialFuncs).Funcs(activityFuncs).Parse(
	`{{range .}}` +
		`<a class="event-card activity-card" href="/activity.html?id={{.ID}}" data-event-time="{{.StartTime.UTC.Format "2006-01-02T15:04:05Z"}}" data-activity-category="{{.Category}}" data-activity-state="{{.State}}">` +
		`<div class="event-thumb activity-thumb">` +
		`{{if .PrimaryEvent.SnapshotAvailable}}<img src="/api/events/{{.PrimaryEvent.ID}}/snapshot" alt="{{activityTitle .}} at {{displayName .CameraName}}" loading="lazy">` +
		`{{else}}<img src="/api/cameras/{{.CameraName}}/snapshot" alt="{{activityTitle .}} at {{displayName .CameraName}}" loading="lazy">{{end}}` +
		`{{if gt .EventCount 1}}<span class="activity-evidence-count">{{.EventCount}} events</span>{{end}}` +
		`{{if eq .State "open"}}<span class="activity-state-badge">{{activityStateLabel .State}}</span>{{end}}` +
		`{{if .MissedDoorbell}}<span class="event-missed-badge">missed ring</span>{{else if .HasDoorbell}}<span class="event-answered-badge">doorbell</span>{{end}}` +
		`</div>` +
		`<div class="event-card-footer">` +
		`<div class="event-card-heading"><span class="event-card-title">{{activityTitle .}}</span><span class="event-time">{{timeAgo .StartTime}}</span></div>` +
		`<div class="event-card-context"><span class="event-camera-name">{{displayName .CameraName}}</span><span>{{activityDuration .}}</span>` +
		`{{with activityFacets .Zones}}<span>{{.}}</span>{{end}}` +
		`{{if eq .Category "detection"}}<span class="event-tier">Low priority</span>{{end}}</div>` +
		`</div></a>{{end}}`))

var activityDetailTmpl = template.Must(template.New("activity-detail").Funcs(partialFuncs).Funcs(activityFuncs).Funcs(activityEvidenceURLPlaceholder).Parse(
	`{{$activity := .}}<div class="activity-detail-root" data-activity-id="{{.ID}}" data-activity-state="{{.State}}" data-activity-camera="{{.CameraName}}" data-activity-time="{{.StartTime.UTC.Format "2006-01-02T15:04:05Z"}}">` +
		`<div class="page-header activity-page-header"><div><h1>{{activityTitle .}}</h1><p>{{displayName .CameraName}} · {{formatTime .StartTime}}</p><span class="activity-detail-state {{.State}}">{{activityStateLabel .State}}{{if eq .State "open"}} · updates as evidence arrives{{end}}</span></div>` +
		`<a class="btn btn-secondary" href="/events.html">Back to Activity</a></div>` +
		`<div class="activity-review-layout"><section class="activity-primary" aria-label="Primary evidence">` +
		`<div class="activity-primary-media">{{if .PrimaryEvent.ClipAvailable}}<video controls preload="metadata" poster="/api/events/{{.PrimaryEvent.ID}}/snapshot"><source src="/api/events/{{.PrimaryEvent.ID}}/clip" type="video/mp4"></video>` +
		`{{else if .PrimaryEvent.SnapshotAvailable}}<img src="/api/events/{{.PrimaryEvent.ID}}/snapshot" alt="{{activityTitle .}} at {{displayName .CameraName}}">` +
		`{{else}}<img src="/api/cameras/{{.CameraName}}/snapshot" alt="Current view of {{displayName .CameraName}}">{{end}}</div>` +
		`<div class="activity-summary" aria-label="Activity summary">` +
		`<div><span>When</span><strong>{{formatTime .StartTime}}</strong></div><div><span>Camera</span><strong>{{displayName .CameraName}}</strong></div>` +
		`<div><span>Status</span><strong>{{activityStateLabel .State}}</strong></div><div><span>Duration</span><strong>{{activityDuration .}}</strong></div><div><span>Evidence</span><strong>{{.EventCount}} {{if eq .EventCount 1}}event{{else}}events{{end}}</strong></div>` +
		`{{with activityFacets .Zones}}<div><span>Zones</span><strong>{{.}}</strong></div>{{end}}` +
		`{{with activityFacets .Labels}}<div><span>Detected</span><strong>{{.}}</strong></div>{{end}}</div></section>` +
		`<aside class="activity-evidence" aria-labelledby="evidence-title"><div class="activity-evidence-heading"><h2 id="evidence-title">Evidence</h2><p>Every detection included in this activity.</p></div>` +
		`<div class="activity-grouping"><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.75"/><path d="M12 11v5m0-8v.01" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg><div><strong>Why these belong together</strong><span>{{.Grouping.Explanation}}</span></div></div>` +
		`<div class="activity-evidence-list">{{range .Events}}<div class="activity-evidence-row"><a class="activity-evidence-item" href="{{activityEvidenceURL .ID}}">` +
		`<div class="activity-evidence-thumb">{{if .SnapshotAvailable}}<img src="/api/events/{{.ID}}/snapshot" alt="{{displayName .Label}} evidence">{{else}}<span aria-hidden="true"></span>{{end}}</div>` +
		`<div><strong>{{if .SubLabel}}{{.SubLabel}}{{else}}{{displayName .Label}}{{end}}</strong><span>{{formatTime .Timestamp}}{{with .ZoneName}} · {{displayName .}}{{end}}</span></div>` +
		`<svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><path d="m9 18 6-6-6-6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg></a>` +
		`<button class="btn btn-ghost activity-evidence-correction" type="button" data-activity-evidence-action="exclude" data-event-id="{{.ID}}" aria-label="Exclude {{displayName .Label}} evidence from this activity" title="{{if eq $activity.EventCount 1}}An Activity must keep at least one evidence event{{else}}Keep the raw event, but remove it from this Activity{{end}}" {{if eq $activity.EventCount 1}}disabled{{end}}>Exclude</button></div>{{end}}</div>` +
		`{{with .ExcludedEvidence}}<section class="activity-excluded" aria-labelledby="excluded-evidence-title"><div class="activity-excluded-heading"><h3 id="excluded-evidence-title">Excluded evidence</h3><p>Kept as raw evidence and available to restore.</p></div><div class="activity-excluded-list">{{range .}}<div class="activity-evidence-row is-excluded"><a class="activity-evidence-item" href="{{activityEvidenceURL .Event.ID}}">` +
		`<div class="activity-evidence-thumb">{{if .Event.SnapshotAvailable}}<img src="/api/events/{{.Event.ID}}/snapshot" alt="Excluded {{displayName .Event.Label}} evidence">{{else}}<span aria-hidden="true"></span>{{end}}</div>` +
		`<div><strong>{{if .Event.SubLabel}}{{.Event.SubLabel}}{{else}}{{displayName .Event.Label}}{{end}}</strong><span>{{.Reason}} · by {{.Actor}}</span></div>` +
		`<svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><path d="m9 18 6-6-6-6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg></a>` +
		`<button class="btn btn-ghost activity-evidence-correction" type="button" data-activity-evidence-action="restore" data-event-id="{{.Event.ID}}" aria-label="Restore {{displayName .Event.Label}} evidence to this activity">Restore</button></div>{{end}}</div></section>{{end}}` +
		`</aside></div></div>`))

var eventsGalleryTmpl = template.Must(template.New("gallery").Funcs(partialFuncs).Parse(
	`{{range .}}` +
		`<a class="event-card" href="/event.html?id={{.ID}}" role="listitem" data-event-time="{{.Timestamp.UTC.Format "2006-01-02T15:04:05Z"}}" data-event-category="{{.Category}}">` +
		`<div class="event-thumb">` +
		`{{if .SnapshotAvailable}}<img src="/api/events/{{.ID}}/snapshot" alt="{{.Label}} detected by {{displayName .CameraName}}" loading="lazy">` +
		`{{else}}<img src="/api/cameras/{{.CameraName}}/snapshot" alt="{{.Label}} detected by {{displayName .CameraName}}" loading="lazy">{{end}}` +
		`{{if eq .Kind "doorbell"}}{{if .AnsweredAt.IsZero}}<span class="event-missed-badge">missed</span>{{else}}<span class="event-answered-badge" title="answered by {{.AnsweredBy}}">answered</span>{{end}}{{end}}` +
		`</div>` +
		`<div class="event-card-footer">` +
		`<div class="event-card-heading"><span class="event-card-title">{{if .SubLabel}}{{.SubLabel}}{{else}}{{displayName .Label}}{{end}}</span><span class="event-time">{{timeAgo .Timestamp}}</span></div>` +
		`<div class="event-card-context">` +
		`<span class="event-camera-name">{{displayName .CameraName}}</span>` +
		`{{if .SubLabel}}<span>{{displayName .Label}}</span>{{end}}` +
		`{{with eventDuration .}}<span>{{.}}</span>{{end}}` +
		`{{if eq .Category "detection"}}<span class="event-tier">Low priority</span>{{end}}` +
		`{{if lt .Score 0.7}}<span class="event-confidence{{if lt .Score 0.5}} low{{end}}" title="Detection confidence">{{scorePercent .Score}}</span>{{end}}` +
		`</div>` +
		`</div>` +
		`</a>{{end}}`))

var eventDetailTmpl = template.Must(template.New("detail").Funcs(partialFuncs).Parse(
	`<div class="event-detail-root" data-event-camera="{{.CameraName}}" data-event-label="{{.Label}}" data-event-time="{{.Timestamp.UTC.Format "2006-01-02 15:04:05 UTC"}}"></div>` +
		`<div class="page-header"><h1>{{displayName .Label}} Detection</h1></div>` +
		`<div class="event-detail-layout">` +
		`<div class="event-media">` +
		`{{if .SnapshotAvailable}}<div class="detection-overlay-wrap" id="detection-wrap">` +
		`<img id="event-snapshot" src="/api/events/{{.ID}}/snapshot" alt="event snapshot" ` +
		`data-box-x1="{{index .Box 0}}" data-box-y1="{{index .Box 1}}" ` +
		`data-box-x2="{{index .Box 2}}" data-box-y2="{{index .Box 3}}" ` +
		`data-label="{{.Label}}" data-sub-label="{{.SubLabel}}" ` +
		`data-score="{{scorePercent .Score}}" data-event-id="{{.ID}}" ` +
		`data-render-detection-overlay="true">` +
		`</div>` +
		`{{else}}<img id="event-snapshot" src="/api/cameras/{{.CameraName}}/snapshot" alt="event">{{end}}` +
		// Prefer the pre-generated per-event clip (a plain MP4 file)
		// over the HLS re-segmenter path when both are available. The
		// clip plays natively in every browser including iOS Safari;
		// the HLS path splits multi-track moofs into single-track for
		// MSE/hls.js compatibility, which iOS's native HLS decoder
		// rejects with a silent black-frame failure. For longer
		// scrub-through-history viewing, the sidebar still offers
		// "View in Recording" which opens the camera playback page.
		`{{if .ClipAvailable}}<button type="button" class="play-overlay" id="play-overlay" aria-label="Play clip" data-action-click="playEventClip(this, '{{.ID}}')">` +
		`<svg viewBox="0 0 24 24" fill="white" width="64" height="64" aria-hidden="true"><polygon points="5 3 19 12 5 21 5 3"/></svg>` +
		`</button>{{else if .HasRecording}}<button type="button" class="play-overlay" id="play-overlay" aria-label="Play recording" data-action-click="playEventRecording(this, '{{.CameraName}}', '{{.Timestamp.Format "2006-01-02T15:04:05Z07:00"}}')">` +
		`<svg viewBox="0 0 24 24" fill="white" width="64" height="64" aria-hidden="true"><polygon points="5 3 19 12 5 21 5 3"/></svg>` +
		`</button>{{end}}` +
		`</div>` +
		`<div class="event-sidebar">` +
		`<div class="event-nav">` +
		`{{if .PrevID}}<a href="{{.PrevURL}}" class="btn" data-prev-id="{{.PrevID}}">&#8592; Previous</a>{{else}}<button class="btn" disabled>&#8592; Previous</button>{{end}}` +
		`{{if .NextID}}<a href="{{.NextURL}}" class="btn" data-next-id="{{.NextID}}">Next &#8594;</a>{{else}}<button class="btn" disabled>Next &#8594;</button>{{end}}` +
		`</div>` +
		`<div class="meta-card">` +
		`<div class="meta-card-header">Details</div>` +
		`<div class="meta-row"><span class="key">Camera</span><span class="val">{{.CameraName}}</span></div>` +
		`<div class="meta-row"><span class="key">Label</span><span class="val">{{.Label}}</span></div>` +
		`{{if .SubLabel}}<div class="meta-row"><span class="key">Identity</span><span class="val">{{.SubLabel}}</span></div>{{end}}` +
		`<div class="meta-row"><span class="key">Confidence</span><span class="val">{{scorePercent .Score}}</span></div>` +
		`<div class="meta-row"><span class="key">Time</span><span class="val">{{formatTime .Timestamp}} <span class="text-tertiary">({{timeAgo .Timestamp}})</span></span></div>` +
		`{{if .Duration}}<div class="meta-row"><span class="key">Duration</span><span class="val">{{.Duration}}</span></div>{{end}}` +
		`<div class="meta-row"><span class="key">Event ID</span><span class="val mono">{{.ID}}</span></div>` +
		`</div>` +
		`<div class="meta-card">` +
		`<div class="meta-card-header">Downloads</div>` +
		`{{if .ClipAvailable}}<a href="/api/events/{{.ID}}/clip?download=1" download class="download-row">` +
		`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>` +
		` Download Clip</a>{{end}}` +
		`{{if .SnapshotAvailable}}<a href="/api/events/{{.ID}}/snapshot?download=1" download class="download-row">` +
		`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>` +
		` Download Snapshot</a>{{end}}` +
		`{{if not .ClipAvailable}}{{if not .SnapshotAvailable}}<div class="download-row disabled">No media available</div>{{end}}{{end}}` +
		`{{if .HasRecording}}<a href="{{.RecordingURL}}" class="download-row">` +
		`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polygon points="5 3 19 12 5 21 5 3"/></svg>` +
		` View in Recording</a>{{end}}` +
		`</div>` +
		`{{if .Sightings}}<div class="meta-card">` +
		`<div class="meta-card-header">Recognized</div>` +
		`{{range .Sightings}}<div class="meta-row"><span class="key">{{.ObjectName}}</span><span class="val">{{scorePercent (toFloat32 .Similarity)}}</span></div>{{end}}` +
		`</div>{{end}}` +
		`{{if .SnapshotAvailable}}<div class="meta-card">` +
		`{{if .SubLabel}}<div class="meta-card-header">Tracked</div>` +
		`<div class="tracked-row">` +
		`<img src="/api/events/{{.ID}}/detection-crop" alt="detection" class="detect-crop-thumb detect-crop-thumb--tracked">` +
		`<span class="tracked-name">{{.SubLabel}}</span>` +
		`</div>` +
		`{{else}}<div class="meta-card-header">Identify</div>` +
		`<div class="identify-row">` +
		`<img src="/api/events/{{.ID}}/detection-crop" alt="detection" class="detect-crop-thumb detect-crop-thumb--identify">` +
		`<label for="identify-search" class="sr-only">Search or add {{.Label}} identity</label>` +
		`<input type="text" id="identify-search" class="person-name-input" placeholder="Search or add new {{.Label}}..." ` +
		`data-action-input="filterIdentifyResults(this.value)" ` +
		`data-identify-enter-id="{{.ID}}" data-identify-enter-label="{{.Label}}">` +
		`</div>` +
		`<div id="identify-grid" data-event-id="{{.ID}}" data-label="{{.Label}}"></div>` +
		`{{end}}` +
		`</div>{{end}}` +
		`</div>` +
		`</div>`))

var systemStatusTmpl = template.Must(template.New("sysstatus").Parse(
	`<span class="topnav-stat"><span class="value">{{.Total}}</span> cameras</span>` +
		`<span class="topnav-stat"><span class="value green">{{.Online}}</span> online</span>`))

var systemTmpl = template.Must(template.New("system").Funcs(partialFuncs).Parse(systemPartialTemplate))
