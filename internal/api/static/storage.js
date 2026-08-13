const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function fmtBytes(n) {
  if (n < 1024) return n + " B";
  const units = ["KB","MB","GB","TB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 10 ? 0 : 1) + " " + units[i];
}

function parseBytesInput(s) {
  const m = /^(\d+(?:\.\d+)?)\s*(B|KB|MB|GB|TB)?$/i.exec(s.trim());
  if (!m) return null;
  const n = parseFloat(m[1]);
  const u = (m[2] || "B").toUpperCase();
  return Math.round(n * ({B:1, KB:1024, MB:1024**2, GB:1024**3, TB:1024**4})[u]);
}

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function daysAgo(dateStr) {
  var d = new Date(dateStr + "T00:00:00Z");
  if (isNaN(d)) return "";
  var diffMs = Date.now() - d.getTime();
  var days = Math.round(diffMs / 86400000);
  if (days < 1) return "today";
  if (days === 1) return "yesterday";
  return days + "d ago";
}

function renderLoadingSkeleton() {
  var body = $("#camera-table-body");
  if (!body) return;
  var rows = "";
  for (var i = 0; i < 3; i++) {
    rows += `<tr class="storage-cam-row">
      <td><div class="skeleton" style="height:0.875rem;width:7rem"></div></td>
      <td class="num"><div class="skeleton" style="height:0.875rem;width:4rem;margin-left:auto"></div></td>
      <td class="num"><div class="skeleton" style="height:0.875rem;width:4rem;margin-left:auto"></div></td>
      <td><div class="skeleton" style="height:0.875rem;width:5rem"></div></td>
      <td class="num"><div class="skeleton" style="height:0.875rem;width:4rem;margin-left:auto"></div></td>
      <td></td>
    </tr>
    <tr class="storage-spark-row"><td colspan="6"><div class="skeleton" style="height:36px;max-width:420px"></div></td></tr>`;
  }
  body.innerHTML = rows;
}

async function loadSummary() {
  var firstLoad = !$("#camera-table-body").dataset.loaded;
  if (firstLoad) renderLoadingSkeleton();

  const r = await fetch("/api/storage");
  if (!r.ok) return;
  const data = await r.json();

  $("#camera-table-body").dataset.loaded = "1";
  $("#recording-paused-banner").hidden = !data.recording_paused;

  renderSummaryCards(data);
  renderCameraTable(data.cameras || []);
  renderRecompression(data.recompression);
}

function renderRecompression(rc) {
  const panel = $("#recompress-panel");
  if (!rc || !rc.enabled) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;

  const lastRun = rc.last_run && !rc.last_run.startsWith("0001")
    ? String(rc.last_run).replace("T", " ").slice(0, 16)
    : "never";
  $("#recompress-stats").innerHTML =
    statCard("Last run", lastRun)
    + statCard("Segments", rc.segments_recompressed || 0)
    + statCard("Clips", rc.clips_recompressed || 0)
    + statCard("Space saved", fmtBytes(rc.bytes_reclaimed || 0), "green");

  const btn = $("#recompress-run-btn");
  if (rc.is_running) {
    btn.disabled = true;
    btn.textContent = "Running…";
  } else if (!btn.dataset.busy) {
    btn.disabled = false;
    btn.textContent = "Run now";
  }
}

function statCard(label, value, tone, sub) {
  return `<div class="stat-card">
    <div class="stat-label">${esc(label)}</div>
    <div class="stat-value${tone ? " " + tone : ""}">${esc(String(value))}</div>
    ${sub ? `<div class="stat-sub">${esc(sub)}</div>` : ""}
  </div>`;
}

function diskFillClass(pct) {
  if (pct >= 90) return "danger";
  if (pct >= 70) return "warning";
  return "";
}

function renderSummaryCards({ recording, snapshots }) {
  const sameFS = snapshots.same_filesystem_as_recording;
  const used = recording.used_bytes || 0;
  const free = recording.disk_available || 0;
  const total = used + free;
  const pct = total > 0 ? Math.round(used / total * 100) : 0;

  let cards = statCard("Recordings", fmtBytes(used), "cyan")
    + statCard("Free on disk", fmtBytes(free), "green")
    + statCard("Total capacity", fmtBytes(total), "", pct + "% used");
  if (!sameFS) {
    cards += statCard("Snapshots", fmtBytes(snapshots.used_bytes))
      + statCard("Snapshots free", fmtBytes(snapshots.disk_available));
  }

  const fillClass = diskFillClass(pct);
  cards += `<div class="storage-bar-wrap">` +
    `<div class="storage-bar"><div class="storage-bar-fill${fillClass ? " " + fillClass : ""}" style="width:${pct}%"></div></div>` +
    `</div>`;

  $("#summary").innerHTML = cards;

  $("#storage-subtitle").textContent =
    `${fmtBytes(used)} used · ${fmtBytes(free)} free · ${pct}% of ${fmtBytes(total)}`;

  let roots = `Recordings: ${recording.root}`;
  if (!sameFS) roots += `  ·  Snapshots: ${snapshots.root}`;
  $("#storage-roots").textContent = roots;
}

function camDisplayName(slug) {
  return slug.replace(/[_-]/g, " ").replace(/\b\w/g, function(c) { return c.toUpperCase(); });
}

function renderCameraTable(cameras) {
  const body = $("#camera-table-body");
  if (!cameras.length) {
    body.innerHTML = `<tr><td colspan="6">
      <div class="empty-state"><p>No recorded data yet.</p></div>
    </td></tr>`;
    return;
  }
  body.innerHTML = "";
  for (const c of cameras) {
    const displayName = camDisplayName(c.name);
    const tr = document.createElement("tr");
    tr.className = "storage-cam-row";
    tr.innerHTML = `
      <td class="storage-cam-name">${esc(c.name)}</td>
      <td class="num mono">${fmtBytes(c.segment_bytes)}</td>
      <td class="num mono">${fmtBytes(c.clip_bytes)}</td>
      <td class="mono oldest-cell">${c.oldest_segment ? esc(c.oldest_segment.slice(0,10)) + '<span class="oldest-rel"> (' + esc(daysAgo(c.oldest_segment.slice(0,10))) + ')</span>' : "-"}</td>
      <td class="num mono">${fmtBytes(c.last_7d_bytes)}</td>
      <td class="storage-actions">
        <button class="btn btn-xs btn-secondary" data-action="older" data-camera="${esc(c.name)}" data-days="${c.effective_retain_days}" aria-label="Delete segments older than ${c.effective_retain_days} days for ${esc(displayName)}">Older than ${c.effective_retain_days}d</button>
        <button class="btn btn-xs btn-secondary" data-action="range" data-camera="${esc(c.name)}" aria-label="Delete date range for ${esc(displayName)}">Date range...</button>
        <button class="btn btn-xs btn-danger" data-action="clips" data-camera="${esc(c.name)}" aria-label="Delete all clips for ${esc(displayName)}">All clips</button>
      </td>
    `;
    body.appendChild(tr);

    const chartRow = document.createElement("tr");
    chartRow.className = "storage-spark-row";
    chartRow.innerHTML = `<td colspan="6">${renderPerDayBars(c.per_day)}</td>`;
    body.appendChild(chartRow);
  }
}

function renderPerDayBars(days) {
  if (!days?.length) return `<span class="storage-spark-empty">No daily data</span>`;
  const max = days.reduce((m, d) => Math.max(m, d.bytes), 0) || 1;
  const w = 9, h = 36;
  const bars = days.map((d, i) => {
    const bh = Math.max(d.bytes > 0 ? 2 : 0, (h - 2) * (d.bytes / max));
    const label = d.date || ("day " + (i + 1));
    return `<rect x="${i * w}" y="${h - bh}" width="${w - 2}" height="${bh}" rx="1"` +
      ` data-date="${esc(label)}" data-bytes="${d.bytes}">` +
      `<title>${esc(label)}: ${fmtBytes(d.bytes)}</title></rect>`;
  }).join("");
  return `<div class="storage-spark-wrap">` +
    `<svg class="storage-spark" width="${days.length * w}" height="${h}" ` +
    `viewBox="0 0 ${days.length * w} ${h}" preserveAspectRatio="none" ` +
    `role="img" aria-label="Daily recording volume, last ${days.length} days">${bars}</svg>` +
    `<div class="spark-tooltip" hidden></div>` +
    `</div>`;
}

document.addEventListener("pointermove", function(e) {
  var wrap = e.target.closest(".storage-spark-wrap");
  if (!wrap) return;
  var tip = wrap.querySelector(".spark-tooltip");
  if (!tip) return;
  var rect = e.target.closest("rect[data-date]");
  if (rect) {
    tip.textContent = rect.dataset.date + ": " + fmtBytes(Number(rect.dataset.bytes));
    tip.hidden = false;
    var wrapRect = wrap.getBoundingClientRect();
    var x = e.clientX - wrapRect.left + 8;
    var y = e.clientY - wrapRect.top - 32;
    tip.style.left = Math.min(x, wrapRect.width - tip.offsetWidth - 4) + "px";
    tip.style.top = Math.max(0, y) + "px";
  } else {
    tip.hidden = true;
  }
});

document.addEventListener("pointerleave", function(e) {
  var wrap = e.target.closest(".storage-spark-wrap");
  if (!wrap) return;
  var tip = wrap.querySelector(".spark-tooltip");
  if (tip) tip.hidden = true;
}, true);

document.addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-action]");
  if (!btn) return;
  const camera = btn.dataset.camera;
  const action = btn.dataset.action;

  if (action === "older") {
    showInputModal(
      `Delete old segments: ${camera}`,
      "Delete recorded segments older than how many days?",
      btn.dataset.days,
      (val) => {
        const days = parseInt(val, 10);
        if (!days || days < 1) { toast("Enter a positive number of days.", "error"); return; }
        confirmAndDelete({ target: "segments", camera, older_than_days: days });
      },
    );
  } else if (action === "range") {
    showInputModal(
      `Delete a date range: ${camera}`,
      "Choose the first day to delete (UTC).",
      "",
      (from) => {
        if (!/^\d{4}-\d{2}-\d{2}$/.test(from)) { toast("Choose a valid start date.", "error"); return; }
        showInputModal(
          `Delete a date range: ${camera}`,
          `Choose the last day to delete, starting from ${from} (UTC).`,
          "",
          (to) => {
            if (!/^\d{4}-\d{2}-\d{2}$/.test(to)) { toast("Choose a valid end date.", "error"); return; }
            if (to < from) { toast("The end date must be on or after the start date.", "error"); return; }
            confirmAndDelete({ target: "segments", camera, from, to });
          },
          { inputType: "date", inputLabel: "End date" },
        );
      },
      { inputType: "date", inputLabel: "Start date" },
    );
  } else if (action === "clips") {
    confirmAndDelete({ target: "clips", camera });
  }
});

