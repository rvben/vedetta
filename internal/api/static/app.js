// Vedetta — Control Room Noir
// Vanilla JS for WebRTC, timeline, keyboard shortcuts, and UI interactions

'use strict';

// ─── State ───
let peerConnection = null;
let mseWebSocket = null;
let mseMediaSource = null;
let mseBlobURL = null;
let currentStream = null; // 'mse' | 'webrtc' | 'mjpeg' | null
let playbackMode = false; // true when playing back a recording
let playbackStartTime = null; // Date when playback segment starts
let playbackOffset = 0; // offset into segment where playback begins
let timelineDragging = false; // true during timeline drag
var cachedSegments = []; // raw segment data from API
var mergedBlocks = []; // merged blocks {start: sec, end: sec} for hit-testing
let timelineDate = new Date();
let calendarDate = new Date();

// ─── Helpers ───
function getCameraName() {
  return new URLSearchParams(location.search).get('name');
}

function el(id) {
  return document.getElementById(id);
}

function formatTimeAgo(dateStr) {
  const d = new Date(dateStr);
  const s = Math.floor((Date.now() - d) / 1000);
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
}

function toast(message, type) {
  const container = el('toasts');
  if (!container) return;
  const div = document.createElement('div');
  div.className = 'toast';
  if (type === 'error') div.style.borderColor = 'var(--red)';
  div.textContent = message;
  container.appendChild(div);
  setTimeout(() => div.remove(), 4000);
}

function readCookie(name) {
  var prefix = name + '=';
  var parts = document.cookie ? document.cookie.split(';') : [];
  for (var i = 0; i < parts.length; i++) {
    var cookie = parts[i].trim();
    if (cookie.indexOf(prefix) === 0) {
      return decodeURIComponent(cookie.slice(prefix.length));
    }
  }
  return '';
}

function isUnsafeMethod(method) {
  method = (method || 'GET').toUpperCase();
  return method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS';
}

var nativeFetch = window.fetch.bind(window);
window.fetch = function(input, init) {
  init = init || {};
  var method = init.method || 'GET';
  var headers = new Headers(init.headers || {});
  if (isUnsafeMethod(method)) {
    var csrf = readCookie('vedetta_csrf');
    if (csrf && !headers.has('X-CSRF-Token')) {
      headers.set('X-CSRF-Token', csrf);
    }
  }
  return nativeFetch(input, Object.assign({}, init, { headers: headers })).then(function(resp) {
    if (resp.status === 401 && location.pathname !== '/login.html') {
      location.href = '/login.html?next=' + encodeURIComponent(location.pathname + location.search + location.hash);
    }
    return resp;
  });
};

document.body.addEventListener('htmx:configRequest', function(evt) {
  var method = evt.detail.verb || evt.detail.method || 'GET';
  if (isUnsafeMethod(method)) {
    var csrf = readCookie('vedetta_csrf');
    if (csrf) {
      evt.detail.headers['X-CSRF-Token'] = csrf;
    }
  }
});

// ─── MSE over WebSocket ───
function startMSE() {
  var name = getCameraName();
  if (!name) return;
  stopStream();

  if (typeof MediaSource === 'undefined') {
    console.warn('MSE not supported, falling back to WebRTC');
    startWebRTC();
    return;
  }

  var video = el('live-video');
  if (!video) return;

  var protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  var wsURL = protocol + '//' + location.host + '/api/cameras/' + encodeURIComponent(name) + '/mse/ws';
  var ws = new WebSocket(wsURL);
  ws.binaryType = 'arraybuffer';
  mseWebSocket = ws;

  var mediaSource = null;
  var sourceBuffer = null;
  var queue = [];
  var initReceived = false;

  ws.onmessage = function(event) {
    // First message is text: codec MIME type string
    if (!initReceived && typeof event.data === 'string') {
      initReceived = true;
      var codecStr = event.data;
      console.log('MSE codec:', codecStr);

      // Check if the browser supports this codec
      if (!MediaSource.isTypeSupported(codecStr)) {
        console.warn('MSE codec not supported: ' + codecStr + ', falling back to WebRTC');
        cleanupMSE();
        startWebRTC();
        return;
      }

      mediaSource = new MediaSource();
      mseMediaSource = mediaSource;
      mseBlobURL = URL.createObjectURL(mediaSource);
      video.src = mseBlobURL;
      video.classList.remove('hidden');
      hide('live-snapshot');
      hide('live-mjpeg');

      mediaSource.addEventListener('sourceopen', function() {
        try {
          sourceBuffer = mediaSource.addSourceBuffer(codecStr);
          sourceBuffer.mode = 'segments';

          sourceBuffer.addEventListener('updateend', function() {
            // Drain one queued segment at a time (appendBuffer is async)
            if (queue.length > 0 && !sourceBuffer.updating) {
              sourceBuffer.appendBuffer(queue.shift());
              return;
            }
            // Trim buffer to keep ~10s to avoid unbounded growth
            if (!sourceBuffer.updating && sourceBuffer.buffered.length > 0) {
              var end = sourceBuffer.buffered.end(sourceBuffer.buffered.length - 1);
              var start = sourceBuffer.buffered.start(0);
              if (end - start > 15) {
                sourceBuffer.remove(start, end - 10);
              }
            }
          });

          // Flush first queued segment (only one — updateend handles the rest)
          if (queue.length > 0 && !sourceBuffer.updating) {
            sourceBuffer.appendBuffer(queue.shift());
          }
        } catch (err) {
          console.error('MSE sourceopen error:', err);
          cleanupMSE();
          startWebRTC();
        }
      });

      currentStream = 'mse';
      mseReconnectAttempts = 0;
      updateStreamButtons();
      updateMuteButton(codecStr.indexOf('mp4a') !== -1);
      startMSEStats();
      toast('MSE stream connected');
      return;
    }

    // Binary messages: fMP4 segments
    if (event.data instanceof ArrayBuffer) {
      var data = new Uint8Array(event.data);
      if (sourceBuffer && !sourceBuffer.updating) {
        try {
          sourceBuffer.appendBuffer(data);
        } catch (err) {
          // QuotaExceededError: trim buffer aggressively
          if (err.name === 'QuotaExceededError' && sourceBuffer.buffered.length > 0) {
            var bEnd = sourceBuffer.buffered.end(sourceBuffer.buffered.length - 1);
            sourceBuffer.remove(0, bEnd - 5);
          }
          queue.push(data);
        }
      } else {
        queue.push(data);
        // Prevent unbounded queue growth
        while (queue.length > 120) {
          queue.shift();
        }
      }

      // Auto-seek to live edge to minimize latency
      if (video.buffered.length > 0) {
        var liveEdge = video.buffered.end(video.buffered.length - 1);
        if (liveEdge - video.currentTime > 2) {
          video.currentTime = liveEdge - 0.5;
        }
      }
    }
  };

  ws.onerror = function() {
    console.error('MSE WebSocket error');
  };

  ws.onclose = function() {
    if (currentStream === 'mse') {
      toast('MSE stream disconnected', 'error');
      cleanupMSE();
      mseAutoReconnect();
    }
  };
}

function cleanupMSE() {
  if (mseWebSocket) {
    mseWebSocket.onclose = null;
    mseWebSocket.close();
    mseWebSocket = null;
  }
  if (mseMediaSource && mseMediaSource.readyState === 'open') {
    try { mseMediaSource.endOfStream(); } catch (e) { /* ignore */ }
  }
  mseMediaSource = null;
  if (mseBlobURL) {
    URL.revokeObjectURL(mseBlobURL);
    mseBlobURL = null;
  }
}

// ─── MSE Auto-Reconnect ───
var mseReconnectAttempts = 0;
var mseMaxReconnect = 5;
var mseReconnectTimer = null;

function mseAutoReconnect() {
  if (mseReconnectAttempts >= mseMaxReconnect) {
    toast('MSE reconnect failed, trying WebRTC...', 'error');
    mseReconnectAttempts = 0;
    startWebRTC();
    return;
  }

  mseReconnectAttempts++;
  var delay = Math.min(1000 * Math.pow(2, mseReconnectAttempts - 1), 8000);
  toast('Reconnecting MSE (' + mseReconnectAttempts + '/' + mseMaxReconnect + ')...');

  mseReconnectTimer = setTimeout(function() {
    startMSE();
  }, delay);
}

function startMSEStats() {
  stopStreamStats();
  streamStatsInterval = setInterval(function() {
    var statsEl = el('stream-stats');
    var video = el('live-video');
    if (!statsEl || !video || currentStream !== 'mse') return;

    var w = video.videoWidth;
    var h = video.videoHeight;
    if (w && h) {
      updateStreamBadge(w + '×' + h);
    }

    var parts = [];
    if (w && h) parts.push('<span>' + w + '×' + h + '</span>');

    // Buffer health
    if (video.buffered.length > 0) {
      var buffered = video.buffered.end(video.buffered.length - 1) - video.currentTime;
      parts.push('<span>' + buffered.toFixed(1) + 's buf</span>');
    }

    statsEl.innerHTML = parts.join('');
  }, 1000);
}

// ─── WebRTC ───
async function startWebRTC() {
  const name = getCameraName();
  if (!name) return;
  stopStream();

  try {
    peerConnection = new RTCPeerConnection({
      iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
    });

    peerConnection.addTransceiver('video', { direction: 'recvonly' });
    peerConnection.addTransceiver('audio', { direction: 'recvonly' });

    peerConnection.ontrack = function(event) {
      const video = el('live-video');
      if (!video) return;
      video.srcObject = event.streams[0];
      video.classList.remove('hidden');
      hide('live-snapshot');
      hide('live-mjpeg');
    };

    peerConnection.oniceconnectionstatechange = function() {
      const state = peerConnection.iceConnectionState;
      if (state === 'failed' || state === 'disconnected') {
        toast('WebRTC connection lost', 'error');
        stopStream();
        webrtcAutoReconnect();
      } else if (state === 'connected') {
        webrtcReconnectAttempts = 0;
      }
    };

    const offer = await peerConnection.createOffer();
    await peerConnection.setLocalDescription(offer);

    const resp = await fetch('/api/cameras/' + encodeURIComponent(name) + '/webrtc/offer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(peerConnection.localDescription)
    });

    if (!resp.ok) throw new Error('Server returned ' + resp.status);

    const answer = await resp.json();
    await peerConnection.setRemoteDescription(new RTCSessionDescription(answer));

    currentStream = 'webrtc';
    updateStreamButtons();
    startStreamStats();

    // Check if audio was negotiated (m=audio with port > 0 in SDP answer)
    var hasAudio = false;
    if (peerConnection.remoteDescription && peerConnection.remoteDescription.sdp) {
      var sdpLines = peerConnection.remoteDescription.sdp.split('\r\n');
      hasAudio = sdpLines.some(function(l) {
        return l.startsWith('m=audio ') && !l.startsWith('m=audio 0');
      });
    }
    updateMuteButton(hasAudio);
    toast('WebRTC connected');
  } catch (err) {
    console.error('WebRTC error:', err);
    toast('WebRTC failed: ' + err.message, 'error');
    stopStream();
  }
}

function startMJPEG() {
  const name = getCameraName();
  if (!name) return;
  stopStream();

  const mjpeg = el('live-mjpeg');
  if (!mjpeg) return;
  mjpeg.src = '/api/cameras/' + encodeURIComponent(name) + '/mjpeg';
  mjpeg.classList.remove('hidden');
  hide('live-snapshot');
  hide('live-video');

  currentStream = 'mjpeg';
  updateStreamButtons();
  toast('MJPEG stream started');

  // Start MJPEG stream stats to show resolution on hover
  stopStreamStats();
  streamStatsInterval = setInterval(function() {
    var statsEl = el('stream-stats');
    var mjpegImg = el('live-mjpeg');
    if (!statsEl || !mjpegImg) return;
    if (mjpegImg.naturalWidth && mjpegImg.naturalHeight) {
      statsEl.innerHTML = '<span>MJPEG</span><span>' + mjpegImg.naturalWidth + '×' + mjpegImg.naturalHeight + '</span>';
    }
  }, 2000);
}

function togglePause() {
  var video = el('live-video');
  if (!video || video.classList.contains('hidden')) return;

  if (video.paused) {
    video.play();
  } else {
    video.pause();
  }
  updatePauseButton();
}

function updatePauseButton() {
  var video = el('live-video');
  var paused = !video || video.classList.contains('hidden') || video.paused;
  var pauseIcon = el('pause-icon');
  var playIcon = el('play-icon');
  if (pauseIcon) pauseIcon.style.display = paused ? 'none' : '';
  if (playIcon) playIcon.style.display = paused ? '' : 'none';
}

function stopStream() {
  if (mseReconnectTimer) {
    clearTimeout(mseReconnectTimer);
    mseReconnectTimer = null;
  }
  if (webrtcReconnectTimer) {
    clearTimeout(webrtcReconnectTimer);
    webrtcReconnectTimer = null;
  }

  cleanupMSE();

  if (peerConnection) {
    peerConnection.close();
    peerConnection = null;
  }

  const video = el('live-video');
  if (video) { video.srcObject = null; video.src = ''; video.classList.add('hidden'); }

  const mjpeg = el('live-mjpeg');
  if (mjpeg) { mjpeg.src = ''; mjpeg.classList.add('hidden'); }

  const snap = el('live-snapshot');
  if (snap) snap.classList.remove('hidden');

  currentStream = null;
  stopStreamStats();
  updateStreamButtons();
  updateMuteButton(false);
}

function updateStreamButtons() {
  const btnWebrtc = el('btn-webrtc');
  const btnMjpeg = el('btn-mjpeg');
  const btnStop = el('btn-stop');

  if (btnWebrtc) {
    btnWebrtc.classList.toggle('active', currentStream === 'webrtc' || currentStream === 'mse');
    btnWebrtc.disabled = currentStream !== null;
  }
  if (btnMjpeg) {
    btnMjpeg.classList.toggle('active', currentStream === 'mjpeg');
    btnMjpeg.disabled = currentStream !== null;
  }
  if (btnStop) {
    btnStop.classList.toggle('hidden', currentStream === null);
  }
  updatePauseButton();

  updateStreamBadge();
}

