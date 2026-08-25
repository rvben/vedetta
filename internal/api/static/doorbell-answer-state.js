(function (root, factory) {
  const model = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = model;
  root.DoorbellAnswerState = model;
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  'use strict';

  function controls(state, talkbackSupported) {
    const live = state === 'live';
    return { listenEnabled: live, talkEnabled: live && Boolean(talkbackSupported) };
  }

  function releasedConnection(state) {
    if (state === 'error') return { label: 'Live view unavailable', tone: 'error' };
    if (state === 'live') return { label: 'Connected', tone: 'ready' };
    return { label: 'Connecting', tone: 'pending' };
  }

  function endedCopy(acknowledged) {
    return acknowledged
      ? 'The ring is marked answered. Its saved evidence remains in Vedetta.'
      : 'The live session ended, but Vedetta could not mark the ring answered. Review the ring before dismissing it.';
  }

  function manualEndedCopy() {
    return 'The live session is closed. No doorbell ring or activity was created.';
  }

  function session(eventID) {
    const ring = Boolean(eventID);
    return {
      ring,
      title: ring ? 'Someone’s at the door' : 'Doorstep view',
      time: ring ? 'just now' : 'opened now',
      endedAction: ring ? 'Review doorbell activity' : 'Back to cameras',
      endedHref: ring ? '/doorbell.html' : '/',
    };
  }

  function shouldAcknowledge(camera, eventID) {
    return Boolean(camera && eventID);
  }

  function defaultListening() {
    return true;
  }

  function isVideoStalled(lastFrameAt, now, timeout, hidden = false) {
    if (hidden || !Number.isFinite(lastFrameAt) || !Number.isFinite(now) || !Number.isFinite(timeout)) return false;
    if (timeout <= 0 || now < lastFrameAt) return false;
    return now - lastFrameAt >= timeout;
  }

  function nextVideoStallAction(transport) {
    return transport === 'webrtc' ? 'hls' : 'error';
  }

  function listenPresentation(active, needsGesture, paused) {
    if (paused) return { label: 'Paused', audio: ' · audio paused while talking' };
    if (active) return { label: 'Listening', audio: ' · camera audio on' };
    if (needsGesture) return { label: 'Tap to hear', audio: ' · tap to enable camera audio' };
    return { label: 'Listen', audio: ' · camera audio off' };
  }

  function talkAudioTransition(listening, preferred = listening, needsGesture = false) {
    const pause = Boolean(listening);
    return { pause, restoreOnRelease: pause || Boolean(preferred && !needsGesture) };
  }

  function talkCopy(state, supported, codec, listening = true) {
    if (state === 'error') return { label: 'Talk unavailable', hint: 'Live view is unavailable' };
    if (!supported) return { label: 'Talk unavailable', hint: 'Camera has no audio return channel' };
    if (state !== 'live') return { label: 'Waiting for live view', hint: 'Microphone stays off' };
    const release = listening ? 'Release to listen' : 'Release when finished';
    return { label: 'Hold to talk', hint: codec ? `${release} · ${codec}` : release };
  }

  return {
    controls,
    defaultListening,
    endedCopy,
    isVideoStalled,
    listenPresentation,
    manualEndedCopy,
    nextVideoStallAction,
    releasedConnection,
    session,
    shouldAcknowledge,
    talkAudioTransition,
    talkCopy,
  };
});