async function confirmAndDelete(body) {
  let preview;
  try {
    preview = await fetch("/api/storage/delete?dry_run=true", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    toast("Preview failed: network error.", "error");
    return;
  }
  if (!preview.ok) {
    toast("Preview failed: " + (await preview.text()), "error");
    return;
  }
  const p = await preview.json();
  if (!p.segments && !p.clips && !p.snapshots) {
    toast("Nothing matches; no files would be deleted.");
    return;
  }
  const summary =
    `This will delete ${p.segments} segments, ${p.clips} event clips and ` +
    `${p.snapshots} snapshots (${fmtBytes(p.bytes)}) across ` +
    `${(p.cameras || []).join(", ") || "(none)"}. This cannot be undone.`;

  showConfirmModal("Confirm delete", summary, async () => {
    let r;
    try {
      r = await fetch("/api/storage/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
    } catch {
      toast("Delete failed: network error.", "error");
      return;
    }
    if (r.status === 409) { toast("Storage busy, try again in a few seconds.", "error"); return; }
    if (r.status === 422) {
      const j = await r.json();
      toast("Cannot delete currently-open segment(s): " + (j.protected_paths || []).join(", "), "error");
      return;
    }
    if (!r.ok) { toast("Delete failed: " + (await r.text()), "error"); return; }
    const out = await r.json();
    toast(`Freed ${fmtBytes(out.bytes)} (${out.segments + out.clips} files).`);
    loadSummary();
    loadAudit();
  }, { confirmLabel: "Delete", destructive: true });
}

$("#run-cleanup-btn")?.addEventListener("click", () => {
  showConfirmModal(
    "Run cleanup now",
    "Run retention cleanup across all cameras now? Segments past their retention window will be removed.",
    async () => {
      const r = await fetch("/api/storage/cleanup", { method: "POST" });
      if (r.status === 409) { toast("Cleanup already running.", "error"); return; }
      if (!r.ok) { toast("Cleanup failed: " + (await r.text()), "error"); return; }
      toast("Cleanup started.");
      loadAudit();
    },
    { confirmLabel: "Run cleanup" },
  );
});

$("#recompress-run-btn")?.addEventListener("click", () => {
  showConfirmModal(
    "Run recompression now",
    "Re-encode every eligible aged segment and event clip to the configured lower resolution now? This runs outside the normal schedule window and may use significant CPU.",
    async () => {
      const btn = $("#recompress-run-btn");
      btn.dataset.busy = "1";
      btn.disabled = true;
      btn.textContent = "Running…";
      let r;
      try {
        r = await fetch("/api/system/recompress/trigger", { method: "POST" });
      } catch {
        delete btn.dataset.busy;
        btn.disabled = false;
        btn.textContent = "Run now";
        toast("Recompression failed: network error.", "error");
        return;
      }
      delete btn.dataset.busy;
      if (r.status === 409) { toast("Recompression already running.", "error"); return; }
      if (!r.ok) {
        btn.disabled = false;
        btn.textContent = "Run now";
        toast("Recompression failed: " + (await r.text()), "error");
        return;
      }
      toast("Recompression started.");
      loadSummary();
    },
    { confirmLabel: "Run now" },
  );
});

$("#free-space-btn")?.addEventListener("click", () => {
  const target = parseBytesInput($("#free-target").value || "");
  if (!target) { toast("Enter a target like '50 GB'.", "error"); return; }
  confirmAndDelete({ target: "free_bytes", free_bytes_target: target });
});

async function loadAudit() {
  const r = await fetch("/api/storage/audit?limit=20");
  if (!r.ok) return;
  const rows = await r.json();
  const list = $("#audit-list");
  if (!rows.length) {
    list.innerHTML = `<li class="storage-audit-empty">No storage activity recorded yet.</li>`;
    return;
  }
  list.innerHTML = rows.map((row) => {
    const ts = String(row.ts).replace("T", " ").slice(0, 19);
    return `<li class="storage-audit-row">
      <span class="storage-audit-time mono">${esc(ts)}</span>
      <span class="storage-audit-actor">${esc(row.actor)}</span>
      <span class="storage-audit-files">${row.files} files · ${fmtBytes(row.bytes)}</span>
      <code class="storage-audit-scope">${esc(JSON.stringify(row.scope))}</code>
    </li>`;
  }).join("");
}

loadSummary();
loadAudit();
setInterval(loadSummary, 30_000);