function updateStreamBadge(resolution) {
  var badge = el('stream-badge');
  if (!badge) return;

  var type = playbackMode ? null : currentStream;

  if (!type) {
    // Show snapshot badge when no stream is active (and not in playback)
    if (!playbackMode) {
      badge.setAttribute('data-type', 'snapshot');
      badge.innerHTML = '<span class="badge-dot"></span>SNAPSHOT';
      badge.classList.remove('hidden');
    } else {
      badge.classList.add('hidden');
    }
    return;
  }

  badge.setAttribute('data-type', type === 'mse' ? 'webrtc' : type);
  badge.classList.remove('hidden');

  var label = type === 'mse' ? 'MSE' : type.toUpperCase();
  var resStr = '';

  if ((type === 'webrtc' || type === 'mse') && resolution) {
    resStr = '<span class="badge-res">' + resolution + '</span>';
  }

  badge.innerHTML = '<span class="badge-dot"></span>' + label + (resStr ? ' · ' + resStr : '');
}

// Stream stats overlay — shows codec, resolution, framerate, bitrate on hover
var streamStatsInterval = null;

function startStreamStats() {
  stopStreamStats();
  streamStatsInterval = setInterval(updateStreamStats, 1000);
}

function stopStreamStats() {
  if (streamStatsInterval) {
    clearInterval(streamStatsInterval);
    streamStatsInterval = null;
  }
  var statsEl = el('stream-stats');
  if (statsEl) statsEl.innerHTML = '';
}

function updateStreamStats() {
  var statsEl = el('stream-stats');
  var video = el('live-video');
  if (!statsEl || !video) return;
  if (currentStream === 'mse') return; // MSE has its own stats via startMSEStats
  if (currentStream !== 'webrtc' || !peerConnection) return;

  // Get resolution from the video element
  var w = video.videoWidth;
  var h = video.videoHeight;
  if (w && h) {
    updateStreamBadge(w + '×' + h);
  }

  peerConnection.getStats().then(function(stats) {
    var parts = [];

    stats.forEach(function(report) {
      if (report.type === 'inbound-rtp' && report.kind === 'audio') {
        if (report.codecId) {
          stats.forEach(function(codec) {
            if (codec.id === report.codecId && codec.mimeType) {
              parts.push('<span>' + codec.mimeType.replace('audio/', '') + '</span>');
            }
          });
        }
      }
      if (report.type === 'inbound-rtp' && report.kind === 'video') {
        // Codec
        if (report.codecId) {
          stats.forEach(function(codec) {
            if (codec.id === report.codecId && codec.mimeType) {
              parts.push('<span>' + codec.mimeType.replace('video/', '') + '</span>');
            }
          });
        }

        // Resolution
        if (w && h) {
          parts.push('<span>' + w + '×' + h + '</span>');
        }

        // FPS
        if (report.framesPerSecond) {
          parts.push('<span>' + Math.round(report.framesPerSecond) + ' fps</span>');
        }

        // Bitrate (calculate from bytesReceived delta)
        if (typeof report.bytesReceived === 'number') {
          var key = 'lastBytes_' + report.id;
          var timeKey = 'lastTime_' + report.id;
          var prev = statsEl[key];
          var prevTime = statsEl[timeKey];
          var now = report.timestamp;

          if (prev !== undefined && prevTime !== undefined) {
            var deltaBits = (report.bytesReceived - prev) * 8;
            var deltaSec = (now - prevTime) / 1000;
            if (deltaSec > 0) {
              var bps = deltaBits / deltaSec;
              var label = bps >= 1000000
                ? (bps / 1000000).toFixed(1) + ' Mbps'
                : Math.round(bps / 1000) + ' kbps';
              parts.push('<span>' + label + '</span>');
            }
          }

          statsEl[key] = report.bytesReceived;
          statsEl[timeKey] = now;
        }

        // Packet loss
        if (report.packetsLost > 0 && report.packetsReceived > 0) {
          var lossRate = (report.packetsLost / (report.packetsReceived + report.packetsLost) * 100);
          if (lossRate > 0.1) {
            parts.push('<span style="color:var(--red)">' + lossRate.toFixed(1) + '% loss</span>');
          }
        }
      }
    });

    statsEl.innerHTML = parts.join('');
  });
}

function hide(id) {
  const e = el(id);
  if (e) e.classList.add('hidden');
}

// ─── Audio Mute/Unmute ───
var audioAvailable = false;

function toggleMute() {
  var video = el('live-video');
  if (!video || !audioAvailable) return;

  video.muted = !video.muted;
  updateMuteIcon();

  if (!video.muted) {
    toast('Audio enabled');
  }
}

function updateMuteButton(hasAudio) {
  audioAvailable = hasAudio;
  var btn = el('btn-mute');
  if (!btn) return;

  if (!hasAudio) {
    btn.disabled = true;
    btn.title = 'No audio available (camera uses unsupported codec)';
    btn.style.opacity = '0.3';
  } else {
    btn.disabled = false;
    btn.title = 'Toggle audio (A)';
    btn.style.opacity = '';
  }
  updateMuteIcon();
}

function updateMuteIcon() {
  var video = el('live-video');
  var iconOff = el('mute-icon-off');
  var iconOn = el('mute-icon-on');
  if (!iconOff || !iconOn) return;

  var muted = !video || video.muted || !audioAvailable;
  iconOff.style.display = muted ? '' : 'none';
  iconOn.style.display = muted ? 'none' : '';
}

// ─── Picture-in-Picture ───
async function togglePiP() {
  const video = el('live-video');
  if (!video || !document.pictureInPictureEnabled) {
    toast('Picture-in-Picture not available', 'error');
    return;
  }

  try {
    if (document.pictureInPictureElement) {
      await document.exitPictureInPicture();
    } else {
      await video.requestPictureInPicture();
    }
  } catch (err) {
    toast('PiP failed: ' + err.message, 'error');
  }
}

// ─── Fullscreen ───
function toggleFullscreen() {
  var container = el('live-viewport')?.closest('.live-container');
  if (!container) return;

  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else {
    (container.requestFullscreen || container.webkitRequestFullscreen).call(container).catch(function() {});
  }
}

document.addEventListener('fullscreenchange', updateFullscreenIcon);
document.addEventListener('webkitfullscreenchange', updateFullscreenIcon);

function updateFullscreenIcon() {
  var enter = el('fs-icon-enter');
  var exit = el('fs-icon-exit');
  if (!enter || !exit) return;
  var isFs = !!document.fullscreenElement;
  enter.style.display = isFs ? 'none' : '';
  exit.style.display = isFs ? '' : 'none';
}

// ─── Timeline ───
function initTimeline() {
  timelineDate = new Date();
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData();
  startPlayheadAnimation();

  const track = el('timeline-track');
  if (!track) return;

  // Add hover cursor element
  var cursor = document.createElement('div');
  cursor.className = 'timeline-cursor';
  var cursorTime = document.createElement('span');
  cursorTime.className = 'timeline-cursor-time';
  cursor.appendChild(cursorTime);
  track.appendChild(cursor);

  // Add thumbnail preview element (outside track to avoid overflow:hidden clipping)
  var preview = document.createElement('div');
  preview.className = 'timeline-preview';
  preview.style.display = 'none';
  var previewImg = document.createElement('img');
  previewImg.className = 'timeline-preview-img';
  preview.appendChild(previewImg);
  var timelineContainer = track.parentElement;
  timelineContainer.style.position = 'relative';
  timelineContainer.appendChild(preview);

  var thumbTimer = null;
  var lastThumbUrl = '';
  var lastThumbTime = 0;

  function requestThumbnail(pct) {
    if (!isOverSegment(pct)) {
      preview.style.display = 'none';
      lastThumbUrl = '';
      return;
    }

    var totalSec = pct * 86400;
    var h = Math.floor(totalSec / 3600);
    var m = Math.floor((totalSec % 3600) / 60);
    var s = Math.floor(totalSec % 60);
    var name = getCameraName();
    if (!name) return;

    var ts = timelineDate.getFullYear() + '-' +
      String(timelineDate.getMonth() + 1).padStart(2, '0') + '-' +
      String(timelineDate.getDate()).padStart(2, '0') + 'T' +
      String(h).padStart(2, '0') + ':' +
      String(m).padStart(2, '0') + ':' +
      String(s).padStart(2, '0') + 'Z';

    var url = '/api/cameras/' + encodeURIComponent(name) + '/thumbnail?t=' + encodeURIComponent(ts);
    if (url === lastThumbUrl) return;
    lastThumbUrl = url;

    previewImg.onload = function() {
      preview.style.display = '';
    };
    previewImg.onerror = function() {
      preview.style.display = 'none';
    };
    previewImg.src = url;
  }

  track.addEventListener('mousedown', function(e) {
    timelineDragging = true;
    scrubTimeline(e, false);
  });

  track.addEventListener('mousemove', function(e) {
    if (timelineDragging) scrubTimeline(e, false);
    // Update hover cursor position and time
    var rect = track.getBoundingClientRect();
    var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    cursor.style.left = (pct * 100) + '%';
    // Position preview relative to the container, just above the track
    var previewX = track.offsetLeft + pct * rect.width;
    preview.style.left = previewX + 'px';
    // Use bottom positioning: container height minus track's top offset, plus gap
    var containerH = timelineContainer.offsetHeight;
    preview.style.bottom = (containerH - track.offsetTop + 4) + 'px';
    preview.style.top = '';
    var totalSec = pct * 86400;
    var h = Math.floor(totalSec / 3600);
    var m = Math.floor((totalSec % 3600) / 60);
    cursorTime.textContent = String(h).padStart(2, '0') + ':' + String(m).padStart(2, '0');
    // Change cursor when over a recorded segment
    track.style.cursor = isOverSegment(pct) ? 'pointer' : 'default';

    // Thumbnail request: throttle to one request per 150ms (fires immediately, then rate-limits)
    var now = Date.now();
    if (now - lastThumbTime >= 150) {
      lastThumbTime = now;
      requestThumbnail(pct);
    } else {
      clearTimeout(thumbTimer);
      thumbTimer = setTimeout(function() { lastThumbTime = Date.now(); requestThumbnail(pct); }, 150 - (now - lastThumbTime));
    }
  });

  track.addEventListener('mouseleave', function() {
    cursor.style.display = 'none';
    preview.style.display = 'none';
    lastThumbUrl = '';
    clearTimeout(thumbTimer);
    track.style.cursor = '';
  });

  track.addEventListener('mouseenter', function() {
    cursor.style.display = '';
  });

  document.addEventListener('mouseup', function() {
    if (timelineDragging) {
      timelineDragging = false;
      scrubTimeline(lastScrubEvent, true);
      preview.style.display = 'none';
    }
  });

  // Touch support
  track.addEventListener('touchstart', function(e) {
    timelineDragging = true;
    scrubTimeline(e.touches[0], false);
    e.preventDefault();
  }, { passive: false });

  track.addEventListener('touchmove', function(e) {
    if (timelineDragging) scrubTimeline(e.touches[0], false);
    e.preventDefault();
  }, { passive: false });

  track.addEventListener('touchend', function(e) {
    if (timelineDragging) {
      timelineDragging = false;
      scrubTimeline(lastScrubEvent, true);
    }
  });
}

var lastScrubEvent = null;

// Convert a 0-1 fraction to seconds-of-day
function pctToSec(pct) {
  return pct * 86400;
}

// Check if a 0-1 fraction falls within any merged block
function isOverSegment(pct) {
  var sec = pctToSec(pct);
  return mergedBlocks.some(function(block) {
    return sec >= block.start && sec <= block.end;
  });
}

// Find nearest merged block edge to a given seconds-of-day value
function snapToNearestSegment(sec) {
  var best = null;
  var bestDist = Infinity;
  mergedBlocks.forEach(function(block) {
    if (Math.abs(sec - block.start) < bestDist) {
      bestDist = Math.abs(sec - block.start);
      best = block.start;
    }
    if (Math.abs(sec - block.end) < bestDist) {
      bestDist = Math.abs(sec - block.end);
      best = block.end;
    }
  });
  return best;
}

function scrubTimeline(e, commit) {
  if (!e) return;
  lastScrubEvent = e;

  const track = el('timeline-track');
  const playhead = el('timeline-playhead');
  if (!track || !playhead) return;

  const rect = track.getBoundingClientRect();
  let pct = (e.clientX - rect.left) / rect.width;
  pct = Math.max(0, Math.min(1, pct));

  playhead.style.left = (pct * 100) + '%';
  playhead.style.display = '';

  // Only start playback on commit (mouseup/touchend)
  if (!commit) return;

  var sec = pctToSec(pct);

  // Check if directly on a segment
  if (isOverSegment(pct)) {
    var hours = Math.floor(sec / 3600);
    var mins = Math.floor((sec % 3600) / 60);
    var secs = Math.floor(sec % 60);
    var d = new Date(timelineDate);
    d.setHours(hours, mins, secs, 0);
    startPlayback(d);
    return;
  }

  // Not on a segment — snap to nearest edge if close (within 5 min)
  var nearest = snapToNearestSegment(sec);
  if (nearest !== null && Math.abs(sec - nearest) < 300) {
    var hours = Math.floor(nearest / 3600);
    var mins = Math.floor((nearest % 3600) / 60);
    var secs = Math.floor(nearest % 60);
    var d = new Date(timelineDate);
    d.setHours(hours, mins, secs, 0);
    startPlayback(d);
    return;
  }

  // Too far from any segment — return to live
  updatePlayheadToNow();
}

function timelineNav(delta) {
  timelineDate.setDate(timelineDate.getDate() + delta);
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData();
}

