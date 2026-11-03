# Camera audio replay benchmark

This is the first development milestone for baby-cry recognition. It provides
bounded camera-audio ingestion and a reproducible local comparison of reference
models. It also supplies provisioning and parity tools for the opt-in Go recognition
runtime. Normal configuration enables neither recognition nor alerts, and the
runtime has no Python dependency.

## Audio ingestion

`internal/audio.Consumer` implements the existing RTSP consumer interface. Attach
it to the existing source after discovering its audio track; do not open another
camera connection. It accepts mono PCMA and PCMU at 8 kHz. Other tracks fail
explicitly. The verified Tapo streams use PCMA at 8 kHz.

RTP callbacks only copy bounded payloads into a nonblocking queue. A separate
worker decodes owned PCM16 windows (default 2 s context, 480 ms hop). Input and
output queues have explicit upper bounds. Drops, disconnects, SSRC changes,
sequence gaps and timestamp jumps reset partial windows. Missing audio is never
replaced with silence. Duplicate/late packets are ignored; sequence and timestamp
wraparound are supported. Completed stale windows are cleared at a reset.

`Window.Epoch` identifies continuity, `RTPStart` identifies the sample position,
and `ReceivedAt` lets a future inference owner reject stale work. `Stats()` exposes
rejections, discontinuities and queue drops. The owner must revalidate/rebuild the
consumer when reconnecting if the codec or payload type changes, detach it before
closing, and enforce its own inference deadline. A packet arriving concurrently
with disconnect can still have completed before the disconnect was observed;
the future owner must invalidate in-flight decisions on source disconnect too.

Tests cover G.711 known values, payload ownership, RTP wrap, duplicates, loss,
disconnects, stalled inference and concurrent close. A real source/recording test
requires all 100 video samples and byte-identical MP4 output with and without
stalled audio inference. The segment writer now records mono G.711 as 48 kHz
AAC-LC, including encoder priming removal and tail draining. Native audio tests
also decode both G.711 variants and verify playback payload preservation.

## Reproduce the model comparison

Use Python 3.12 and FFmpeg. Keep model weights, audio and reports outside the
repository. Downloading assets is explicit; normal tests never access the network.

```sh
python3.12 -m venv /tmp/vedetta-audio-venv
/tmp/vedetta-audio-venv/bin/pip install -r tools/audiobench/requirements.txt
python3.12 tools/audiobench/prepare.py /tmp/vedetta-audio-assets --models --smoke
```

The optional smoke corpus is ESC-50 fold 5: eight baby-cry clips and forty hard
negatives (laughter, coughing, breathing, snoring and cats). ESC-50 is CC BY-NC
3.0; the downloader saves its full individual-source attribution/license file.
Use it only for noncommercial evaluation and do not redistribute these assets.
Its labels are clip-level; they are not reviewed cry onset/offset annotations.
It is not held-out nursery data and overlap with pretraining is not established.

The smoke comparison defaults to 5 s context (one score per full clip). Smoke
classification uses a score crossing, not the live two-hit episode rule. Run
models sequentially with other heavy workloads stopped for useful timings:

```sh
/tmp/vedetta-audio-venv/bin/python tools/audiobench/replay.py \
  /tmp/vedetta-audio-assets/smoke.json --smoke --model yamnet \
  --yamnet /tmp/vedetta-audio-assets/yamnet \
  --output /tmp/yamnet-smoke.json

/tmp/vedetta-audio-venv/bin/python tools/audiobench/replay.py \
  /tmp/vedetta-audio-assets/smoke.json --smoke --model efficientat \
  --efficientat-source /tmp/vedetta-audio-assets/source/EfficientAT-a425fdce92572e602a1d5634799bd9f1f2efa806 \
  --checkpoint /tmp/vedetta-audio-assets/mn10.pt \
  --output /tmp/efficientat-smoke.json
```

Add `--export-onnx /tmp/mn10.onnx` to the EfficientAT command to export its fixed
selected-context **model core** and compare every logit against PyTorch on every window.
Maximum absolute logit error above `1e-4` fails the run. Preprocessing remains
upstream PyTorch; this is not proof of a Go frontend or Go ONNX runtime's parity.
Parity timing includes both reference and converted inference and must not be
used as a production performance estimate. YAMNet uses the official TensorFlow
SavedModel; no YAMNet ONNX conversion is claimed here.

The downloader pins EfficientAT source, weights and YAMNet's archive using
SHA-256. EfficientAT source is MIT licensed; preserve upstream licenses when
distributing models or ported code. YAMNet's official model and class maps retain
their respective upstream licensing. Source links:

- [Official YAMNet](https://github.com/tensorflow/models/tree/master/research/audioset/yamnet)
- [Official EfficientAT](https://github.com/fschmid56/EfficientAT)
- [ESC-50](https://github.com/karolpiczak/ESC-50)

Both models receive identical mono 8 kHz PCM after an FFmpeg PCMA round trip
(`--codec pcm` skips companding; `pcmu` tests mu-law). Resampling uses polyphase
antialiasing. YAMNet resamples to 16 kHz, averages its native frame scores over
the common context, and retains the reference model's internal padding. EfficientAT
resamples to 32 kHz and uses its unmodified evaluation mel frontend and sigmoid
logits. The infant-cry class is looked up by AudioSet MID `/t/dd00002`, never by a
shared numeric class index. Upsampling cannot recover frequencies absent from
the camera. Scores are not calibrated probabilities. Neither RMS nor voice
activity detection removes quiet windows.

Use `--window` and `--hop` to investigate other contexts. Do not assume
EfficientAT retains accuracy at short streaming contexts merely because its
architecture accepts the input shape. Its training clips are longer than the
default live PCM window. Validate context length alongside model and threshold.

Reports include score trajectories, preprocessing/inference p50/p95, process
peak RSS, thresholds and optional ONNX parity error. Run each model in a fresh
process. RSS includes its Python/framework overhead and ONNX export when enabled;
it does not predict the eventual embedded Go runtime's memory footprint. Loading,
warmup, WAV reading and FFmpeg companding are excluded from inference timing.
Audio context and two-hit confirmation add decision delay independently of CPU
time. Push delivery latency is not measured by an offline benchmark.

## Held-out nursery evaluation

Use local PCM16 WAVs and a JSON manifest, with timestamps in seconds:

```json
[
  {
    "path": "nursery-session.wav",
    "duration": 28800,
    "reviewed": true,
    "cry_intervals": [[123.4, 140.1], [678.0, 699.2]],
    "split": "test",
    "recording_group": "recording-01",
    "subject_group": "infant-01",
    "session_group": "night-01"
  }
]
```

These are example local pseudonyms, not actual nursery recordings. Only mark a
recording `reviewed` after its entire audio has been annotated, including negative
intervals. No cries is `cry_intervals: []`. Preserve uninterrupted sessions, keep
the WAV duration accurate, and use the same group identifiers for all chunks of
the same recording/infant/session. The validator rejects any group present in
multiple splits and overlapping/out-of-range annotations. Do not split overlapping
windows randomly; use distinct infants when measuring cross-infant generalization.
For evaluating a personalized monitor with just one infant, explicitly pass
`--within-subject`. This allows the same infant across splits while still rejecting
shared recordings/sessions. The report labels this as within-subject evaluation;
it cannot support claims about generalization to other infants.

Use the same CLI without `--smoke`. `--split validation` produces a threshold
sweep; choose the model and threshold there, then run `--split test --threshold X`
once on the held-out split. The test run reports only that locked threshold.
All split groupings are validated before filtering the selected split.

Episode onset requires two consecutive scores over threshold, and release requires
three below 70% of the onset threshold. A timestamp gap resets the decision state. Onset timestamps
are the actual decision time, never retrospectively backdated. Reports count missed
cry episodes, false episode onsets per eight hours of reviewed negative audio,
and onset-to-decision median. A false episode beginning before a labeled cry is
not credited as detecting that later cry. Short cries may be missed because of
context/confirmation; this tradeoff must be evaluated explicitly on real audio.

## Remaining product work

Before choosing the production default: collect reviewed local nursery nights with
quiet cries, speech/TV, white noise, pets, coughs, music, distance changes and
dropouts. Evaluate C200/C220 independently and inspect misses rather than selecting
from the smoke corpus. Measure on the deployment M4 while recording all cameras.

New Tapo recordings preserve camera audio when libfdk-aac is installed. Historical
MP4s created before this fix have no recoverable audio. Verify a new recording's
audio before collecting the benchmark corpus; use explicit local audio captures
for historical sessions.

The Go YAMNet backend now ports the reference frontend and checks all 521
outputs against the official model. Camera monitoring has independent continuity,
reconnection, overload and freshness handling. Cry events stay outside video
Activities and object counts, so configured onset notifications do not wait for
video activity to finish. Two consecutive high scores confirm onset; three
scores below 70% of the onset threshold release it. Notifications have a default
60-second cooldown; recognition and episode evidence continue during cooldown. Replay episode reports use the same hysteresis and cooldown
(`--cooldown` overrides the latter). Observation mode and snooze still save events
but do not notify.

### Provision the local model

Use the downloaded official SavedModel from `prepare.py`; keep generated weights,
fixtures and reports outside the repository. Conversion preserves all classes,
fuses inference batch normalization and verifies ONNX scores against TensorFlow.
The exported directory includes the upstream Apache-2.0 license and notice;
retain both when copying the bundle.

```sh
/tmp/vedetta-audio-venv/bin/python tools/audiobench/export_yamnet.py \
  /tmp/vedetta-audio-assets/yamnet /tmp/vedetta-cry-model
/tmp/vedetta-audio-venv/bin/python tools/audiobench/yamnet_fixtures.py \
  /tmp/vedetta-audio-assets/yamnet /tmp/vedetta-audio-assets/smoke.json \
  /tmp/vedetta-cry-fixtures
VEDETTA_CRY_MODEL=/tmp/vedetta-cry-model \
VEDETTA_CRY_FIXTURES=/tmp/vedetta-cry-fixtures make test-cry-model
```

Parity covers silence, noise and 48 camera-codec smoke clips: every class of
every patch must differ by at most `1e-4`; log-mel values allow `3e-3` for
float32 FFT differences near the noise floor. This verifies conversion and
preprocessing, not clinical accuracy or overnight alert reliability.

Copy the complete model directory to the appliance, then pin the
`manifest_sha256` printed by the exporter in config:

```yaml
cry:
  model_path: models/yamnet/manifest.json
  model_sha256: "<exporter's manifest_sha256>"
  threshold: 0.2
  cooldown_seconds: 60
cameras:
  - name: nursery
    url: "<existing RTSP URL>"
    cry:
      enabled: true
      alerts: false
```

Add the cry block to the existing camera entry; keep its other settings. Relative
model paths resolve beside config.yml. Restart after configuration changes.
Only continuous mono 8 kHz PCMA/PCMU tracks are supported; battery/on-demand
cameras are rejected. Missing or corrupt assets expose an unavailable monitor
while recording continues. `/api/health` includes `checks.cry_recognition`; the
camera list/detail also expose recognition state, score, last analyzed time,
episode count, dropped windows and snooze expiry. Scores are not probabilities.

### Enable and check alerts

Review complete nursery sessions, including quiet cries and negative audio,
before changing `alerts` to `true`. Use `--window 2 --hop .48` for the live model
context in replay evaluation. Validation chooses threshold/cooldown; keep the
test split untouched until those choices are locked. MP4-extracted audio already
passed through camera encoding and recording AAC: use `--codec pcm` when
evaluating it and label that additional AAC stage in your report. Historical
silent recordings cannot be used.

Web push honors the account's mute and per-camera **Crying** preference. Pushover
honors its configured camera/event filters; add `cry` to an explicit `events`
list. If omitted, the provider defaults to doorbell, activity and cry events.
Pushover cry alerts include a bounded JPEG of a frame decoded within the last
ten seconds and a **Watch live** link to that camera, using the same credentials,
sound, priority and device as doorbell notifications. Inline photo uploads start
before the full-resolution snapshot is written; successful providers are never
resent during queue retries. If no fresh frame is available, the cry alert still
arrives without a photo instead of showing an old cached picture. Set Pushover's
`base_url` to the externally reachable Vedetta URL for the live action.
Camera recognition remains independently opt-in. Both providers receive onset
notifications linking to the live camera, where **Listen live** enables sound
through a deliberate tap (mobile autoplay restrictions still apply). Recent
episode links open recorded evidence as it becomes available. Event clips
retain the existing retention, pre/post-capture and maximum-duration limits.

Camera snooze suppresses all cry notifications, preserves candidate evidence
and survives restart. `POST /api/cameras/{name}/cry/snooze` accepts
`{"minutes":30}` (0 resumes; maximum 1440) and requires an authenticated admin
scope. Resuming does not resend an already-running episode. Mobile-profile
browser tests verify the UI; physical phones must still verify actual push
delivery, permissions, sound, lock-screen behavior and tap-through latency.

Before enabling bedroom alerts on the appliance, measure the Go runtime on its
M4 while all cameras record. Inference is shared and serialized; busy work, old
results and stream discontinuities cannot confirm a cry. Overload becomes a
visible degraded state instead of accumulating work or stalling recording.

Run dependency-free evaluation checks with `make test-audio-bench`; they are also
part of `make check` and CI. Model smoke/parity runs remain explicit local checks.

## Evaluate your nursery with the production scorer

Use fully reviewed local recordings containing cries, speech, music, ordinary
room noise and silence. Label all cry intervals, including missed cries, and
keep sessions separate between validation and test. Do not use detections as
labels. Keep audio, annotations and reports outside this repository.

```sh
go build -o /tmp/vedetta-cryreplay ./cmd/cryreplay
python3 tools/audiobench/replay.py /local/reviewed-nursery.json \
  --model vedetta --vedetta-replay /tmp/vedetta-cryreplay \
  --vedetta-manifest /local/cry-model/manifest.json \
  --vedetta-sha256 MANIFEST_SHA256 --codec pcma --within-subject \
  --split validation --output /local/validation-report.json
```

This uses Vedetta's actual Go frontend and inference runtime at the live
2-second context and 480 ms hop, with a camera-codec round trip. Choose a
threshold on validation sessions, then evaluate untouched test sessions with
`--split test --threshold VALUE`. Report missed episodes, onset-to-alert delay
and false alerts per eight hours of reviewed negative audio. A small dataset
or a single infant does not establish general recognition accuracy. Nothing
is uploaded, no live camera is opened and no notifications are sent.

To start annotations from future local recordings:

```sh
ffmpeg -i /local/recording.mp4 -vn -ac 1 -ar 8000 -c:a pcm_s16le /local/review.wav
python3 tools/audiobench/review_nursery.py /local/review.wav --output /local/review.json
```

Listen through each entire file, annotate `cry_intervals` in seconds, assign
session groups and held-out splits, and only then set `reviewed` to true. The
template deliberately cannot pass evaluation until reviewed. An MP4 without
an audio track cannot be used; do not substitute silence for missing audio.
