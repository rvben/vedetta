(() => {
  'use strict';

  const params = new URLSearchParams(location.search);
  const camera = params.get('camera') || '';
  const eventID = params.get('event') || '';
  const session = window.DoorbellAnswerState.session(eventID);
  const root = document.getElementById('doorstep');
  const video = document.getElementById('doorstep-video');
  const snapshot = document.getElementById('doorstep-snapshot');
  const listen = document.getElementById('doorstep-listen');
  const listenLabel = document.getElementById('doorstep-listen-label');
  const talk = document.getElementById('doorstep-talk');
  const connection = document.getElementById('doorstep-connection');
  const connectionState = document.getElementById('doorstep-connection-state');
  const audioState = document.getElementById('doorstep-audio-state');
  const errorPanel = document.getElementById('doorstep-error');
  const connectingCopy = document.getElementById('doorstep-connecting-copy');
  const VIDEO_STALL_TIMEOUT_MS = 8000;
  const VIDEO_WATCH_INTERVAL_MS = 1000;
  let livePeer = null;
  let talkPeer = null;
  let microphone = null;
  let talking = false;
  let ended = false;
  let liveState = 'connecting';
  let talkbackSupported = false;
  let talkbackCodec = '';
  let acknowledged = false;
  let listening = false;
  let listenPreference = true;
  let listenNeedsGesture = false;
  let listenPausedForTalk = false;
  let restoreListeningOnRelease = false;
  let listenAttempt = 0;
  let mediaAttempt = 0;
  let liveTransport = '';
  let videoWatchTimer = null;
  let videoFrameCallback = null;
  let lastVideoFrameAt = 0;
  let lastVideoMarker = '';

  const stateModel = window.DoorbellAnswerState;

  const displayName = (value) => value.split(/[_-]+/).filter(Boolean).map((part) => part[0].toUpperCase() + part.slice(1)).join(' ');
  const apiPath = (suffix) => `/api/cameras/${encodeURIComponent(camera)}${suffix}`;

  function csrfToken() {
    const match = document.cookie.split(';').map((part) => part.trim()).find((part) => part.startsWith('vedetta_csrf='));
    return match ? decodeURIComponent(match.slice('vedetta_csrf='.length)) : '';
  }

  function postJSON(url, body) {
    const headers = { 'Content-Type': 'application/json' };
    const csrf = csrfToken();
    if (csrf) headers['X-CSRF-Token'] = csrf;
    return fetch(url, { method: 'POST', headers, body: body === undefined ? undefined : JSON.stringify(body) });
  }

  function setConnection(label, tone = 'pending') {
    connectionState.textContent = label;
    connection.dataset.tone = tone;
  }

  function setTalkCopy(label, hint) {
    talk.querySelector('.doorstep-talk-label').textContent = label;
    talk.querySelector('.doorstep-talk-hint').textContent = hint;
  }

  function readVideoMarker() {
    if (typeof video.getVideoPlaybackQuality === 'function') {
      const quality = video.getVideoPlaybackQuality();
      if (quality && Number.isFinite(quality.totalVideoFrames)) return `frames:${quality.totalVideoFrames}`;
    }
    if (Number.isFinite(video.webkitDecodedFrameCount)) return `frames:${video.webkitDecodedFrameCount}`;
    return Number.isFinite(video.currentTime) ? `time:${video.currentTime.toFixed(3)}` : '';
  }

  function stopVideoWatch() {
    if (videoWatchTimer) clearInterval(videoWatchTimer);
    videoWatchTimer = null;
    if (videoFrameCallback !== null && typeof video.cancelVideoFrameCallback === 'function') {
      video.cancelVideoFrameCallback(videoFrameCallback);
    }
    videoFrameCallback = null;
  }

  function showRecovering() {
    closeTalkbackSession();
    liveState = 'recovering';
    root.dataset.state = 'recovering';
    document.getElementById('doorstep-live-label').textContent = 'RECONNECTING';
    connectingCopy.textContent = 'Video stopped updating. Reconnecting…';
    setConnection('Video stalled · reconnecting', 'warning');
    syncAudioControls();
  }

  function handleVideoStall(attempt, transport) {
    if (attempt !== mediaAttempt || liveState !== 'live' || ended) return;
    stopVideoWatch();
    showRecovering();
    if (stateModel.nextVideoStallAction(transport) === 'hls') {
      startHLSFallback(attempt);
      return;
    }
    showError(session.ring
      ? 'Live video stopped updating after Vedetta tried both streaming paths. You can still review the saved ring image.'
      : 'Live video stopped updating after Vedetta tried both streaming paths. The latest camera snapshot remains available.');
  }

  function startVideoWatch(attempt, transport) {
    stopVideoWatch();
    liveTransport = transport;
    lastVideoFrameAt = performance.now();
    lastVideoMarker = readVideoMarker();

    if (typeof video.requestVideoFrameCallback === 'function') {
      const onFrame = (now) => {
        if (attempt !== mediaAttempt || ended || liveTransport !== transport) return;
        lastVideoFrameAt = now;
        videoFrameCallback = video.requestVideoFrameCallback(onFrame);
      };
      videoFrameCallback = video.requestVideoFrameCallback(onFrame);
    }

    videoWatchTimer = setInterval(() => {
      if (attempt !== mediaAttempt || ended || liveTransport !== transport) {
        stopVideoWatch();
        return;
      }
      if (document.hidden) {
        lastVideoFrameAt = performance.now();
        return;
      }
      if (typeof video.requestVideoFrameCallback !== 'function') {
        const marker = readVideoMarker();
        if (marker && marker !== lastVideoMarker) {
          lastVideoMarker = marker;
          lastVideoFrameAt = performance.now();
        }
      }
      if (liveState === 'live' && stateModel.isVideoStalled(
        lastVideoFrameAt,
        performance.now(),
        VIDEO_STALL_TIMEOUT_MS,
        document.hidden
      )) handleVideoStall(attempt, transport);
    }, VIDEO_WATCH_INTERVAL_MS);
  }

  function syncReadyTalkCopy() {
    if (!talkbackSupported || talking) return;
    const copy = stateModel.talkCopy(liveState, true, talkbackCodec, listening);
    setTalkCopy(copy.label, copy.hint);
  }

  function renderListening() {
    const active = listening && liveState === 'live' && !listenPausedForTalk;
    const presentation = stateModel.listenPresentation(active, listenNeedsGesture, listenPausedForTalk);
    const canListen = liveState === 'live' && !talking && !ended;
    listen.disabled = !canListen;
    listen.classList.toggle('is-active', active);
    listen.classList.toggle('needs-gesture', listenNeedsGesture && canListen);
    listen.setAttribute('aria-pressed', String(active));
    listen.querySelector('.speaker-on').hidden = !active;
    listen.querySelector('.speaker-off').hidden = active;
    listenLabel.textContent = presentation.label;
    audioState.textContent = presentation.audio;
    syncReadyTalkCopy();
  }

  function silenceIncomingAudio(clearPrompt = true) {
    listenAttempt++;
    video.muted = true;
    listening = false;
    if (clearPrompt) listenNeedsGesture = false;
    renderListening();
  }

  function enableListening(automatic) {
    if (liveState !== 'live' || ended || talking) return Promise.resolve(false);
    const attempt = ++listenAttempt;
    video.muted = false;
    let playback;
    try {
      playback = video.play();
    } catch (_) {
      playback = Promise.reject(new Error('audio playback blocked'));
    }
    return Promise.resolve(playback).then(() => {
      if (attempt !== listenAttempt || liveState !== 'live' || ended || talking) {
        video.muted = true;
        return false;
      }
      listening = true;
      listenNeedsGesture = false;
      renderListening();
      return true;
    }).catch(() => {
      if (attempt !== listenAttempt) return false;
      video.muted = true;
      listening = false;
      listenNeedsGesture = true;
      renderListening();
      return false;
    });
  }

  function pauseListeningForTalk() {
    const transition = stateModel.talkAudioTransition(listening, listenPreference, listenNeedsGesture);
    restoreListeningOnRelease = transition.restoreOnRelease;
    listenPausedForTalk = transition.pause;
    listenAttempt++;
    video.muted = true;
    listening = false;
    if (transition.pause) listenNeedsGesture = false;
    renderListening();
  }

  function resumeListeningAfterTalk() {
    const restore = restoreListeningOnRelease && listenPreference && liveState === 'live' && !ended;
    restoreListeningOnRelease = false;
    listenPausedForTalk = false;
    renderListening();
    if (restore) enableListening(false);
  }

  function syncAudioControls() {
    const controls = stateModel.controls(liveState, talkbackSupported);
    talk.disabled = !controls.talkEnabled;
    if (!controls.listenEnabled) {
      silenceIncomingAudio();
      listenPausedForTalk = false;
      restoreListeningOnRelease = false;
    }
    renderListening();
  }

  function closeTalkbackSession() {
    talking = false;
    listenPausedForTalk = false;
    restoreListeningOnRelease = false;
    if (microphone) microphone.getAudioTracks().forEach((track) => { track.enabled = false; });
    if (talkPeer) talkPeer.close();
    if (microphone) microphone.getTracks().forEach((track) => track.stop());
    talkPeer = null;
    microphone = null;
    talk.classList.remove('is-talking');
    talk.setAttribute('aria-pressed', 'false');
  }

  function showAcknowledgementFailure() {
    const chip = document.getElementById('doorstep-answered');
    chip.classList.add('is-warning');
	chip.querySelector('.doorstep-answer-success').hidden = true;
	chip.querySelector('.doorstep-answer-warning').hidden = false;
    document.getElementById('doorstep-answered-copy').textContent = 'Answer not saved';
    chip.hidden = false;
  }

  function showError(message) {
	closeTalkbackSession();
	stopVideoWatch();
	liveState = 'error';
    liveTransport = 'error';
    video.pause();
    errorPanel.hidden = false;
    document.getElementById('doorstep-error-copy').textContent = message;
    root.dataset.state = 'error';
    document.getElementById('doorstep-live-label').textContent = 'SNAPSHOT';
    setConnection('Live view unavailable', 'error');
	setTalkCopy('Talk unavailable', 'Live view is unavailable');
	syncAudioControls();
  }

  async function acknowledge() {
    if (!stateModel.shouldAcknowledge(camera, eventID)) return;
    try {
      const response = await postJSON(apiPath(`/doorbell/${encodeURIComponent(eventID)}/answer`));
      if (!response.ok) {
		showAcknowledgementFailure();
		return;
	  }
      const answer = await response.json();
	  acknowledged = true;
      const chip = document.getElementById('doorstep-answered');
      const copy = document.getElementById('doorstep-answered-copy');
	  chip.classList.remove('is-warning');
	  chip.querySelector('.doorstep-answer-success').hidden = false;
	  chip.querySelector('.doorstep-answer-warning').hidden = true;
      copy.textContent = answer.answered_by ? `Answered by ${answer.answered_by}` : 'Answered';
      chip.hidden = false;
    } catch (_) {
	  showAcknowledgementFailure();
    }
  }

  async function iceServers() {
	try {
	  const response = await fetch('/api/streaming/ice-servers');
	  if (!response.ok) return [];
	  const body = await response.json();
	  return body.ice_servers || [];
	} catch (_) {
	  return [];
	}
  }

  async function startLive() {
    if (!camera) {
      showError('Choose a doorbell camera from Cameras to open this view.');
      return;
    }
	const attempt = ++mediaAttempt;
	stopVideoWatch();
    liveTransport = 'connecting';
    errorPanel.hidden = true;
	liveState = 'connecting';
    root.dataset.state = 'connecting';
    document.getElementById('doorstep-live-label').textContent = 'CONNECTING';
    connectingCopy.textContent = 'Opening secure live view…';
    setConnection('Connecting');
	syncAudioControls();
    if (livePeer) livePeer.close();
    video.pause();
    video.srcObject = null;
    video.removeAttribute('src');
    video.load();
    try {
      const peer = new RTCPeerConnection({ iceServers: await iceServers() });
      if (attempt !== mediaAttempt || ended) { peer.close(); return; }
      livePeer = peer;
      peer.addTransceiver('video', { direction: 'recvonly' });
      peer.addTransceiver('audio', { direction: 'recvonly' });
      peer.ontrack = (event) => {
        if (attempt !== mediaAttempt || livePeer !== peer) return;
        video.srcObject = event.streams[0] || new MediaStream([event.track]);
        const playback = video.play();
        if (playback && typeof playback.catch === 'function') playback.catch(() => {});
      };
      peer.oniceconnectionstatechange = () => {
        if (attempt === mediaAttempt && livePeer === peer && ['failed', 'disconnected'].includes(peer.iceConnectionState)) {
          if (liveState === 'live') showRecovering();
          startHLSFallback(attempt);
        }
      };
      const offer = await peer.createOffer();
      await peer.setLocalDescription(offer);
      await waitForICE(peer);
      if (attempt !== mediaAttempt || ended) { peer.close(); return; }
      const response = await postJSON(apiPath('/webrtc/offer?quality=low'), peer.localDescription);
      if (!response.ok) throw new Error('WebRTC negotiation failed');
      await peer.setRemoteDescription(await response.json());
      if (attempt !== mediaAttempt || ended || livePeer !== peer) { peer.close(); return; }
      liveTransport = 'webrtc-starting';
      await video.play();
      await waitForVideo();
      markLive(attempt, 'webrtc');
    } catch (_) {
      if (attempt === mediaAttempt && !ended) startHLSFallback(attempt);
    }
  }

  function waitForICE(peer) {
    if (peer.iceGatheringState === 'complete') return Promise.resolve();
    return new Promise((resolve) => {
      const timeout = setTimeout(resolve, 2500);
      peer.addEventListener('icegatheringstatechange', () => {
        if (peer.iceGatheringState === 'complete') { clearTimeout(timeout); resolve(); }
      });
    });
  }

  function waitForVideo() {
    if (video.readyState >= 2) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('live view timed out')), 7000);
      video.addEventListener('loadeddata', () => { clearTimeout(timeout); resolve(); }, { once: true });
    });
  }

  async function startHLSFallback(attempt = mediaAttempt) {
    if (ended || attempt !== mediaAttempt) return;
    if (liveTransport === 'hls-starting') return;
    stopVideoWatch();
    liveTransport = 'hls-starting';
    const peer = livePeer;
    livePeer = null;
    if (peer) {
      peer.oniceconnectionstatechange = null;
      peer.close();
    }
    video.pause();
    video.srcObject = null;
    video.removeAttribute('src');
    video.load();
    video.src = apiPath('/live.m3u8?quality=low');
    try {
      await video.play();
      await waitForVideo();
      markLive(attempt, 'hls');
    } catch (_) {
      if (attempt !== mediaAttempt || ended) return;
      showError(session.ring
        ? 'The camera did not provide a live stream. You can still review the saved ring image.'
        : 'The camera did not provide a live stream. The latest camera snapshot remains available.');
    }
  }

  function markLive(attempt, transport) {
    if (ended || attempt !== mediaAttempt) return;
    if (transport === 'webrtc' && liveTransport !== 'webrtc-starting') return;
    if (transport === 'hls' && liveTransport !== 'hls-starting') return;
	liveState = 'live';
    root.dataset.state = 'live';
    document.getElementById('doorstep-live-label').textContent = 'LIVE';
	setConnection('Connected', 'ready');
	syncAudioControls();
    startVideoWatch(attempt, transport);
    if (listenPreference) enableListening(true);
  }

  async function loadTalkbackCapability() {
    try {
      const response = await fetch(apiPath('/talkback/capabilities'));
      const body = await response.json();
      if (!response.ok || !body.supported) {
		talkbackSupported = false;
        talkbackCodec = '';
        setTalkCopy('Talk unavailable', body.reason || 'Camera has no audio return channel');
		syncAudioControls();
        return;
	  }
	  talkbackSupported = true;
	  talkbackCodec = body.codec || '';
	  const copy = stateModel.talkCopy(liveState, true, talkbackCodec, listening);
	  setTalkCopy(copy.label, copy.hint);
	  syncAudioControls();
    } catch (_) {
	  talkbackSupported = false;
      setTalkCopy('Talk unavailable', 'Could not check camera audio');
	  syncAudioControls();
    }
  }

  async function ensureTalkPeer() {
    if (talkPeer && microphone) return;
    setTalkCopy('Connecting microphone…', 'Keep holding to speak');
    microphone = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true }, video: false });
    talkPeer = new RTCPeerConnection({ iceServers: await iceServers() });
    const track = microphone.getAudioTracks()[0];
    track.enabled = false;
    talkPeer.addTrack(track, microphone);
    const offer = await talkPeer.createOffer();
    await talkPeer.setLocalDescription(offer);
    await waitForICE(talkPeer);
    const response = await postJSON(apiPath('/talkback/offer'), talkPeer.localDescription);
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      throw new Error(body.error || 'Talkback could not connect');
    }
    await talkPeer.setRemoteDescription(await response.json());
  }

  async function startTalking(event) {
    if (talk.disabled || talking || ended) return;
    talking = true;
    pauseListeningForTalk();
    if (event.type === 'pointerdown') talk.setPointerCapture(event.pointerId);
    try {
      await ensureTalkPeer();
      if (!talking || ended) return;
      microphone.getAudioTracks()[0].enabled = true;
      talk.classList.add('is-talking');
      talk.setAttribute('aria-pressed', 'true');
      setTalkCopy('Talking…', 'Release when finished');
      setConnection(`Talking to ${displayName(camera)}`, 'ready');
    } catch (error) {
      talking = false;
      if (talkPeer) talkPeer.close();
      if (microphone) microphone.getTracks().forEach((track) => track.stop());
	  talkPeer = null;
	  microphone = null;
	  talkbackSupported = false;
      talkbackCodec = '';
      setTalkCopy('Talk unavailable', error.message);
	  resumeListeningAfterTalk();
	  syncAudioControls();
	  const released = stateModel.releasedConnection(liveState);
	  setConnection(released.label, released.tone);
    }
  }

  function stopTalking() {
    talking = false;
    if (microphone) microphone.getAudioTracks().forEach((track) => { track.enabled = false; });
    talk.classList.remove('is-talking');
    talk.setAttribute('aria-pressed', 'false');
	resumeListeningAfterTalk();
	if (!ended) {
	  const released = stateModel.releasedConnection(liveState);
	  setConnection(released.label, released.tone);
	}
  }

  function stopAll() {
    ended = true;
    mediaAttempt++;
    stopVideoWatch();
    stopTalking();
    if (livePeer) livePeer.close();
    if (talkPeer) talkPeer.close();
    if (microphone) microphone.getTracks().forEach((track) => track.stop());
    video.pause();
    document.getElementById('doorstep-ended-copy').textContent = session.ring
      ? stateModel.endedCopy(acknowledged)
      : stateModel.manualEndedCopy();
    document.getElementById('doorstep-ended').hidden = false;
  }

  listen.addEventListener('click', () => {
    if (listening) {
      listenPreference = false;
      silenceIncomingAudio();
      return;
    }
    listenPreference = true;
    enableListening(false);
  });
  talk.addEventListener('pointerdown', startTalking);
  talk.addEventListener('pointerup', stopTalking);
  talk.addEventListener('pointercancel', stopTalking);
  talk.addEventListener('lostpointercapture', stopTalking);
  talk.addEventListener('keydown', (event) => { if ((event.key === ' ' || event.key === 'Enter') && !event.repeat) startTalking(event); });
  talk.addEventListener('keyup', (event) => { if (event.key === ' ' || event.key === 'Enter') stopTalking(); });
	window.addEventListener('blur', stopTalking);
	document.addEventListener('visibilitychange', () => {
	  if (document.hidden) {
		stopTalking();
		return;
	  }
	  if (liveState === 'live') lastVideoFrameAt = performance.now();
	});
  document.getElementById('doorstep-end').addEventListener('click', stopAll);
  document.getElementById('doorstep-retry').addEventListener('click', startLive);
  video.addEventListener('error', () => {
    if (liveState === 'live') handleVideoStall(mediaAttempt, liveTransport);
  });
  window.addEventListener('pagehide', () => { if (!ended) stopAll(); });

  const name = displayName(camera || 'front_door');
  listenPreference = stateModel.defaultListening();
  document.title = `${session.ring ? 'Answer' : 'Open'} ${name} - Vedetta`;
  document.getElementById('doorstep-camera-name').textContent = name;
  document.getElementById('doorstep-camera-meta').textContent = name;
  document.getElementById('doorstep-title').textContent = session.title;
  document.getElementById('doorstep-time').textContent = session.time;
  document.getElementById('doorstep-manual').hidden = session.ring;
  const endedAction = document.getElementById('doorstep-ended-action');
  endedAction.textContent = session.endedAction;
  endedAction.href = session.endedHref;
  snapshot.addEventListener('error', () => {
    if (snapshot.dataset.fallback !== 'camera' && camera) {
      snapshot.dataset.fallback = 'camera';
      snapshot.src = apiPath(`/snapshot?t=${Date.now()}`);
      return;
    }
    if (snapshot.dataset.fallback !== 'icon') {
      snapshot.dataset.fallback = 'icon';
      snapshot.src = '/icon-512.png';
    }
  });
  if (eventID) {
    snapshot.src = `/api/events/${encodeURIComponent(eventID)}/snapshot`;
  } else if (camera) {
    snapshot.dataset.fallback = 'camera';
    snapshot.src = apiPath(`/snapshot?t=${Date.now()}`);
  }
  acknowledge();
  startLive();
  loadTalkbackCapability();
})();