function timelineToday() {
  timelineDate = new Date();
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData();
}

function updateTimelineDate() {
  const label = el('timeline-date');
  if (!label) return;

  const today = new Date();
  const isToday = timelineDate.toDateString() === today.toDateString();

  if (isToday) {
    label.textContent = 'Today';
  } else {
    label.textContent = timelineDate.toLocaleDateString('en-US', {
      weekday: 'short', month: 'short', day: 'numeric'
    });
  }

  // Update hidden date input
  var dateInput = el('timeline-date-input');
  if (dateInput) {
    dateInput.value = timelineDate.getFullYear() + '-' +
      String(timelineDate.getMonth() + 1).padStart(2, '0') + '-' +
      String(timelineDate.getDate()).padStart(2, '0');
  }
}

function updatePlayheadToNow() {
  const playhead = el('timeline-playhead');
  if (!playhead) return;

  const now = new Date();
  const today = new Date();
  const isToday = timelineDate.toDateString() === today.toDateString();

  if (isToday) {
    var pct = (now.getHours() * 60 + now.getMinutes()) / (24 * 60) * 100;
    playhead.style.left = pct + '%';
    playhead.style.display = '';
  } else {
    playhead.style.left = '0%';
    playhead.style.display = 'none';
  }
}

function fetchTimelineData() {
  var name = getCameraName();
  if (!name) return;

  var dateStr = timelineDate.getFullYear() + '-' +
    String(timelineDate.getMonth() + 1).padStart(2, '0') + '-' +
    String(timelineDate.getDate()).padStart(2, '0');

  fetch('/api/cameras/' + encodeURIComponent(name) + '/timeline?date=' + dateStr)
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(data) {
      cachedSegments = data.segments || [];
      renderTimelineSegments(cachedSegments);
      renderTimelineEvents(data.events || []);
    })
    .catch(function(err) {
      console.error('Timeline fetch error:', err);
    });
}

function renderTimelineSegments(segments) {
  var track = el('timeline-track');
  if (!track) return;

  // Remove existing segments
  track.querySelectorAll('.timeline-segment').forEach(function(s) { s.remove(); });

  // Merge adjacent segments (gap < 60s) into continuous blocks
  var blocks = [];
  segments.forEach(function(seg) {
    var start = new Date(seg.start_time);
    var end = new Date(seg.end_time);
    var startSec = start.getHours() * 3600 + start.getMinutes() * 60 + start.getSeconds();
    var endSec = end.getHours() * 3600 + end.getMinutes() * 60 + end.getSeconds();
    if (endSec <= startSec) return;

    if (blocks.length > 0 && startSec - blocks[blocks.length - 1].end <= 60) {
      // Extend previous block
      if (endSec > blocks[blocks.length - 1].end) {
        blocks[blocks.length - 1].end = endSec;
      }
    } else {
      blocks.push({ start: startSec, end: endSec });
    }
  });

  // Store merged blocks for hit-testing
  mergedBlocks = blocks;

  blocks.forEach(function(block) {
    var startPct = block.start / 86400 * 100;
    var widthPct = (block.end - block.start) / 86400 * 100;
    if (widthPct < 0.15) widthPct = 0.15;

    var div = document.createElement('div');
    div.className = 'timeline-segment';
    div.style.left = startPct + '%';
    div.style.width = widthPct + '%';
    track.insertBefore(div, track.querySelector('.timeline-playhead'));
  });
}

function renderTimelineEvents(events) {
  var track = el('timeline-track');
  if (!track) return;

  // Remove existing event markers and tooltips
  track.querySelectorAll('.timeline-event').forEach(function(e) { e.remove(); });
  document.querySelectorAll('.timeline-tooltip').forEach(function(t) { t.remove(); });

  events.forEach(function(evt) {
    var ts = new Date(evt.timestamp);
    var pct = (ts.getHours() * 3600 + ts.getMinutes() * 60 + ts.getSeconds()) / 86400 * 100;
    var timeStr = String(ts.getHours()).padStart(2, '0') + ':' +
      String(ts.getMinutes()).padStart(2, '0') + ':' +
      String(ts.getSeconds()).padStart(2, '0');
    var scoreStr = Math.round(evt.score * 100) + '%';

    var dot = document.createElement('div');
    dot.className = 'timeline-event ' + evt.label;
    dot.style.left = pct + '%';
    dot.title = evt.label + ' at ' + timeStr + ' (' + scoreStr + ')';

    dot.addEventListener('mouseenter', function(e) {
      showTimelineTooltip(dot, evt.label, timeStr, scoreStr);
    });

    dot.addEventListener('mouseleave', function() {
      hideTimelineTooltip();
    });

    dot.addEventListener('click', function(e) {
      e.stopPropagation();
      location.href = '/event.html?id=' + encodeURIComponent(evt.id);
    });

    track.insertBefore(dot, track.querySelector('.timeline-playhead'));
  });
}

function showTimelineTooltip(anchor, label, time, score) {
  hideTimelineTooltip();

  var tooltip = document.createElement('div');
  tooltip.className = 'timeline-tooltip';
  tooltip.innerHTML = '<strong>' + label + '</strong><br>' + time + '<br>' + score;

  var container = el('timeline-container');
  if (!container) return;
  container.appendChild(tooltip);

  var anchorRect = anchor.getBoundingClientRect();
  var containerRect = container.getBoundingClientRect();
  var tooltipWidth = tooltip.offsetWidth;

  var left = anchorRect.left + anchorRect.width / 2 - containerRect.left - tooltipWidth / 2;
  left = Math.max(4, Math.min(left, containerRect.width - tooltipWidth - 4));

  tooltip.style.left = left + 'px';
}

function hideTimelineTooltip() {
  document.querySelectorAll('.timeline-tooltip').forEach(function(t) { t.remove(); });
}

// ─── Birdseye View ───
let birdseyeInterval = null;

function setView(mode) {
  var btnGrid = el('btn-grid-view');
  var btnBirdseye = el('btn-birdseye-view');
  var cameraGrid = el('camera-grid');
  var birdseyeGrid = el('birdseye-grid');

  if (!cameraGrid || !birdseyeGrid) return;

  if (mode === 'birdseye') {
    cameraGrid.style.display = 'none';
    birdseyeGrid.style.display = '';
    if (btnGrid) btnGrid.classList.remove('active');
    if (btnBirdseye) btnBirdseye.classList.add('active');
    startBirdseye();
  } else {
    cameraGrid.style.display = '';
    birdseyeGrid.style.display = 'none';
    if (btnGrid) btnGrid.classList.add('active');
    if (btnBirdseye) btnBirdseye.classList.remove('active');
    stopBirdseye();
  }

  localStorage.setItem('vedetta-view', mode);
}

function startBirdseye() {
  stopBirdseye();
  refreshBirdseye();
  birdseyeInterval = setInterval(refreshBirdseye, 2000);
}

function stopBirdseye() {
  if (birdseyeInterval) {
    clearInterval(birdseyeInterval);
    birdseyeInterval = null;
  }
}

function refreshBirdseye() {
  fetch('/api/cameras')
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(cameras) {
      var grid = el('birdseye-grid');
      if (!grid) return;

      var cameraList = cameras || [];
      if (cameraList.length === 0) {
        grid.innerHTML = '<div class="empty-state"><p>No cameras configured</p></div>';
        return;
      }

      // Build or update cells
      cameraList.forEach(function(cam) {
        var cellId = 'birdseye-' + cam.name;
        var cell = document.getElementById(cellId);
        if (!cell) {
          cell = document.createElement('div');
          cell.id = cellId;
          cell.className = 'birdseye-cell';
          cell.setAttribute('role', 'listitem');
          cell.onclick = function() {
            location.href = '/camera.html?name=' + encodeURIComponent(cam.name);
          };

          var img = document.createElement('img');
          img.alt = cam.name + ' camera feed';
          cell.appendChild(img);

          var label = document.createElement('div');
          label.className = 'birdseye-label';
          label.textContent = cam.name;
          cell.appendChild(label);

          grid.appendChild(cell);
        }

        // Update snapshot with cache-busting timestamp
        var img = cell.querySelector('img');
        if (img) {
          var newSrc = '/api/cameras/' + encodeURIComponent(cam.name) + '/snapshot?t=' + Date.now();
          img.src = newSrc;
        }
      });

      // Remove cells for cameras that no longer exist
      var validIds = new Set(cameraList.map(function(c) { return 'birdseye-' + c.name; }));
      Array.from(grid.querySelectorAll('.birdseye-cell')).forEach(function(cell) {
        if (!validIds.has(cell.id)) {
          cell.remove();
        }
      });
    })
    .catch(function(err) {
      console.error('Birdseye refresh error:', err);
    });
}

// Restore saved view preference on page load
(function() {
  var saved = localStorage.getItem('vedetta-view');
  if (saved === 'birdseye') {
    // Defer to after DOM is ready
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', function() { setView('birdseye'); });
    } else {
      setView('birdseye');
    }
  }
})();

// ─── Playback ───
function startPlayback(timestamp) {
  var name = getCameraName();
  if (!name) return;

  var isoStr = timestamp.toISOString();
  var url = '/api/cameras/' + encodeURIComponent(name) + '/playback?start=' + encodeURIComponent(isoStr);

  // Stop any live stream first
  if (currentStream) {
    stopStream();
  }

  var video = el('live-video');
  if (!video) return;

  // Check if segment exists before loading video
  fetch(url, { method: 'HEAD' })
    .then(function(resp) {
      if (!resp.ok) {
        if (resp.status === 404) {
          toast('No recording found for this timestamp', 'error');
        } else {
          toast('Playback error: ' + resp.status, 'error');
        }
        updatePlayheadToNow();
        return;
      }

      // Server trims the fMP4 to start at the requested time,
      // so playback starts at the clicked position directly.
      playbackOffset = 0;
      playbackStartTime = timestamp;

      video.srcObject = null;
      video.src = url;
      video.muted = false;
      video.autoplay = true;
      video.classList.remove('hidden');
      hide('live-snapshot');
      hide('live-mjpeg');

      video.onloadedmetadata = function() {
        video.play().catch(function() {});
      };

      video.ontimeupdate = function() {
        updatePlayheadForPlayback(video.currentTime);
      };

      video.onended = function() {
        playNextSegment();
      };

      playbackMode = true;
      updatePlaybackUI();
      toast('Playing recording from ' + timestamp.toLocaleTimeString());
    })
    .catch(function(err) {
      toast('Playback failed: ' + err.message, 'error');
    });
}

function playNextSegment() {
  if (!playbackStartTime || !playbackMode) {
    returnToLive();
    return;
  }

  var video = el('live-video');
  if (!video) { returnToLive(); return; }

  // Calculate where the current segment ended in wall-clock time
  var elapsed = video.duration || 0;
  var nextTime = new Date(playbackStartTime.getTime() + elapsed * 1000);

  // Don't play past current time
  if (nextTime >= new Date()) {
    returnToLive();
    return;
  }

  var name = getCameraName();
  var nextISO = nextTime.toISOString();
  var url = '/api/cameras/' + encodeURIComponent(name) + '/playback?start=' + encodeURIComponent(nextISO);

  fetch(url, { method: 'HEAD' })
    .then(function(resp) {
      if (!resp.ok) {
        returnToLive();
        return;
      }

      playbackStartTime = nextTime;
      video.src = url;
      video.play().catch(function() {});
    })
    .catch(function() {
      returnToLive();
    });
}

function returnToLive() {
  var video = el('live-video');
  if (video) {
    video.pause();
    video.src = '';
    video.srcObject = null;
    video.onloadedmetadata = null;
    video.ontimeupdate = null;
    video.onended = null;
    video.classList.add('hidden');
  }

  var snap = el('live-snapshot');
  if (snap) snap.classList.remove('hidden');

  playbackMode = false;
  currentStream = null;
  updatePlaybackUI();
  updateStreamButtons();

  // Reset playhead to current time if viewing today
  updatePlayheadToNow();

  toast('Returned to live view');
}

function updatePlaybackUI() {
  var toolbar = el('live-toolbar');
  var pbBadge = el('playback-badge');
  var btnLive = el('btn-live');

  // Show toolbar only during playback (Return to Live + badge)
  if (toolbar) toolbar.classList.toggle('hidden', !playbackMode);
  if (pbBadge) pbBadge.classList.toggle('hidden', !playbackMode);
  if (btnLive) btnLive.classList.toggle('hidden', !playbackMode);
}

function updatePlayheadForPlayback(currentTime) {
  if (!playbackStartTime || timelineDragging) return;
  var playhead = el('timeline-playhead');
  if (!playhead) return;

  // Calculate the wall-clock time being played
  var wallTime = new Date(playbackStartTime.getTime() + currentTime * 1000);
  var pct = (wallTime.getHours() * 3600 + wallTime.getMinutes() * 60 + wallTime.getSeconds()) / 86400 * 100;
  playhead.style.left = pct + '%';
  playhead.style.display = '';
}

// ─── Filter Chips ───
function toggleChip(chipEl, filterType) {
  // Deactivate siblings of same filter type
  const siblings = chipEl.parentElement.querySelectorAll('.chip[data-filter="' + filterType + '"]');
  siblings.forEach(s => s.classList.remove('active'));
  chipEl.classList.add('active');

  // Reload events with current filters
  reloadEvents();
}

function reloadEvents() {
  const gallery = el('events-gallery');
  if (!gallery) return;

  const labelChip = document.querySelector('.chip[data-filter="label"].active');
  const cameraChip = document.querySelector('.chip[data-filter="camera"].active');
  const objectChip = document.querySelector('.chip[data-filter="object"].active');

  let url = '/partials/events-gallery?limit=50';
  if (labelChip && labelChip.dataset.value) {
    url += '&label=' + encodeURIComponent(labelChip.dataset.value);
  }
  if (cameraChip && cameraChip.dataset.value) {
    url += '&camera=' + encodeURIComponent(cameraChip.dataset.value);
  }
  if (objectChip && objectChip.dataset.value) {
    url += '&object=' + encodeURIComponent(objectChip.dataset.value);
  }

  gallery.setAttribute('hx-get', url);
  htmx.trigger(gallery, 'htmx:abort');
  htmx.ajax('GET', url, { target: '#events-gallery', swap: 'innerHTML' });
}

