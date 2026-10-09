# Camera audio replay benchmark

This is the first development milestone for baby-cry recognition. It provides
bounded camera-audio ingestion and a reproducible local comparison of reference
models. It does not enable production inference, create alerts, or introduce a
Python runtime dependency into Vedetta.

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
three below threshold. A timestamp gap resets the decision state. Onset timestamps
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

Then port and verify the selected model's preprocessing, add a typed CPU audio
inference backend with bounded deadlines, and integrate an opt-in independent
audio lifecycle. Audio must run even without video motion. Persist a distinct
cry event kind and notify at onset; do not count it as a detected object or let
video activity quiet periods delay the alert. Add cooldown/mute behavior,
visible audio availability, replayable evidence, and actual iPhone/Android push
delivery tests. None of these live product behaviors is enabled by this milestone.

Run dependency-free evaluation checks with `make test-audio-bench`; they are also
part of `make check` and CI. Model smoke/parity runs remain explicit local checks.
