const imageBody = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360" fill="#151b23"/><rect x="250" y="85" width="140" height="190" rx="18" fill="#52677a"/></svg>';

function activityCard(id, label, camera, timestamp) {
  const image = 'data:image/svg+xml,' + encodeURIComponent(imageBody);
  return `<a class="event-card activity-card" href="/activity.html?id=act_${id}" role="listitem" data-event-time="${timestamp}" data-activity-category="alert" data-activity-state="finalized">
    <div class="event-thumb"><img src="${image}" alt="${label} detected by ${camera}"></div>
    <div class="event-card-footer">
      <div class="event-card-heading"><span class="event-card-title">${label}</span><span class="event-time">2m ago</span></div>
      <div class="event-card-context"><span class="event-camera-name">${camera}</span><span>8s</span></div>
    </div>
  </a>`;
}

async function mockApplication(page) {
  const now = new Date('2026-08-13T10:00:00Z');
  const gallery = [
    activityCard('event-1', 'Ruben', 'Front Door', now.toISOString()),
    activityCard('event-2', 'Car', 'Driveway', new Date(now - 60_000).toISOString()),
    activityCard('event-3', 'Dog', 'Back Garden', new Date(now - 86_400_000).toISOString()),
  ].join('');

  await page.route('**/*', async route => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;

    if (/\/(snapshot|crop|detection-crop)$/.test(path)) {
      return route.fulfill({ status: 200, contentType: 'image/svg+xml', body: imageBody });
    }

    if (path.startsWith('/partials/')) {
      if (path === '/partials/dashboard-stats') {
        return route.fulfill({ contentType: 'text/html', body: '<div class="stat-card"><span class="stat-label">Online</span><strong class="stat-value">3</strong></div><div class="stat-card"><span class="stat-label">Events</span><strong class="stat-value">42</strong></div>' });
      }
      if (path === '/partials/camera-grid') {
        return route.fulfill({ contentType: 'text/html', body: '<article class="cam-card"><a href="/camera.html?name=front_door" aria-label="Open Front Door camera"><div class="cam-media"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Front Door camera snapshot"></div><div class="cam-footer"><strong class="cam-name">Front Door</strong><span class="badge badge-success">Online</span></div></a></article><article class="cam-card"><a href="/camera.html?name=driveway" aria-label="Open Driveway camera"><div class="cam-media"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Driveway camera snapshot"></div><div class="cam-footer"><strong class="cam-name">Driveway</strong><span class="badge">Sleeping</span></div></a></article>' });
      }
      if (path === '/partials/activities-gallery') {
        return route.fulfill({ contentType: 'text/html', headers: { 'X-Total-Count': '3' }, body: gallery });
      }
      if (path.startsWith('/partials/activity/')) {
        return route.fulfill({ contentType: 'text/html', body: '<div class="activity-detail-root" data-activity-id="act_event-1" data-activity-state="finalized" data-activity-camera="front_door" data-activity-time="2026-08-13T10:00:00Z"><div class="page-header activity-page-header"><div><h1>Ruben</h1><p>Front Door · Today at 10:00</p><span class="activity-detail-state finalized">Finalized</span></div><a class="btn btn-secondary" href="/events.html">Back to Activity</a></div><div class="activity-review-layout"><section class="activity-primary" aria-label="Primary evidence"><div class="activity-primary-media"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Ruben at Front Door"></div><div class="activity-summary" aria-label="Activity summary"><div><span>When</span><strong>Today at 10:00</strong></div><div><span>Camera</span><strong>Front Door</strong></div><div><span>Status</span><strong>Finalized</strong></div><div><span>Duration</span><strong>8s</strong></div><div><span>Evidence</span><strong>2 events</strong></div></div></section><aside class="activity-evidence" aria-labelledby="evidence-title"><div class="activity-evidence-heading"><h2 id="evidence-title">Evidence</h2><p>Every detection included in this activity.</p></div><div class="activity-evidence-list"><a class="activity-evidence-item" href="/event.html?id=event-1"><div class="activity-evidence-thumb"><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Person evidence"></div><div><strong>Ruben</strong><span>Today at 10:00</span></div></a></div></aside></div></div>' });
      }
      if (path.startsWith('/partials/event/')) {
        return route.fulfill({ contentType: 'text/html', body: '<article class="event-detail-card"><header><h2>Person at Front Door</h2><p class="text-tertiary">Today at 10:00 · 92% confidence</p></header><img src="data:image/svg+xml,' + encodeURIComponent(imageBody) + '" alt="Person detected at the Front Door"><div class="event-detail-actions"><a class="btn btn-primary" href="/recordings.html">View recording</a><button class="btn btn-ghost" type="button">Download snapshot</button></div></article>' });
      }
      if (path === '/partials/system') {
        return route.fulfill({ contentType: 'text/html', body: '<section class="sys-card"><h3 class="sys-card-header">System status</h3><div class="sys-card-body"><div class="sys-row"><span class="key">Recorder</span><strong class="val">Healthy</strong></div><div class="sys-row"><span class="key">Storage</span><strong class="val">62% used</strong></div></div></section>' });
      }
      if (path === '/partials/system-status') {
        return route.fulfill({ contentType: 'text/html', body: '<span class="conn-dot ok"></span><span>Connected</span>' });
      }
      return route.fulfill({ contentType: 'text/html', body: '' });
    }

    if (path.startsWith('/api/')) {
      let json = {};
      if (path === '/api/health') json = { status: 'ok', checks: {} };
      else if (path === '/api/cameras') json = { items: [{ name: 'front_door', online: true }, { name: 'driveway', online: false, sleeping: true }] };
      else if (path === '/api/cameras/manage') json = { cameras: [{ index: 0, name: 'front_door', url: 'rtsp://camera/live', record_url: '', enabled: true, has_credentials: true, detect: { width: 640, height: 480, fps: 5, enabled: true }, record: { width: 1920, height: 1080, fps: 15 } }] };
      else if (path === '/api/cameras/front_door') json = { name: 'front_door', online: false, sleeping: true, last_seen: '2026-08-13T09:55:00Z' };
      else if (path.endsWith('/timeline')) json = { segments: [], activity: [], events: [] };
      else if (path.endsWith('/zones')) json = { items: [] };
      else if (path === '/api/activities/counts') json = { total: 1284, today: 42, open: 1, finalized: 1283 };
      else if (path === '/api/events') json = { items: [{ id: 'event-4', camera: 'Front Door', label: 'person', score: 0.91, timestamp: now.toISOString(), snapshot_available: true, sub_label: '' }] };
      else if (path === '/api/people') json = { items: [{ id: 1, name: 'Ruben', face_count: 8, appearance_count: 16, best_face_id: 1, ignore: false, created_at: now.toISOString() }] };
      else if (path === '/api/faces/unmatched') json = { items: [{ id: 2, event_id: 'event-4', camera: 'Front Door', timestamp: now.toISOString() }] };
      else if (path === '/api/objects') json = { items: [{ id: 1, name: 'Family car', label: 'car', crop_path: 'car.jpg', created_at: now.toISOString(), match_threshold: 0.72 }] };
      else if (/^\/api\/objects\/1\/(references|sightings)$/.test(path)) json = { items: [] };
      else if (path === '/api/recordings/calendar') json = { days: [13] };
      else if (path === '/api/recordings/summary') json = { total_bytes: 4_200_000_000, cameras: [{ name: 'front_door', total_bytes: 4_200_000_000, segments: [{ start_time: '2026-08-13T08:00:00Z', end_time: '2026-08-13T10:00:00Z', size: 4_200_000_000 }] }] };
      else if (path === '/api/storage') json = { recording_paused: false, recording: { used_bytes: 620_000_000_000, disk_available: 380_000_000_000, root: '/recordings' }, snapshots: { same_filesystem_as_recording: true, used_bytes: 12_000_000_000, disk_available: 380_000_000_000, root: '/snapshots' }, cameras: [{ name: 'front_door', segment_bytes: 420_000_000_000, clip_bytes: 8_000_000_000, oldest_segment: '2026-08-01', last_7d_bytes: 140_000_000_000, effective_retain_days: 14, per_day: [{ date: '2026-08-13', bytes: 20_000_000_000 }] }], recompression: { enabled: true, last_run: '2026-08-13T02:00:00Z', segments_recompressed: 12, clips_recompressed: 3, bytes_reclaimed: 4_000_000_000, is_running: false } };
      else if (path === '/api/storage/audit') json = [{ ts: now.toISOString(), actor: 'retention', files: 18, bytes: 2_000_000_000, scope: { camera: 'front_door' } }];
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(json) });
    }

    return route.continue();
  });
}


module.exports = { mockApplication };