// ─── Calendar & Recordings page ───
function initCalendar() {
  calendarDate = new Date();
  renderCalendar();
}

function calendarNav(delta) {
  calendarDate.setMonth(calendarDate.getMonth() + delta);
  renderCalendar();
}

function renderCalendar() {
  var grid = el('calendar-grid');
  var monthLabel = el('calendar-month');
  if (!grid || !monthLabel) return;

  var year = calendarDate.getFullYear();
  var month = calendarDate.getMonth();

  monthLabel.textContent = calendarDate.toLocaleDateString('en-US', { month: 'long', year: 'numeric' });

  var labels = grid.querySelectorAll('.calendar-day-label');
  grid.innerHTML = '';
  labels.forEach(function(l) { grid.appendChild(l); });

  var firstDay = new Date(year, month, 1).getDay();
  var daysInMonth = new Date(year, month + 1, 0).getDate();
  var today = new Date();

  for (var i = 0; i < firstDay; i++) {
    var empty = document.createElement('span');
    empty.className = 'calendar-day empty';
    grid.appendChild(empty);
  }

  for (var d = 1; d <= daysInMonth; d++) {
    var cell = document.createElement('button');
    cell.className = 'calendar-day';
    cell.textContent = d;
    cell.dataset.day = d;

    var cellDate = new Date(year, month, d);
    if (cellDate.toDateString() === today.toDateString()) {
      cell.classList.add('today');
    }

    cell.onclick = (function(cd, c) {
      return function() {
        grid.querySelectorAll('.calendar-day.selected').forEach(function(s) { s.classList.remove('selected'); });
        c.classList.add('selected');
        loadRecordingsSummary(cd);
      };
    })(cellDate, cell);

    grid.appendChild(cell);
  }

  var monthStr = year + '-' + String(month + 1).padStart(2, '0');
  var url = '/api/recordings/calendar?month=' + monthStr;

  fetch(url)
    .then(function(resp) { return resp.json(); })
    .then(function(data) {
      var daysWithData = new Set(data.days || []);
      grid.querySelectorAll('.calendar-day[data-day]').forEach(function(cell) {
        if (daysWithData.has(parseInt(cell.dataset.day, 10))) {
          cell.classList.add('has-data');
        }
      });

      var todayDay = (today.getFullYear() === year && today.getMonth() === month) ? today.getDate() : null;
      var selectDay = null;
      if (todayDay && daysWithData.has(todayDay)) {
        selectDay = todayDay;
      } else if (daysWithData.size > 0) {
        selectDay = Math.max.apply(null, Array.from(daysWithData));
      }

      if (selectDay) {
        var btn = grid.querySelector('.calendar-day[data-day="' + selectDay + '"]');
        if (btn) btn.click();
      } else {
        renderEmptyRecordings();
      }
    })
    .catch(function(err) { console.error('Failed to load calendar data:', err); });
}

function formatBytesJS(bytes) {
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  return (bytes / (1024 * 1024 * 1024)).toFixed(1) + ' GB';
}

function humanizeName(name) {
  return name.replace(/_/g, ' ').replace(/\b\w/g, function(c) { return c.toUpperCase(); });
}

function renderEmptyRecordings() {
  var summary = el('day-summary');
  var cards = el('camera-cards');
  if (summary) summary.innerHTML = '';
  if (cards) cards.innerHTML = '<div class="empty-state"><p>No recordings for this date.</p></div>';
}

function loadRecordingsSummary(date) {
  var dateStr = date.getFullYear() + '-' + String(date.getMonth() + 1).padStart(2, '0') + '-' + String(date.getDate()).padStart(2, '0');

  fetch('/api/recordings/summary?date=' + dateStr)
    .then(function(resp) { return resp.json(); })
    .then(function(data) {
      renderRecordingsSummary(data, date);
    })
    .catch(function(err) {
      console.error('Failed to load recordings summary:', err);
    });
}

function renderRecordingsSummary(data, date) {
  var summary = el('day-summary');
  var cardsContainer = el('camera-cards');
  if (!summary || !cardsContainer) return;

  var cameras = data.cameras || [];
  var totalBytes = data.total_bytes || 0;

  if (cameras.length === 0) {
    summary.innerHTML = '';
    cardsContainer.innerHTML = '<div class="empty-state"><p>No recordings for this date.</p></div>';
    return;
  }

  // Day summary bar
  var dateLabel = date.toLocaleDateString('en-US', { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' });
  summary.innerHTML =
    '<div class="day-summary-bar">' +
      '<span class="day-summary-date">' + dateLabel + '</span>' +
      '<span class="day-summary-stats">' +
        '<span>' + cameras.length + ' camera' + (cameras.length !== 1 ? 's' : '') + '</span>' +
        '<span class="day-summary-dot"></span>' +
        '<span>' + formatBytesJS(totalBytes) + '</span>' +
      '</span>' +
    '</div>';

  // Camera cards
  cardsContainer.innerHTML = '';
  cameras.forEach(function(cam) {
    var card = document.createElement('div');
    card.className = 'rec-camera-card';

    // Header row
    var header = document.createElement('div');
    header.className = 'rec-camera-header';
    header.innerHTML =
      '<a href="/camera.html?name=' + encodeURIComponent(cam.name) + '" class="rec-camera-name">' + humanizeName(cam.name) + '</a>' +
      '<span class="rec-camera-size">' + formatBytesJS(cam.total_bytes) + '</span>';
    card.appendChild(header);

    // Coverage bar (24h)
    var barWrap = document.createElement('div');
    barWrap.className = 'rec-coverage-bar';

    // Merge adjacent segments (gap < 60s)
    var blocks = [];
    cam.segments.forEach(function(seg) {
      var start = new Date(seg.start_time);
      var end = new Date(seg.end_time);
      var startSec = start.getHours() * 3600 + start.getMinutes() * 60 + start.getSeconds();
      var endSec = end.getHours() * 3600 + end.getMinutes() * 60 + end.getSeconds();
      if (endSec <= startSec) endSec = 86400;

      if (blocks.length > 0 && startSec - blocks[blocks.length - 1].end <= 60) {
        if (endSec > blocks[blocks.length - 1].end) {
          blocks[blocks.length - 1].end = endSec;
        }
      } else {
        blocks.push({ start: startSec, end: endSec });
      }
    });

    blocks.forEach(function(block) {
      var startPct = block.start / 86400 * 100;
      var widthPct = (block.end - block.start) / 86400 * 100;
      if (widthPct < 0.2) widthPct = 0.2;
      var div = document.createElement('div');
      div.className = 'rec-coverage-fill';
      div.style.left = startPct + '%';
      div.style.width = widthPct + '%';
      barWrap.appendChild(div);
    });

    // Click on bar to navigate to camera playback
    barWrap.addEventListener('click', function(e) {
      var rect = barWrap.getBoundingClientRect();
      var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
      var totalSec = pct * 86400;
      var h = Math.floor(totalSec / 3600);
      var m = Math.floor((totalSec % 3600) / 60);
      var s = Math.floor(totalSec % 60);
      var ts = date.getFullYear() + '-' +
        String(date.getMonth() + 1).padStart(2, '0') + '-' +
        String(date.getDate()).padStart(2, '0') + 'T' +
        String(h).padStart(2, '0') + ':' +
        String(m).padStart(2, '0') + ':' +
        String(s).padStart(2, '0') + 'Z';
      location.href = '/camera.html?name=' + encodeURIComponent(cam.name) + '&t=' + encodeURIComponent(ts);
    });

    // Hover cursor
    var cursor = document.createElement('div');
    cursor.className = 'rec-coverage-cursor';
    var cursorTime = document.createElement('span');
    cursorTime.className = 'rec-coverage-cursor-time';
    cursor.appendChild(cursorTime);
    barWrap.appendChild(cursor);

    barWrap.addEventListener('mousemove', function(e) {
      var rect = barWrap.getBoundingClientRect();
      var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
      cursor.style.left = (pct * 100) + '%';
      cursor.style.display = '';
      var totalSec = pct * 86400;
      var h = Math.floor(totalSec / 3600);
      var m = Math.floor((totalSec % 3600) / 60);
      cursorTime.textContent = String(h).padStart(2, '0') + ':' + String(m).padStart(2, '0');
    });

    barWrap.addEventListener('mouseleave', function() {
      cursor.style.display = 'none';
    });

    card.appendChild(barWrap);

    // Time labels
    var labels = document.createElement('div');
    labels.className = 'rec-coverage-labels';
    labels.innerHTML = '<span>00</span><span>03</span><span>06</span><span>09</span><span>12</span><span>15</span><span>18</span><span>21</span><span>24</span>';
    card.appendChild(labels);

    // Expandable segment list
    var toggle = document.createElement('button');
    toggle.className = 'btn btn-sm rec-segments-toggle';
    toggle.innerHTML =
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14"><polyline points="6 9 12 15 18 9"/></svg>' +
      cam.segments.length + ' segment' + (cam.segments.length !== 1 ? 's' : '');

    var segList = document.createElement('div');
    segList.className = 'rec-segment-list hidden';

    cam.segments.forEach(function(seg) {
      var start = new Date(seg.start_time);
      var end = new Date(seg.end_time);
      var startLocal = String(start.getHours()).padStart(2, '0') + ':' + String(start.getMinutes()).padStart(2, '0');
      var endLocal = String(end.getHours()).padStart(2, '0') + ':' + String(end.getMinutes()).padStart(2, '0');
      var durMin = Math.round((end - start) / 60000);
      var durStr = durMin >= 60 ? Math.floor(durMin / 60) + 'h' + (durMin % 60 ? durMin % 60 + 'm' : '') : durMin + 'm';

      var row = document.createElement('div');
      row.className = 'rec-segment-row';
      row.innerHTML =
        '<span class="rec-seg-time">' + startLocal + ' – ' + endLocal + '</span>' +
        '<span class="rec-seg-dur">' + durStr + '</span>' +
        '<span class="rec-seg-size">' + formatBytesJS(seg.size_bytes) + '</span>' +
        '<span class="rec-seg-actions">' +
          '<a href="/camera.html?name=' + encodeURIComponent(cam.name) + '&t=' + encodeURIComponent(seg.start_time) + '" class="btn btn-sm" title="Play">' +
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14"><polygon points="5 3 19 12 5 21 5 3"/></svg></a>' +
        '</span>';
      segList.appendChild(row);
    });

    toggle.onclick = function() {
      var isHidden = segList.classList.contains('hidden');
      segList.classList.toggle('hidden');
      toggle.classList.toggle('active');
    };

    card.appendChild(toggle);
    card.appendChild(segList);
    cardsContainer.appendChild(card);
  });
}

// ─── Keyboard Shortcuts ───
document.addEventListener('keydown', function(e) {
  // Don't capture when typing in inputs
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT' || e.target.tagName === 'TEXTAREA') return;

  switch (e.key) {
    case '?':
      toggleShortcutModal();
      break;
    case 'w':
    case 'W':
      if (el('btn-webrtc')) startMSE();
      break;
    case 'm':
    case 'M':
      if (el('btn-mjpeg')) startMJPEG();
      break;
    case 's':
    case 'S':
      if (el('btn-stop') && currentStream) stopStream();
      break;
    case 'p':
    case 'P':
      if (el('btn-pip')) togglePiP();
      break;
    case 'l':
    case 'L':
      if (playbackMode) returnToLive();
      break;
    case 'a':
    case 'A':
      if (el('btn-mute')) toggleMute();
      break;
    case 'f':
    case 'F':
      if (el('live-viewport')) toggleFullscreen();
      break;
    case ' ':
      if (el('btn-pause')) { togglePause(); e.preventDefault(); }
      break;
    case 'ArrowLeft': {
      var prev = document.querySelector('[data-prev-id]');
      if (prev) { location.href = prev.href; e.preventDefault(); }
      break;
    }
    case 'ArrowRight': {
      var next = document.querySelector('[data-next-id]');
      if (next) { location.href = next.href; e.preventDefault(); }
      break;
    }
    case 'd':
    case 'D': {
      var clipLink = document.querySelector('.download-row[href*="/clip"]');
      if (clipLink) { clipLink.click(); }
      break;
    }
    case 'Escape':
      if (el('account-modal') && el('account-modal').classList.contains('open')) {
        closeAccountModal();
      } else if (el('shortcut-modal') && el('shortcut-modal').classList.contains('open')) {
        closeShortcutModal();
      } else if (document.fullscreenElement) {
        document.exitFullscreen();
      } else if (playbackMode) {
        returnToLive();
      } else if (currentStream) {
        stopStream();
      }
      break;
    case '1':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/'; }
      break;
    case '2':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/events.html'; }
      break;
    case '3':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/recordings.html'; }
      break;
    case '4':
      if (e.ctrlKey || e.metaKey) { e.preventDefault(); location.href = '/system.html'; }
      break;
  }
});

// ─── Theme Toggle ───
function initTheme() {
  var saved = localStorage.getItem('vedetta-theme');
  if (saved === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
  }
  updateThemeUI();
}

function toggleTheme() {
  var current = document.documentElement.getAttribute('data-theme');
  var next = current === 'light' ? 'dark' : 'light';

  if (next === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
  } else {
    document.documentElement.removeAttribute('data-theme');
  }

  localStorage.setItem('vedetta-theme', next);
  updateThemeUI();
}

function updateThemeUI() {
  var isLight = document.documentElement.getAttribute('data-theme') === 'light';
  var iconDark = document.getElementById('theme-icon-dark');
  var iconLight = document.getElementById('theme-icon-light');
  var meta = document.querySelector('meta[name="theme-color"]');

  if (iconDark) iconDark.style.display = isLight ? 'none' : '';
  if (iconLight) iconLight.style.display = isLight ? '' : 'none';
  if (meta) meta.setAttribute('content', isLight ? '#ffffff' : '#0a0e14');
}

initTheme();

// ─── Connection Status ───
var connDebounceTimer = null;

function setConnStatus(ok) {
  var dot = document.getElementById('conn-dot');
  var label = document.getElementById('conn-label');
  if (!dot || !label) return;

  if (ok) {
    dot.className = 'conn-dot ok';
    label.textContent = 'Connected';
  } else {
    dot.className = 'conn-dot error';
    label.textContent = 'Reconnecting...';
  }
}

document.addEventListener('htmx:sendError', function() {
  clearTimeout(connDebounceTimer);
  setConnStatus(false);
});

document.addEventListener('htmx:responseError', function(e) {
  console.error('HTMX error:', e.detail);
  clearTimeout(connDebounceTimer);
  setConnStatus(false);
});

document.addEventListener('htmx:afterRequest', function(e) {
  if (!e.detail.failed) {
    clearTimeout(connDebounceTimer);
    connDebounceTimer = setTimeout(function() {
      setConnStatus(true);
    }, 300);
  }
});

// ─── Page visibility: pause updates when hidden ───
document.addEventListener('visibilitychange', function() {
  if (document.hidden) {
    stopBirdseye();
    stopGridSnapshotRefresh();
    stopStatsRefresh();
  } else {
    if (localStorage.getItem('vedetta-view') === 'birdseye') {
      var birdseyeGrid = el('birdseye-grid');
      if (birdseyeGrid && birdseyeGrid.style.display !== 'none') {
        startBirdseye();
      }
    } else if (el('camera-grid')) {
      startGridSnapshotRefresh();
    }
    if (el('stats-row')) {
      startStatsRefresh();
    }
  }
});

// ─── Grid Snapshot Refresh ───
// Instead of htmx replacing the entire grid DOM every 2s (causes flash),
// update only the <img> src attributes with a cache-busting timestamp.
let gridSnapshotInterval = null;

function startGridSnapshotRefresh() {
  stopGridSnapshotRefresh();
  gridSnapshotInterval = setInterval(refreshGridSnapshots, 2000);
}

function stopGridSnapshotRefresh() {
  if (gridSnapshotInterval) {
    clearInterval(gridSnapshotInterval);
    gridSnapshotInterval = null;
  }
}

function refreshGridSnapshots() {
  var grid = el('camera-grid');
  if (!grid || grid.style.display === 'none') return;

  // Fetch live camera status and update badges + snapshots in one pass
  fetch('/api/cameras')
    .then(function(r) { return r.ok ? r.json() : null; })
    .then(function(cameras) {
      if (!cameras) return;
      var statusMap = {};
      cameras.forEach(function(c) { statusMap[c.name] = c; });

      var cards = grid.querySelectorAll('.cam-card');
      var t = Date.now();
      cards.forEach(function(card) {
        var img = card.querySelector('.cam-preview img');
        if (!img) return;
        var name = img.alt;
        var cam = statusMap[name];
        if (!cam) return;

        // Update LIVE/OFFLINE badge
        var dot = card.querySelector('.cam-live-dot');
        var badge = card.querySelector('.cam-live-badge');
        if (dot && badge) {
          dot.className = 'cam-live-dot' + (cam.online ? '' : ' offline');
          var label = badge.lastChild;
          if (label) label.textContent = cam.online ? 'LIVE' : 'OFFLINE';
        }

        // Refresh snapshot for online cameras
        if (cam.online) {
          var base = img.src.split('?')[0];
          img.src = base + '?t=' + t;
        }
      });
    })
    .catch(function() {});
}

// Start refresh after htmx loads the grid initially
document.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail.target && e.detail.target.id === 'camera-grid') {
    startGridSnapshotRefresh();
  }
});

// ─── Stats Refresh ───
// Fetch dashboard stats as HTML fragment and diff-update only changed values.
let statsInterval = null;

function startStatsRefresh() {
  stopStatsRefresh();
  statsInterval = setInterval(refreshStats, 5000);
}

function stopStatsRefresh() {
  if (statsInterval) {
    clearInterval(statsInterval);
    statsInterval = null;
  }
}

function refreshStats() {
  var row = el('stats-row');
  if (!row) return;
  fetch('/partials/dashboard-stats')
    .then(function(r) { return r.ok ? r.text() : null; })
    .then(function(html) {
      if (!html) return;
      var tmp = document.createElement('div');
      tmp.innerHTML = html;
      var newValues = tmp.querySelectorAll('.stat-value');
      var oldValues = row.querySelectorAll('.stat-value');
      newValues.forEach(function(nv, i) {
        if (i < oldValues.length && oldValues[i].innerHTML !== nv.innerHTML) {
          oldValues[i].innerHTML = nv.innerHTML;
          oldValues[i].className = nv.className;
        }
      });
    })
    .catch(function() {});
}

document.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail.target && e.detail.target.id === 'stats-row') {
    startStatsRefresh();
  }
});

// ─── Staggered Fade-In ───
// Apply fade-in classes to cards after htmx swaps
document.addEventListener('htmx:afterSwap', function(e) {
  var target = e.detail.target;
  if (!target) return;

  // Stagger stat cards
  var statCards = target.querySelectorAll('.stat-card');
  statCards.forEach(function(card, i) {
    card.classList.add('fade-in', 'stagger-' + Math.min(i + 1, 4));
  });

  // Stagger camera cards
  var camCards = target.querySelectorAll('.cam-card');
  camCards.forEach(function(card, i) {
    card.classList.add('fade-in', 'stagger-' + Math.min(i + 1, 4));
  });

  // Stagger event cards
  var eventCards = target.querySelectorAll('.event-card');
  eventCards.forEach(function(card, i) {
    card.classList.add('fade-in', 'stagger-' + Math.min(i + 1, 4));
  });
});

// ─── Localize <time> Elements ───
// Server renders <time datetime="ISO"> with UTC display text.
// This converts them to the user's local timezone in the browser.
function localizeTimeElements(root) {
  (root || document).querySelectorAll('time[datetime]').forEach(function(el) {
    var d = new Date(el.getAttribute('datetime'));
    if (isNaN(d)) return;
    el.textContent = d.getFullYear() + '-' +
      String(d.getMonth() + 1).padStart(2, '0') + '-' +
      String(d.getDate()).padStart(2, '0') + ' ' +
      String(d.getHours()).padStart(2, '0') + ':' +
      String(d.getMinutes()).padStart(2, '0') + ':' +
      String(d.getSeconds()).padStart(2, '0');
  });
}

document.addEventListener('htmx:afterSwap', function(e) {
  localizeTimeElements(e.detail.target);
});

// ─── Keyboard Shortcut Modal ───
function toggleShortcutModal() {
  var backdrop = el('shortcut-backdrop');
  var modal = el('shortcut-modal');
  if (!backdrop || !modal) return;

  var isOpen = modal.classList.contains('open');
  if (isOpen) {
    closeShortcutModal();
  } else {
    openShortcutModal();
  }
}

function openShortcutModal() {
  var backdrop = el('shortcut-backdrop');
  var modal = el('shortcut-modal');
  if (!backdrop || !modal) return;

  backdrop.classList.add('open');
  modal.classList.add('open');
  // Trap focus
  modal.querySelector('button')?.focus();
}

function closeShortcutModal() {
  var backdrop = el('shortcut-backdrop');
  var modal = el('shortcut-modal');
  if (!backdrop || !modal) return;

  backdrop.classList.remove('open');
  modal.classList.remove('open');
}

// Close modal on Escape (handled in keydown handler via existing Escape case)
// Close modal on backdrop click
document.addEventListener('click', function(e) {
  if (e.target && e.target.id === 'shortcut-backdrop') {
    closeShortcutModal();
  }
  if (e.target && e.target.id === 'account-backdrop') {
    closeAccountModal();
  }
});

// ─── Account Modal ───
function openAccountModal() {
  var backdrop = el('account-backdrop');
  var modal = el('account-modal');
  if (!backdrop || !modal) return;

  fetch('/api/auth/me').then(function(resp) {
    if (!resp.ok) return;
    return resp.json();
  }).then(function(data) {
    if (data && data.username) {
      el('account-username').textContent = data.username;
    }
  });

  // Reset form
  var form = el('change-password-form');
  if (form) form.reset();
  var status = el('cp-status');
  if (status) { status.textContent = ''; status.style.color = ''; }

  backdrop.classList.add('open');
  modal.classList.add('open');
  modal.querySelector('button')?.focus();
}

function closeAccountModal() {
  var backdrop = el('account-backdrop');
  var modal = el('account-modal');
  if (!backdrop || !modal) return;

  backdrop.classList.remove('open');
  modal.classList.remove('open');
}

(function() {
  var form = document.getElementById('change-password-form');
  if (!form) return;
  form.addEventListener('submit', function(e) {
    e.preventDefault();
    var status = document.getElementById('cp-status');
    var newPw = document.getElementById('cp-new').value;
    var confirmPw = document.getElementById('cp-confirm').value;

    if (newPw !== confirmPw) {
      status.textContent = 'New passwords do not match.';
      status.style.color = 'var(--danger, #ff7d7d)';
      return;
    }

    status.textContent = 'Updating...';
    status.style.color = 'var(--text-secondary)';

    fetch('/api/auth/change-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        current_password: document.getElementById('cp-current').value,
        new_password: newPw
      })
    }).then(function(resp) {
      return resp.json().then(function(body) { return { ok: resp.ok, body: body }; });
    }).then(function(result) {
      if (result.ok) {
        status.textContent = 'Password updated successfully.';
        status.style.color = 'var(--success, #5dff7d)';
        form.reset();
      } else {
        status.textContent = result.body.error || 'Failed to update password.';
        status.style.color = 'var(--danger, #ff7d7d)';
      }
    }).catch(function() {
      status.textContent = 'Network error. Please try again.';
      status.style.color = 'var(--danger, #ff7d7d)';
    });
  });
})();

// ─── Real-time Playhead Animation ───
var playheadRAF = null;
var lastTimelineRefresh = 0;

function startPlayheadAnimation() {
  if (playheadRAF) cancelAnimationFrame(playheadRAF);

  function tick() {
    if (!playbackMode && !timelineDragging) {
      var playhead = el('timeline-playhead');
      if (playhead) {
        var now = new Date();
        var today = new Date();
        if (timelineDate.toDateString() === today.toDateString()) {
          var pct = (now.getHours() * 60 + now.getMinutes() + now.getSeconds() / 60) / (24 * 60) * 100;
          playhead.style.left = pct + '%';
          playhead.style.display = '';

          // Refresh timeline segments every 30s so blue bars stay current
          var ts = Date.now();
          if (ts - lastTimelineRefresh > 30000) {
            lastTimelineRefresh = ts;
            fetchTimelineData();
          }
        }
      }
    }
    playheadRAF = requestAnimationFrame(tick);
  }
  tick();
}

// ─── Infinite Scroll for Events ───
var infiniteScrollObserver = null;
var eventsOffset = 0;
var eventsLoading = false;
var eventsExhausted = false;

function initInfiniteScroll() {
  var gallery = el('events-gallery');
  if (!gallery) return;

  eventsOffset = 0;
  eventsExhausted = false;
  eventsLoading = false;

  // Watch for initial load completion, then set up sentinel
  var checkReady = setInterval(function() {
    var cards = gallery.querySelectorAll('.event-card');
    if (cards.length > 0 || gallery.querySelector('.empty-state')) {
      clearInterval(checkReady);
      eventsOffset = cards.length;
      if (cards.length >= 50) {
        addScrollSentinel();
      }
    }
  }, 500);
}

function addScrollSentinel() {
  var gallery = el('events-gallery');
  if (!gallery || gallery.querySelector('.scroll-sentinel')) return;

  var sentinel = document.createElement('div');
  sentinel.className = 'scroll-sentinel';
  sentinel.innerHTML = '<div class="loading-spinner"></div>';
  gallery.appendChild(sentinel);

  if (infiniteScrollObserver) infiniteScrollObserver.disconnect();
  infiniteScrollObserver = new IntersectionObserver(function(entries) {
    if (entries[0].isIntersecting && !eventsLoading && !eventsExhausted) {
      loadMoreEvents();
    }
  }, { rootMargin: '200px' });

  infiniteScrollObserver.observe(sentinel);
}

function loadMoreEvents() {
  if (eventsLoading || eventsExhausted) return;
  eventsLoading = true;

  var labelChip = document.querySelector('.chip[data-filter="label"].active');
  var cameraChip = document.querySelector('.chip[data-filter="camera"].active');

  var url = '/partials/events-gallery?limit=50&offset=' + eventsOffset;
  if (labelChip && labelChip.dataset.value) {
    url += '&label=' + encodeURIComponent(labelChip.dataset.value);
  }
  if (cameraChip && cameraChip.dataset.value) {
    url += '&camera=' + encodeURIComponent(cameraChip.dataset.value);
  }

  fetch(url)
    .then(function(resp) { return resp.text(); })
    .then(function(html) {
      var gallery = el('events-gallery');
      if (!gallery) return;

      // Remove old sentinel
      var oldSentinel = gallery.querySelector('.scroll-sentinel');
      if (oldSentinel) oldSentinel.remove();

      // Parse and count new cards
      var tmp = document.createElement('div');
      tmp.innerHTML = html;
      var newCards = tmp.querySelectorAll('.event-card');

      if (newCards.length === 0) {
        eventsExhausted = true;
        eventsLoading = false;
        return;
      }

      // Append new cards with stagger animation
      newCards.forEach(function(card, i) {
        card.classList.add('fade-in', 'stagger-' + Math.min(i + 1, 4));
        gallery.appendChild(card);
      });

      eventsOffset += newCards.length;
      eventsLoading = false;

      // Add new sentinel if we got a full page
      if (newCards.length >= 50) {
        addScrollSentinel();
      }
    })
    .catch(function(err) {
      console.error('Failed to load more events:', err);
      eventsLoading = false;
    });
}

// ─── Event Search ───
var searchDebounceTimer = null;

function initEventSearch() {
  var input = el('event-search');
  if (!input) return;

  input.addEventListener('input', function() {
    clearTimeout(searchDebounceTimer);
    searchDebounceTimer = setTimeout(function() {
      reloadEvents();
    }, 300);
  });
}

// Patch reloadEvents to include search term
var _origReloadEvents = typeof reloadEvents === 'function' ? reloadEvents : null;

function reloadEventsWithSearch() {
  var gallery = el('events-gallery');
  if (!gallery) return;

  var labelChip = document.querySelector('.chip[data-filter="label"].active');
  var cameraChip = document.querySelector('.chip[data-filter="camera"].active');
  var searchInput = el('event-search');

  var url = '/partials/events-gallery?limit=50';
  if (labelChip && labelChip.dataset.value) {
    url += '&label=' + encodeURIComponent(labelChip.dataset.value);
  }
  if (cameraChip && cameraChip.dataset.value) {
    url += '&camera=' + encodeURIComponent(cameraChip.dataset.value);
  }
  if (searchInput && searchInput.value.trim()) {
    url += '&q=' + encodeURIComponent(searchInput.value.trim());
  }

  // Reset infinite scroll state
  eventsOffset = 0;
  eventsExhausted = false;

  htmx.ajax('GET', url, { target: '#events-gallery', swap: 'innerHTML' });
}

// Override reloadEvents to include search
reloadEvents = function() {
  reloadEventsWithSearch();
};

// ─── Timeline Date Picker ───
function openTimelineDatePicker() {
  var input = el('timeline-date-input');
  if (input) {
    input.showPicker();
  }
}

function onTimelineDatePick(input) {
  if (!input.value) return;
  var parts = input.value.split('-');
  timelineDate = new Date(parseInt(parts[0]), parseInt(parts[1]) - 1, parseInt(parts[2]));
  updateTimelineDate();
  updatePlayheadToNow();
  fetchTimelineData();
}

// ─── WebRTC Auto-Reconnect ───
var webrtcReconnectAttempts = 0;
var webrtcMaxReconnect = 3;
var webrtcReconnectTimer = null;

function webrtcAutoReconnect() {
  if (webrtcReconnectAttempts >= webrtcMaxReconnect) {
    toast('WebRTC reconnect failed after ' + webrtcMaxReconnect + ' attempts', 'error');
    webrtcReconnectAttempts = 0;
    return;
  }

  webrtcReconnectAttempts++;
  var delay = Math.min(1000 * Math.pow(2, webrtcReconnectAttempts - 1), 8000);
  toast('Reconnecting WebRTC (' + webrtcReconnectAttempts + '/' + webrtcMaxReconnect + ')...');

  webrtcReconnectTimer = setTimeout(function() {
    startWebRTC().then(function() {
      webrtcReconnectAttempts = 0;
    }).catch(function() {
      webrtcAutoReconnect();
    });
  }, delay);
}

// ─── Health Monitor ───
var healthWarningVisible = false;

function pollHealth() {
  fetch('/api/health')
    .then(function(resp) { return resp.json(); })
    .then(function(data) {
      var storage = data.checks && data.checks.storage;
      if (!storage) return;

      if (storage.disk_low || storage.recording_paused) {
        showDiskWarning(storage);
      } else {
        hideDiskWarning();
      }
    })
    .catch(function() {});
}

function showDiskWarning(storage) {
  var banner = document.getElementById('disk-warning');
  if (!banner) {
    banner = document.createElement('div');
    banner.id = 'disk-warning';
    banner.className = 'disk-warning';
    var page = document.querySelector('.page');
    if (page) page.insertBefore(banner, page.firstChild);
  }

  var msg = storage.recording_paused
    ? 'Recording paused — disk space critically low (' + storage.disk_available + ' free)'
    : 'Disk space low (' + storage.disk_available + ' free) — recording may stop soon';

  banner.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>' + msg;
  banner.style.display = '';
  healthWarningVisible = true;
}

function hideDiskWarning() {
  if (!healthWarningVisible) return;
  var banner = document.getElementById('disk-warning');
  if (banner) banner.style.display = 'none';
  healthWarningVisible = false;
}

// ─── Object Tracking ───
function trackObject(eventId, label) {
  var searchBox = document.getElementById('identify-search');
  var prefill = searchBox ? searchBox.value.trim() : '';
  showInputModal('Track ' + label, 'Name this ' + label + ':', prefill, function(name) {
    var endpoint = label === 'person' ? '/api/events/' + eventId + '/track-person' : '/api/objects';
    var body = label === 'person'
      ? JSON.stringify({name: name})
      : JSON.stringify({event_id: eventId, name: name});

    fetch(endpoint, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: body
    }).then(function(r) {
      if (!r.ok) return r.json().then(function(e) { throw new Error(e.error); });
      return r.json();
    }).then(function(result) {
      toast('"' + name + '" is now being tracked');
      reloadEventDetail(eventId);
    }).catch(function(e) {
      toast('Failed: ' + e.message);
    });
  });
}

function assignPersonToEvent(personId, personName, eventId) {
  fetch('/api/events/' + eventId + '/assign-person', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({person_id: personId})
  }).then(function(r) {
    if (!r.ok) return r.json().then(function(e) { throw new Error(e.error); });
    return r.json();
  }).then(function() {
    toast('Identified as ' + personName);
    reloadEventDetail(eventId);
  }).catch(function(e) {
    toast('Failed: ' + e.message);
  });
}

function renderDetectionOverlay(img) {
  var wrap = img.parentElement;
  if (!wrap || wrap.querySelector('.detection-box')) return;

  var natW = img.naturalWidth;
  var natH = img.naturalHeight;
  if (!natW || !natH) return;

  var x1 = parseInt(img.dataset.boxX1) || 0;
  var y1 = parseInt(img.dataset.boxY1) || 0;
  var x2 = parseInt(img.dataset.boxX2) || 0;
  var y2 = parseInt(img.dataset.boxY2) || 0;
  var label = img.dataset.label || '';
  var subLabel = img.dataset.subLabel || '';
  var score = img.dataset.score || '';
  var eventId = img.dataset.eventId || '';
  var identified = subLabel !== '';
  var color = identified ? '#00ff64' : '#5dd7ff';

  // Convert to percentages
  var left = (x1 / natW * 100) + '%';
  var top = (y1 / natH * 100) + '%';
  var width = ((x2 - x1) / natW * 100) + '%';
  var height = ((y2 - y1) / natH * 100) + '%';

  // Bounding box
  var box = document.createElement('div');
  box.className = 'detection-box';
  box.style.cssText = 'position:absolute;left:' + left + ';top:' + top + ';width:' + width + ';height:' + height
    + ';border:2px solid ' + color + ';border-radius:3px;cursor:pointer;transition:border-color 0.2s';
  box.addEventListener('mouseenter', function() { box.style.borderWidth = '3px'; });
  box.addEventListener('mouseleave', function() { box.style.borderWidth = '2px'; });
  box.addEventListener('click', function(e) {
    e.stopPropagation();
    toggleDetectionPopover(box, eventId, label);
  });

  // Label tag above box
  var tag = document.createElement('div');
  tag.className = 'detection-tag';
  tag.style.cssText = 'position:absolute;bottom:100%;left:0;padding:1px 6px;font-size:13px;font-weight:700;'
    + 'background:' + color + ';color:#000;border-radius:3px 3px 0 0;white-space:nowrap;pointer-events:none';
  tag.textContent = subLabel || (label + ' ' + score);
  box.appendChild(tag);

  wrap.appendChild(box);
}

function toggleDetectionPopover(box, eventId, label) {
  var existing = box.querySelector('.detection-popover');
  if (existing) { existing.remove(); return; }

  // Close any other open popovers
  document.querySelectorAll('.detection-popover').forEach(function(p) { p.remove(); });

  var pop = document.createElement('div');
  pop.className = 'detection-popover';

  // Build buttons from the sidebar data if available
  var html = '<div style="font-size:var(--text-sm);font-weight:600;margin-bottom:0.25rem">Identify this ' + label + '</div>';

  // Check for named people buttons in the sidebar
  var sidebar = document.querySelector('.event-sidebar');
  if (sidebar && label === 'person') {
    var personBtns = sidebar.querySelectorAll('[onclick*="assignPersonToEvent"]');
    personBtns.forEach(function(btn) {
      var match = btn.getAttribute('onclick').match(/assignPersonToEvent\((\d+),\s*'([^']+)'/);
      if (match) {
        html += '<button class="btn btn-sm" style="width:100%;margin-bottom:0.2rem" '
          + 'onclick="assignPersonToEvent(' + match[1] + ',\'' + match[2] + '\',\'' + eventId + '\')">'
          + 'This is ' + match[2] + '</button>';
      }
    });
  }

  var objectBtns = sidebar ? sidebar.querySelectorAll('[onclick*="addObjectReference"]') : [];
  objectBtns.forEach(function(btn) {
    var match = btn.getAttribute('onclick').match(/addObjectReference\((\d+),\s*'([^']+)'/);
    if (match) {
      html += '<button class="btn btn-sm" style="width:100%;margin-bottom:0.2rem" '
        + 'onclick="addObjectReference(' + match[1] + ',\'' + match[2] + '\',\'' + eventId + '\')">'
        + 'This is ' + match[2] + '</button>';
    }
  });

  html += '<button class="btn btn-sm btn-ghost" style="width:100%" '
    + 'onclick="trackObject(\'' + eventId + '\',\'' + label + '\')">Track as new ' + label + '</button>';

  pop.innerHTML = html;
  box.appendChild(pop);

  // Close on outside click
  setTimeout(function() {
    document.addEventListener('click', function closePopover(e) {
      if (!pop.contains(e.target)) {
        pop.remove();
        document.removeEventListener('click', closePopover);
      }
    });
  }, 0);
}

// Identify grid: searchable face/object picker
var _identifyData = [];

function loadIdentifyGrid() {
  var grid = document.getElementById('identify-grid');
  if (!grid) return;
  var label = grid.dataset.label;

  if (label === 'person') {
    fetch('/api/people').then(function(r) { return r.json(); }).then(function(data) {
      _identifyData = (data.people || data || []).filter(function(p) { return p.name && !p.ignore; });
      // Deduplicate by name (keep first)
      var seen = {};
      _identifyData = _identifyData.filter(function(p) {
        if (seen[p.name.toLowerCase()]) return false;
        seen[p.name.toLowerCase()] = true;
        return true;
      });
      renderIdentifyResults('');
      // Load thumbnails
      _identifyData.forEach(function(p) {
        if (p.face_count > 0) {
          fetch('/api/people/' + p.id + '/faces?limit=1').then(function(r) { return r.json(); }).then(function(faces) {
            if (faces && faces.length > 0) {
              var el = document.getElementById('id-thumb-' + p.id);
              if (el) el.style.backgroundImage = 'url(/api/faces/' + faces[0].id + '/crop)';
            }
          });
        } else if (p.source_event_id) {
          var el = document.getElementById('id-thumb-' + p.id);
          if (el) el.style.backgroundImage = 'url(/api/events/' + p.source_event_id + '/detection-crop)';
        }
      });
    });
  } else {
    fetch('/api/objects').then(function(r) { return r.json(); }).then(function(objects) {
      _identifyData = (objects || []).filter(function(o) { return o.label === label; });
      _identifyData.forEach(function(o) { o._isObject = true; });
      renderIdentifyResults('');
    });
  }
}

function filterIdentifyResults(query) {
  renderIdentifyResults(query);
}

function renderIdentifyResults(query) {
  var grid = document.getElementById('identify-grid');
  if (!grid) return;
  var eventId = grid.dataset.eventId;
  var label = grid.dataset.label;
  var raw = (query || '').trim();
  var q = raw.toLowerCase();

  var filtered = q ? _identifyData.filter(function(p) {
    return p.name.toLowerCase().indexOf(q) !== -1;
  }) : _identifyData;

  var html = '<div style="display:flex;flex-wrap:wrap;gap:0.35rem;margin-top:0.25rem">';
  filtered.forEach(function(p) {
    var escapedName = p.name.replace(/'/g, "\\'");
    var onclick = p._isObject
      ? 'addObjectReference(' + p.id + ',\'' + escapedName + '\',\'' + eventId + '\')'
      : 'assignPersonToEvent(' + p.id + ',\'' + escapedName + '\',\'' + eventId + '\')';
    html += '<div class="identify-chip" onclick="' + onclick + '" title="' + p.name + '">';
    if (p._isObject) {
      html += '<div class="identify-chip-thumb" style="background-image:url(/api/objects/' + p.id + '/crop)"></div>';
    } else {
      html += '<div class="identify-chip-thumb" id="id-thumb-' + p.id + '"></div>';
    }
    html += '<span>' + p.name + '</span></div>';
  });

  // Show "create new" chip if query doesn't match anything exactly
  if (q && !filtered.some(function(p) { return p.name.toLowerCase() === q; })) {
    html += '<div class="identify-chip identify-chip-new" onclick="trackObject(\'' + eventId + '\',\'' + label + '\')" title="Track as new">';
    html += '<div class="identify-chip-thumb" style="display:flex;align-items:center;justify-content:center;font-size:16px;color:var(--accent)">+</div>';
    html += '<span>New: ' + raw + '</span></div>';
  }

  html += '</div>';
  grid.innerHTML = html;
}

function identifyEnter(query, eventId, label) {
  var q = (query || '').trim();
  if (!q) return;
  // Check if exact match exists
  var match = _identifyData.find(function(p) { return p.name.toLowerCase() === q.toLowerCase(); });
  if (match) {
    if (match._isObject) {
      addObjectReference(match.id, match.name, eventId);
    } else {
      assignPersonToEvent(match.id, match.name, eventId);
    }
  } else {
    // Create new
    trackObject(eventId, label);
  }
}

document.addEventListener('htmx:afterSwap', function(e) {
  if (e.detail.target && e.detail.target.id === 'event-detail') {
    loadIdentifyGrid();
  }
});
if (document.getElementById('identify-grid')) loadIdentifyGrid();

function reloadEventDetail(eventId) {
  var detail = document.getElementById('event-detail');
  if (detail && typeof htmx !== 'undefined') {
    htmx.ajax('GET', '/partials/event/' + eventId, {target: '#event-detail', swap: 'innerHTML'});
  }
}

// Generic input modal (replaces prompt())
function showInputModal(title, message, defaultValue, onConfirm) {
  var modal = document.getElementById('input-modal');
  if (!modal) {
    var html = '<div class="shortcut-backdrop" id="input-backdrop"></div>'
      + '<div class="shortcut-modal" id="input-modal" role="dialog">'
      + '<div class="shortcut-modal-header"><h2 id="input-title"></h2>'
      + '<button class="btn btn-icon btn-ghost" onclick="closeInputModal()" aria-label="Close">'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg></button></div>'
      + '<div class="shortcut-modal-body" style="grid-template-columns:1fr;gap:0.75rem;padding:1rem 1.25rem">'
      + '<p id="input-message" style="margin:0;font-size:var(--text-sm);color:var(--text-secondary)"></p>'
      + '<input type="text" id="input-field" class="person-name-input" style="width:100%;padding:0.6rem 0.8rem;font-size:1rem">'
      + '<div style="display:flex;gap:0.5rem;justify-content:flex-end">'
      + '<button class="btn btn-sm btn-ghost" onclick="closeInputModal()">Cancel</button>'
      + '<button class="btn btn-sm btn-primary" id="input-confirm-btn">OK</button>'
      + '</div></div></div>';
    document.body.insertAdjacentHTML('beforeend', html);
    modal = document.getElementById('input-modal');
    document.getElementById('input-backdrop').addEventListener('click', closeInputModal);
  }
  document.getElementById('input-title').textContent = title;
  document.getElementById('input-message').textContent = message;
  var field = document.getElementById('input-field');
  field.value = defaultValue || '';
  document.getElementById('input-confirm-btn').onclick = function() {
    var val = field.value.trim();
    if (!val) return;
    closeInputModal();
    onConfirm(val);
  };
  field.onkeydown = function(e) {
    if (e.key === 'Enter') document.getElementById('input-confirm-btn').click();
  };
  document.getElementById('input-backdrop').classList.add('open');
  modal.classList.add('open');
  setTimeout(function() { field.focus(); field.select(); }, 100);
}

function closeInputModal() {
  var modal = document.getElementById('input-modal');
  var backdrop = document.getElementById('input-backdrop');
  if (modal) modal.classList.remove('open');
  if (backdrop) backdrop.classList.remove('open');
}

function addObjectReference(objectId, objectName, eventId) {
  fetch('/api/objects/' + objectId + '/references', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({event_id: eventId})
  }).then(function(r) {
    if (!r.ok) return r.json().then(function(e) { throw new Error(e.error); });
    return r.json();
  }).then(function() {
    toast('Reference added to "' + objectName + '"');
    reloadEventDetail(eventId);
  }).catch(function(e) {
    toast('Failed: ' + e.message);
  });
}

// ─── Real-time Event Stream (SSE) ───
(function() {
  var evtSource = null;
  var reconnectTimer = null;

  function connectSSE() {
    if (evtSource) return;
    evtSource = new EventSource('/api/events/stream');

    evtSource.onmessage = function(e) {
      try {
        var msg = JSON.parse(e.data);
        if (msg.type === 'doorbell') {
          showDoorbellNotification(msg.data);
        }
      } catch(err) {}
    };

    evtSource.onerror = function() {
      evtSource.close();
      evtSource = null;
      if (!reconnectTimer) {
        reconnectTimer = setTimeout(function() {
          reconnectTimer = null;
          connectSSE();
        }, 5000);
      }
    };
  }

  function showDoorbellNotification(data) {
    var person = data.person || 'Someone';
    var camera = (data.camera || '').replace(/_/g, ' ');

    // Create notification banner
    var banner = document.createElement('div');
    banner.className = 'doorbell-notification';
    banner.innerHTML = '<div class="doorbell-notification-inner">'
      + '<div class="doorbell-icon">'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="24" height="24"><path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>'
      + '</div>'
      + '<div class="doorbell-content">'
      + '<div class="doorbell-title">' + person + ' at the door</div>'
      + '<div class="doorbell-meta">' + camera + '</div>'
      + '</div>'
      + '<img class="doorbell-snapshot" src="/api/cameras/' + data.camera + '/snapshot?t=' + Date.now() + '" alt="snapshot">'
      + '<button class="doorbell-close" onclick="this.parentElement.parentElement.remove()">&times;</button>'
      + '</div>';

    banner.addEventListener('click', function(e) {
      if (e.target.tagName === 'BUTTON') return;
      window.location.href = '/camera.html?name=' + data.camera;
    });

    document.body.appendChild(banner);

    // Auto-dismiss after 30 seconds
    setTimeout(function() {
      if (banner.parentElement) banner.remove();
    }, 30000);

    // Play notification sound if available
    try {
      var audio = new Audio('data:audio/wav;base64,UklGRnoGAABXQVZFZm10IBAAAAABAAEAQB8AAEAfAAABAAgAZGF0YQ==');
      audio.volume = 0.3;
      audio.play().catch(function(){});
    } catch(e) {}
  }

  connectSSE();
})();

// Poll health every 30 seconds
pollHealth();
setInterval(pollHealth, 30000);

// ─── Event Detail: Play Clip on Demand ───
function playEventClip(overlay, eventId) {
  var media = overlay.parentElement;
  var wrap = media.querySelector('#detection-wrap');
  if (wrap) wrap.style.display = 'none';
  overlay.style.display = 'none';

  var video = document.createElement('video');
  video.controls = true;
  video.autoplay = true;
  video.src = '/api/events/' + encodeURIComponent(eventId) + '/clip';
  media.appendChild(video);
}

function playEventRecording(overlay, cameraName, timestamp) {
  var media = overlay.parentElement;
  var wrap = media.querySelector('#detection-wrap');
  if (wrap) wrap.style.display = 'none';
  overlay.style.display = 'none';

  var video = document.createElement('video');
  video.controls = true;
  video.autoplay = true;
  video.src = '/api/cameras/' + encodeURIComponent(cameraName) + '/playback?start=' + encodeURIComponent(timestamp);
  media.appendChild(video);
}

// ─── Zones ───
var zoneData = [];          // current zones from API
var zoneDrawing = false;    // drawing mode active
var zoneEditing = null;     // zone name being edited (null = new)
var zoneDragStart = null;   // {x, y} percentage coords where drag began
var zoneDragRect = null;    // SVG rect element for in-progress draw
var zonePresenceTimer = null;
var zoneSnapshotTimer = null;

// Zone color scheme: blue=regular, green=presence, amber=face
function zoneColor(z) {
  if (z.face_recognition) return { stroke: 'var(--amber)', fill: 'rgba(255, 171, 0, 0.15)' };
  if (z.track_presence) return { stroke: 'var(--green)', fill: 'rgba(0, 230, 118, 0.15)' };
  return { stroke: 'var(--blue)', fill: 'rgba(68, 138, 255, 0.15)' };
}

function zoneRectPoints(x1, y1, x2, y2) {
  return [
    [x1, y1],
    [x2, y1],
    [x2, y2],
    [x1, y2]
  ];
}

function zoneBounds(points) {
  if (!points || !points.length) return { x1: 0, y1: 0, x2: 0, y2: 0 };
  var x1 = points[0][0];
  var y1 = points[0][1];
  var x2 = x1;
  var y2 = y1;
  points.forEach(function(point) {
    x1 = Math.min(x1, point[0]);
    y1 = Math.min(y1, point[1]);
    x2 = Math.max(x2, point[0]);
    y2 = Math.max(y2, point[1]);
  });
  return { x1: x1, y1: y1, x2: x2, y2: y2 };
}

function zoneSvgPoints(points) {
  return (points || []).map(function(point) {
    return (point[0] * 100) + ',' + (point[1] * 100);
  }).join(' ');
}

function initZones() {
  var name = getCameraName();
  if (!name) return;

  var snap = el('zone-snapshot');
  if (snap) {
    snap.src = '/api/cameras/' + encodeURIComponent(name) + '/zones/snapshot?t=' + Date.now();
    // Refresh snapshot every 10 seconds
    zoneSnapshotTimer = setInterval(function() {
      snap.src = '/api/cameras/' + encodeURIComponent(name) + '/zones/snapshot?t=' + Date.now();
    }, 10000);
  }

  loadZones();
  setupZoneDrawEvents();
}

function loadZones() {
  var name = getCameraName();
  if (!name) return;

  fetch('/api/cameras/' + encodeURIComponent(name) + '/zones')
    .then(function(r) { return r.json(); })
    .then(function(zones) {
      zoneData = zones || [];
      renderZoneOverlay();
      renderZoneList();
      startPresencePolling();
    })
    .catch(function(err) {
      console.error('Failed to load zones:', err);
    });
}

function renderZoneOverlay() {
  var svg = el('zone-overlay');
  if (!svg) return;
  svg.setAttribute('viewBox', '0 0 100 100');
  svg.setAttribute('preserveAspectRatio', 'none');

  // Keep any in-progress draw rect
  var drawRect = zoneDragRect;

  // Clear existing zone elements
  while (svg.firstChild) svg.removeChild(svg.firstChild);

  // Re-add draw rect if active
  if (drawRect) svg.appendChild(drawRect);

  zoneData.forEach(function(z) {
    var colors = zoneColor(z);
    var points = z.points || zoneRectPoints(z.x1 || 0, z.y1 || 0, z.x2 || 0, z.y2 || 0);
    var bounds = zoneBounds(points);

    var polygon = document.createElementNS('http://www.w3.org/2000/svg', 'polygon');
    polygon.setAttribute('points', zoneSvgPoints(points));
    polygon.setAttribute('fill', colors.fill);
    polygon.setAttribute('stroke', colors.stroke);
    polygon.setAttribute('stroke-width', '2');
    polygon.classList.add('zone-rect');
    polygon.dataset.zoneName = z.name;
    if (zoneEditing === z.name) polygon.classList.add('selected');

    polygon.addEventListener('click', function(e) {
      e.stopPropagation();
      if (!zoneDrawing) zoneSelect(z.name);
    });

    svg.appendChild(polygon);

    // Label text
    var text = document.createElementNS('http://www.w3.org/2000/svg', 'text');
    text.setAttribute('x', (bounds.x1 * 100) + 0.5);
    text.setAttribute('y', (bounds.y1 * 100) + 4);
    text.classList.add('zone-label-text');
    text.textContent = z.name;
    svg.appendChild(text);
  });
}

function renderZoneList() {
  var container = el('zone-list');
  if (!container) return;

  if (zoneData.length === 0) {
    container.innerHTML = '<div class="zone-list-empty">No zones configured. Click "Add Zone" to create one.</div>';
    return;
  }
  container.innerHTML = '';
}

function setupZoneDrawEvents() {
  var svg = el('zone-overlay');
  if (!svg) return;

  svg.addEventListener('mousedown', function(e) {
    if (!zoneDrawing) return;
    e.preventDefault();
    var pct = svgEventToPercent(e);
    zoneDragStart = pct;

    zoneDragRect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    zoneDragRect.setAttribute('x', pct.x);
    zoneDragRect.setAttribute('y', pct.y);
    zoneDragRect.setAttribute('width', '0');
    zoneDragRect.setAttribute('height', '0');
    zoneDragRect.setAttribute('fill', 'rgba(0, 229, 255, 0.15)');
    zoneDragRect.setAttribute('stroke', 'var(--cyan)');
    zoneDragRect.setAttribute('stroke-width', '2');
    zoneDragRect.setAttribute('stroke-dasharray', '6 3');
    zoneDragRect.setAttribute('rx', '3');
    svg.appendChild(zoneDragRect);
  });

  svg.addEventListener('mousemove', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    e.preventDefault();
    var pct = svgEventToPercent(e);
    var x1 = Math.min(zoneDragStart.x, pct.x);
    var y1 = Math.min(zoneDragStart.y, pct.y);
    var x2 = Math.max(zoneDragStart.x, pct.x);
    var y2 = Math.max(zoneDragStart.y, pct.y);

    zoneDragRect.setAttribute('x', x1);
    zoneDragRect.setAttribute('y', y1);
    zoneDragRect.setAttribute('width', (x2 - x1));
    zoneDragRect.setAttribute('height', (y2 - y1));
  });

  svg.addEventListener('mouseup', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    e.preventDefault();
    var pct = svgEventToPercent(e);
    var x1 = Math.min(zoneDragStart.x, pct.x) / 100;
    var y1 = Math.min(zoneDragStart.y, pct.y) / 100;
    var x2 = Math.max(zoneDragStart.x, pct.x) / 100;
    var y2 = Math.max(zoneDragStart.y, pct.y) / 100;

    // Remove draw rect
    if (zoneDragRect && zoneDragRect.parentNode) {
      zoneDragRect.parentNode.removeChild(zoneDragRect);
    }
    zoneDragRect = null;
    zoneDragStart = null;

    // Minimum size check (at least 2% in each dimension)
    if ((x2 - x1) < 0.02 || (y2 - y1) < 0.02) {
      toast('Zone too small, try again', 'error');
      return;
    }

    // Clamp to 0..1
    x1 = Math.max(0, Math.min(1, x1));
    y1 = Math.max(0, Math.min(1, y1));
    x2 = Math.max(0, Math.min(1, x2));
    y2 = Math.max(0, Math.min(1, y2));

    zoneDrawing = false;
    var overlay = el('zone-overlay');
    if (overlay) overlay.classList.remove('drawing');
    el('zone-draw-hint').classList.add('hidden');
    el('btn-zone-add').classList.remove('hidden');
    el('btn-zone-cancel').classList.add('hidden');

    // Show form for the new zone
    zoneEditing = null;
    var points = zoneRectPoints(x1, y1, x2, y2);
    showZoneForm({
      name: '',
      points: points,
      labels: [],
      track_presence: false,
      face_recognition: false,
      enabled: true
    });
  });

  // Touch events for mobile
  svg.addEventListener('touchstart', function(e) {
    if (!zoneDrawing) return;
    if (e.touches.length !== 1) return;
    e.preventDefault();
    var touch = e.touches[0];
    var pct = svgTouchToPercent(touch);
    zoneDragStart = pct;

    zoneDragRect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    zoneDragRect.setAttribute('x', pct.x);
    zoneDragRect.setAttribute('y', pct.y);
    zoneDragRect.setAttribute('width', '0');
    zoneDragRect.setAttribute('height', '0');
    zoneDragRect.setAttribute('fill', 'rgba(0, 229, 255, 0.15)');
    zoneDragRect.setAttribute('stroke', 'var(--cyan)');
    zoneDragRect.setAttribute('stroke-width', '2');
    zoneDragRect.setAttribute('stroke-dasharray', '6 3');
    zoneDragRect.setAttribute('rx', '3');
    svg.appendChild(zoneDragRect);
  }, { passive: false });

  svg.addEventListener('touchmove', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    if (e.touches.length !== 1) return;
    e.preventDefault();
    var touch = e.touches[0];
    var pct = svgTouchToPercent(touch);
    var x1 = Math.min(zoneDragStart.x, pct.x);
    var y1 = Math.min(zoneDragStart.y, pct.y);
    var x2 = Math.max(zoneDragStart.x, pct.x);
    var y2 = Math.max(zoneDragStart.y, pct.y);

    zoneDragRect.setAttribute('x', x1);
    zoneDragRect.setAttribute('y', y1);
    zoneDragRect.setAttribute('width', (x2 - x1));
    zoneDragRect.setAttribute('height', (y2 - y1));
  }, { passive: false });

  svg.addEventListener('touchend', function(e) {
    if (!zoneDragStart || !zoneDragRect) return;
    // Use last known position from the rect itself
    var x1 = parseFloat(zoneDragRect.getAttribute('x')) / 100;
    var y1 = parseFloat(zoneDragRect.getAttribute('y')) / 100;
    var w = parseFloat(zoneDragRect.getAttribute('width')) / 100;
    var h = parseFloat(zoneDragRect.getAttribute('height')) / 100;
    var x2 = x1 + w;
    var y2 = y1 + h;

    if (zoneDragRect && zoneDragRect.parentNode) {
      zoneDragRect.parentNode.removeChild(zoneDragRect);
    }
    zoneDragRect = null;
    zoneDragStart = null;

    if (w < 0.02 || h < 0.02) {
      toast('Zone too small, try again', 'error');
      return;
    }

    x1 = Math.max(0, Math.min(1, x1));
    y1 = Math.max(0, Math.min(1, y1));
    x2 = Math.max(0, Math.min(1, x2));
    y2 = Math.max(0, Math.min(1, y2));

    zoneDrawing = false;
    var overlay = el('zone-overlay');
    if (overlay) overlay.classList.remove('drawing');
    el('zone-draw-hint').classList.add('hidden');
    el('btn-zone-add').classList.remove('hidden');
    el('btn-zone-cancel').classList.add('hidden');

    zoneEditing = null;
    var points = zoneRectPoints(x1, y1, x2, y2);
    showZoneForm({
      name: '',
      points: points,
      labels: [],
      track_presence: false,
      face_recognition: false,
      enabled: true
    });
  });
}

function svgEventToPercent(e) {
  var svg = el('zone-overlay');
  var rect = svg.getBoundingClientRect();
  var x = ((e.clientX - rect.left) / rect.width) * 100;
  var y = ((e.clientY - rect.top) / rect.height) * 100;
  return { x: Math.max(0, Math.min(100, x)), y: Math.max(0, Math.min(100, y)) };
}

function svgTouchToPercent(touch) {
  var svg = el('zone-overlay');
  var rect = svg.getBoundingClientRect();
  var x = ((touch.clientX - rect.left) / rect.width) * 100;
  var y = ((touch.clientY - rect.top) / rect.height) * 100;
  return { x: Math.max(0, Math.min(100, x)), y: Math.max(0, Math.min(100, y)) };
}

function zoneStartDraw() {
  zoneDrawing = true;
  zoneEditing = null;
  hideZoneForm();
  el('zone-overlay').classList.add('drawing');
  el('zone-draw-hint').classList.remove('hidden');
  el('btn-zone-add').classList.add('hidden');
  el('btn-zone-cancel').classList.remove('hidden');
  renderZoneOverlay();
}

function zoneCancelDraw() {
  zoneDrawing = false;
  if (zoneDragRect && zoneDragRect.parentNode) {
    zoneDragRect.parentNode.removeChild(zoneDragRect);
  }
  zoneDragRect = null;
  zoneDragStart = null;
  el('zone-overlay').classList.remove('drawing');
  el('zone-draw-hint').classList.add('hidden');
  el('btn-zone-add').classList.remove('hidden');
  el('btn-zone-cancel').classList.add('hidden');
}

function zoneSelect(name) {
  var z = zoneData.find(function(zone) { return zone.name === name; });
  if (!z) return;
  zoneEditing = name;
  showZoneForm(z);
  renderZoneOverlay();
  loadZonePresence(z);
}

function showZoneForm(z) {
  var form = el('zone-form');
  form.classList.remove('hidden');
  el('zone-form-title').textContent = zoneEditing ? 'Edit Zone: ' + zoneEditing : 'New Zone';
  el('zone-name').value = z.name || '';
  el('zone-labels').value = (z.labels || []).join(', ');
  el('zone-track-presence').checked = !!z.track_presence;
  el('zone-face-recognition').checked = !!z.face_recognition;
  el('zone-enabled').checked = z.enabled !== false;

  form.dataset.points = JSON.stringify(z.points || zoneRectPoints(z.x1 || 0, z.y1 || 0, z.x2 || 0, z.y2 || 0));

  // Show delete button only when editing
  var delBtn = el('btn-zone-delete');
  if (zoneEditing) {
    delBtn.classList.remove('hidden');
  } else {
    delBtn.classList.add('hidden');
  }

  // Name field: editable only for new zones
  el('zone-name').readOnly = !!zoneEditing;
}

function hideZoneForm() {
  el('zone-form').classList.add('hidden');
  el('zone-presence').classList.add('hidden');
  el('zone-presence').innerHTML = '';
}

function zoneFormCancel() {
  zoneEditing = null;
  hideZoneForm();
  renderZoneOverlay();
}

function zoneSave() {
  var name = getCameraName();
  if (!name) return;

  var form = el('zone-form');
  var zoneName = el('zone-name').value.trim();
  if (!zoneName) {
    toast('Zone name is required', 'error');
    return;
  }

  var labelsRaw = el('zone-labels').value.trim();
  var labels = labelsRaw ? labelsRaw.split(',').map(function(s) { return s.trim(); }).filter(Boolean) : [];

  var payload = {
    name: zoneName,
    points: JSON.parse(form.dataset.points || '[]'),
    labels: labels,
    track_presence: el('zone-track-presence').checked,
    face_recognition: el('zone-face-recognition').checked,
    enabled: el('zone-enabled').checked
  };

  var url, method;
  if (zoneEditing) {
    url = '/api/cameras/' + encodeURIComponent(name) + '/zones/' + encodeURIComponent(zoneEditing);
    method = 'PUT';
  } else {
    url = '/api/cameras/' + encodeURIComponent(name) + '/zones';
    method = 'POST';
  }

  fetch(url, {
    method: method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  })
    .then(function(r) {
      if (!r.ok) return r.json().then(function(err) { throw new Error(err.error || 'Save failed'); });
      return r.json();
    })
    .then(function() {
      toast('Zone saved');
      zoneEditing = null;
      hideZoneForm();
      loadZones();
    })
    .catch(function(err) {
      toast(err.message, 'error');
    });
}

function zoneDelete() {
  if (!zoneEditing) return;
  var name = getCameraName();
  if (!name) return;

  if (!confirm('Delete zone "' + zoneEditing + '"?')) return;

  fetch('/api/cameras/' + encodeURIComponent(name) + '/zones/' + encodeURIComponent(zoneEditing), {
    method: 'DELETE'
  })
    .then(function(r) {
      if (!r.ok) return r.json().then(function(err) { throw new Error(err.error || 'Delete failed'); });
      return r.json();
    })
    .then(function() {
      toast('Zone deleted');
      zoneEditing = null;
      hideZoneForm();
      loadZones();
    })
    .catch(function(err) {
      toast(err.message, 'error');
    });
}

// Presence polling
function startPresencePolling() {
  if (zonePresenceTimer) clearInterval(zonePresenceTimer);

  var hasPresenceZones = zoneData.some(function(z) { return z.track_presence; });
  if (!hasPresenceZones) return;

  // Poll presence for the selected zone if it has track_presence
  zonePresenceTimer = setInterval(function() {
    if (zoneEditing) {
      var z = zoneData.find(function(zone) { return zone.name === zoneEditing; });
      if (z && z.track_presence) loadZonePresence(z);
    }
  }, 5000);
}

function loadZonePresence(z) {
  if (!z.track_presence) {
    el('zone-presence').classList.add('hidden');
    return;
  }

  var name = getCameraName();
  if (!name) return;

  fetch('/api/cameras/' + encodeURIComponent(name) + '/zones/' + encodeURIComponent(z.name) + '/presence')
    .then(function(r) { return r.json(); })
    .then(function(presence) {
      var container = el('zone-presence');
      container.classList.remove('hidden');

      if (!presence || presence.length === 0) {
        container.innerHTML = '<span style="color: var(--text-tertiary); font-size: var(--text-xs);">No presence data yet</span>';
        return;
      }

      container.innerHTML = presence.map(function(p) {
        var cls = p.present ? 'present' : 'absent';
        var status = p.present ? 'present' : 'absent';
        return '<span class="zone-presence-badge ' + cls + '">' +
          '<span class="zone-presence-dot"></span>' +
          p.label + ': ' + status +
          '</span>';
      }).join('');
    })
    .catch(function() {
      el('zone-presence').classList.add('hidden');
    });
}

function startDiscovery() {
  toast('Camera discovery coming soon');
}

function showAddManual() {
  toast('Manual camera add coming soon');
}

// PTZ Controls
function initPTZ(cameraName) {
  fetch('/api/cameras')
    .then(function(r) { return r.json(); })
    .then(function(cameras) {
      var cam = cameras.find(function(c) { return c.name === cameraName; });
      if (cam && cam.ptz) {
        var ptzEl = el('ptz-controls');
        if (ptzEl) ptzEl.classList.remove('hidden');
        bindPTZControls(cameraName);
      }
    })
    .catch(function() {});
}

function bindPTZControls(cameraName) {
  var ptzLastCmd = 0;
  var ptzActive = false;

  function ptzCommand(action, direction) {
    var now = Date.now();
    if (now - ptzLastCmd < 100) return;
    ptzLastCmd = now;
    var body = direction ? {action: action, direction: direction} : {action: action};
    fetch('/api/cameras/' + encodeURIComponent(cameraName) + '/ptz', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body)
    }).catch(function() {});
  }

  var buttons = document.querySelectorAll('#ptz-controls .ptz-btn');
  buttons.forEach(function(btn) {
    var ptzAction = btn.getAttribute('data-ptz');

    btn.addEventListener('pointerdown', function(e) {
      e.preventDefault();
      btn.setPointerCapture(e.pointerId);
      ptzActive = true;
      if (ptzAction === 'stop') {
        ptzCommand('stop');
      } else if (ptzAction === 'zoom_in') {
        ptzCommand('zoom', 'in');
      } else if (ptzAction === 'zoom_out') {
        ptzCommand('zoom', 'out');
      } else {
        ptzCommand('move', ptzAction);
      }
    });

    btn.addEventListener('pointerup', function() {
      if (ptzActive && ptzAction !== 'stop') {
        ptzCommand('stop');
      }
      ptzActive = false;
    });

    btn.addEventListener('pointerleave', function() {
      if (ptzActive && ptzAction !== 'stop') {
        ptzCommand('stop');
      }
      ptzActive = false;
    });
  });

  var ptzKeyActive = {};
  document.addEventListener('keydown', function(e) {
    if (e.repeat) return;
    if (!el('ptz-controls') || el('ptz-controls').classList.contains('hidden')) return;
    var tag = (e.target || {}).tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

    var handled = true;
    switch (e.key) {
      case 'ArrowUp':    ptzCommand('move', 'up'); break;
      case 'ArrowDown':  ptzCommand('move', 'down'); break;
      case 'ArrowLeft':  ptzCommand('move', 'left'); break;
      case 'ArrowRight': ptzCommand('move', 'right'); break;
      case '+': case '=': ptzCommand('zoom', 'in'); break;
      case '-':           ptzCommand('zoom', 'out'); break;
      default: handled = false;
    }
    if (handled) {
      e.preventDefault();
      ptzKeyActive[e.key] = true;
    }
  });

  document.addEventListener('keyup', function(e) {
    if (ptzKeyActive[e.key]) {
      delete ptzKeyActive[e.key];
      ptzCommand('stop');
    }
  });
}
