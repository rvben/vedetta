package media

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// diskPauseRetryInterval is how often a paused consumer retries the disk check.
const diskPauseRetryInterval = 30 * time.Second

// dropLogInterval rate-limits the warning about dropped packets. A volume slow
// enough to overrun the queue overruns it thousands of times a minute.
const dropLogInterval = time.Minute

// dropRotateInterval bounds how often a packet-drop gap may rotate the segment.
// The same slow volume that overruns the queue keeps overrunning it, so the
// discontinuity marker is raised again between consecutive packets; rotating on
// each one would create and immediately delete a segment (and its DB row) per
// packet, which costs far more than the hole it is trying to isolate. Beyond
// the first rotation the stream is understood to be degraded, and the drop
// counter is what reports it.
//
// The bound is on the rotation rate, not on one storm, so a second and
// unrelated gap inside the window is spliced into the open segment instead of
// being isolated. That is the accepted cost rather than an oversight: telling
// an unrelated gap from a continuing one means rotating on drops that arrive
// after a quiet run, which is precisely a rotation sooner than this interval,
// and the rate bound is the only thing stopping a degraded volume from
// producing a file and a DB row per packet. A splice costs artifacts until the
// next keyframe; unbounded rotation costs the recording.
const dropRotateInterval = 5 * time.Second

// segmentWriterCreateTimeout bounds segment-file creation. A stalled storage
// volume makes os.Create block forever in the kernel; without this bound the
// single processLoop goroutine wedges under rc.mu and recording never
// recovers, even after the volume comes back. On timeout the create is
// abandoned and surfaced as a write error so the existing pause/resume path
// takes over and recording self-heals once I/O succeeds again.
const segmentWriterCreateTimeout = 5 * time.Second

// SegmentInfo is passed to the OnSegmentDone callback when a segment is completed.
type SegmentInfo struct {
	Camera    string
	Path      string
	StartTime time.Time
	EndTime   time.Time
	SizeBytes int64
}

type rtpMsg struct {
	pkt   *rtp.Packet
	video bool
}

// RecordingConsumer implements rtsp.Consumer and writes RTP packets to fMP4 segments.
// Packets are buffered via a channel so the RTSP reader goroutine is never blocked.
type RecordingConsumer struct {
	camera     string
	segLen     time.Duration
	videoTrack *rtsp.TrackInfo
	audioTrack *rtsp.TrackInfo
	onSegment  func(SegmentInfo)
	segDir     string
	disk       *DiskSpace

	// onSegmentRemoved retracts the DB row written when a segment file was
	// created, for the paths that delete the file again.
	onSegmentRemoved func(path string)

	// pktCh is never closed. rtsp.Source fans out synchronously on its
	// connection goroutine, so a sender can already be past the closed check
	// when Close runs; closing the channel under it would crash the process
	// with "send on closed channel". processLoop is stopped via stop instead,
	// and late sends land harmlessly in the buffer.
	pktCh  chan rtpMsg
	stop   chan struct{}
	done   chan struct{}
	closed atomic.Bool

	// newWriter creates the segment writer; indirected so a stalled-volume
	// hang in file creation can be bounded and tested. Defaults to
	// NewSegmentWriter.
	newWriter func(path string, video, audio *rtsp.TrackInfo) (*SegmentWriter, error)
	// createTimeout bounds one newWriter call. Defaults to
	// segmentWriterCreateTimeout; lowered by tests.
	createTimeout time.Duration
	// createPending is true while a newWriter call is still outstanding after
	// its timeout. Only one may be in flight per consumer.
	createPending atomic.Bool

	// droppedPackets counts packets the nonblocking queue could not accept.
	droppedPackets atomic.Int64
	// discontinuous is set when a packet was dropped and cleared once the
	// segment has been closed at that discontinuity.
	discontinuous atomic.Bool
	// lastDropLog is the unix nano timestamp of the last drop warning.
	lastDropLog atomic.Int64
	// lastDropRotate is when a drop last rotated the segment. Guarded by mu.
	lastDropRotate time.Time

	mu              sync.Mutex
	writer          *SegmentWriter
	segPath         string
	currentPath     string // path of the segment currently being written; "" when closed
	segStart        time.Time
	lastSegBase     string // timestamp base of the last segment filename
	segDupCount     int    // disambiguates same-second rotations (gap rotation is instant)
	paused          bool
	pausedAtomic    atomic.Bool // lock-free read for external status checks
	pausedSince     time.Time
	lastDiskWarning time.Time
	writeErrors     int
}

// NewRecordingConsumer creates a consumer that records to rotating fMP4 segments.
// onSegment is called when each segment completes (for DB registration).
func NewRecordingConsumer(segDir, camera string, segLen time.Duration, video, audio *rtsp.TrackInfo, disk *DiskSpace, onSegment func(SegmentInfo)) *RecordingConsumer {
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		slog.Error("failed to create segment directory", "camera", camera, "error", err)
	}

	rc := &RecordingConsumer{
		camera:     camera,
		segLen:     segLen,
		videoTrack: video,
		audioTrack: audio,
		onSegment:  onSegment,
		segDir:     segDir,
		disk:       disk,
		pktCh:      make(chan rtpMsg, 512),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		newWriter:  NewSegmentWriter,

		createTimeout: segmentWriterCreateTimeout,
	}

	go rc.processLoop()

	return rc
}

// Paused returns true if recording is paused due to low disk space.
// Uses an atomic load to avoid blocking on the processing mutex.
func (rc *RecordingConsumer) Paused() bool {
	return rc.pausedAtomic.Load()
}

// SetSegmentRemovedHook registers a callback invoked with the path of a segment
// file the consumer deletes after having registered it. The segment row is
// written as soon as the file is created, so every removal path has to retract
// it or the DB keeps pointing at a file that no longer exists.
func (rc *RecordingConsumer) SetSegmentRemovedHook(fn func(path string)) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.onSegmentRemoved = fn
}

// DroppedPackets returns the number of RTP packets this consumer could not
// queue. The queue is deliberately nonblocking, so a slow volume drops packets
// rather than stalling the RTSP reader; a nonzero and growing count is the
// signal that recorded video has holes.
func (rc *RecordingConsumer) DroppedPackets() int64 {
	return rc.droppedPackets.Load()
}

// SegmentCreatePending reports whether a segment-file creation is still
// outstanding after having timed out, which means the storage volume is wedged
// in the kernel. Recording stays paused and skips further create attempts while
// this holds.
func (rc *RecordingConsumer) SegmentCreatePending() bool {
	return rc.createPending.Load()
}

// OnVideoRTP enqueues a video RTP packet for async processing.
func (rc *RecordingConsumer) OnVideoRTP(pkt *rtp.Packet) {
	if rc.closed.Load() {
		return
	}
	select {
	case rc.pktCh <- rtpMsg{pkt: pkt, video: true}:
	default:
		rc.noteDrop()
	}
}

// OnAudioRTP enqueues an audio RTP packet for async processing.
func (rc *RecordingConsumer) OnAudioRTP(pkt *rtp.Packet) {
	if rc.closed.Load() {
		return
	}
	select {
	case rc.pktCh <- rtpMsg{pkt: pkt, video: false}:
	default:
		rc.noteDrop()
	}
}

// noteDrop records a packet the queue could not accept. The queue stays
// nonblocking on purpose: rtsp.Source fans out synchronously on its connection
// goroutine, so blocking here would stall the reader for every consumer on that
// source. The drop is therefore not preventable at this point, but it must not
// be silent. It is counted, warned about at a bounded rate, and marked so the
// segment is closed at the discontinuity instead of writing a hole into the
// middle of a file that still claims to be continuous (the decode errors that
// follow a hole are swallowed by the writer, so the gap has no other tell).
func (rc *RecordingConsumer) noteDrop() {
	total := rc.droppedPackets.Add(1)
	rc.discontinuous.Store(true)

	now := time.Now().UnixNano()
	last := rc.lastDropLog.Load()
	if now-last >= int64(dropLogInterval) && rc.lastDropLog.CompareAndSwap(last, now) {
		slog.Warn("recording queue full, dropping packets",
			"camera", rc.camera, "dropped_total", total)
	}
}

// OnDisconnect is called when the RTSP source disconnects.
func (rc *RecordingConsumer) OnDisconnect() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.closeCurrentSegment()
}

// Close finalizes the current segment and stops the processing goroutine.
// It is idempotent: repeated calls (teardown plus an OnDisconnect-driven path)
// return without touching the already-finalized state.
func (rc *RecordingConsumer) Close() {
	if !rc.closed.CompareAndSwap(false, true) {
		return
	}
	close(rc.stop)
	<-rc.done // wait for processLoop to finish

	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.closeCurrentSegment()
}

func (rc *RecordingConsumer) processLoop() {
	defer close(rc.done)

	for {
		select {
		case msg := <-rc.pktCh:
			rc.dispatch(msg)
		case <-rc.stop:
			// Drain what is already queued so the final GOP is written
			// before the segment is finalized, then stop.
			for {
				select {
				case msg := <-rc.pktCh:
					rc.dispatch(msg)
				default:
					return
				}
			}
		}
	}
}

// dispatch processes a single packet. A corrupt or malformed stream can make
// the H264 depacketize/decode/mux path panic; recovering per-packet keeps the
// processLoop goroutine alive so one bad packet degrades this camera's
// recording instead of taking the whole process down. The recover runs while
// rc.mu is still held (its deferred Unlock is registered first, so it runs
// last), allowing the poisoned segment writer to be discarded safely; the next
// packet then starts a fresh segment.
func (rc *RecordingConsumer) dispatch(msg rtpMsg) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic while processing packet",
				"camera", rc.camera,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			rc.discardWriterAfterPanic()
		}
	}()

	if rc.paused {
		rc.handlePaused()
		return
	}
	if rc.discontinuous.Swap(false) {
		rc.rotateAtDiscontinuity()
	}
	if msg.video {
		rc.processVideo(msg.pkt)
	} else {
		rc.processAudio(msg.pkt)
	}
}

// rotateAtDiscontinuity closes the open segment at a packet-drop gap so the
// file ends at the last frame actually written and the next one starts from a
// keyframe, rather than splicing post-gap frames onto a reference frame they
// were not coded against. Two conditions bound it, because the marker is
// raised once per dropped packet and cleared once per processed packet: a
// segment holding no media yet has nothing to splice onto and is already where
// the next one would start, and beyond dropRotateInterval the rotation buys
// less than the churn it costs. Called with rc.mu held.
func (rc *RecordingConsumer) rotateAtDiscontinuity() {
	if rc.writer == nil || !rc.writer.HasMedia() {
		return
	}
	if !rc.lastDropRotate.IsZero() && time.Since(rc.lastDropRotate) < dropRotateInterval {
		return
	}
	rc.lastDropRotate = time.Now()
	slog.Warn("closing segment at a packet-drop discontinuity",
		"camera", rc.camera, "path", rc.segPath,
		"dropped_total", rc.droppedPackets.Load())
	rc.closeCurrentSegment()
}

// discardWriterAfterPanic drops the current segment writer after a panic so a
// half-updated GOP buffer can't trigger repeated panics. Called with rc.mu
// held. The partial segment file is best-effort closed and removed.
func (rc *RecordingConsumer) discardWriterAfterPanic() {
	if rc.writer == nil {
		return
	}
	_, _ = rc.writer.Close()
	rc.removeSegment()
	rc.writer = nil
	rc.currentPath = ""
}

// removeSegment deletes a segment file and retracts the row registered for it
// when the file was created. Both have to go together: the row is written at
// creation time so the segment is queryable before it rotates, so a removal
// that only unlinks the file leaves the DB pointing at a path that no longer
// exists, which fails every clip extraction over that range and is never
// reclaimed by retention. Called with rc.mu held.
func (rc *RecordingConsumer) removeSegment() {
	if rc.segPath == "" {
		return
	}
	if err := os.Remove(rc.segPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove segment file",
			"camera", rc.camera, "path", rc.segPath, "error", err)
	}
	if rc.onSegmentRemoved != nil {
		rc.onSegmentRemoved(rc.segPath)
	}
}

// handlePaused checks if disk space has recovered. Called with mu held.
func (rc *RecordingConsumer) handlePaused() {
	if time.Since(rc.pausedSince) < diskPauseRetryInterval {
		return
	}

	avail := rc.disk.Available()
	threshold := rc.disk.MinRequired()
	if avail < threshold {
		rc.pausedSince = time.Now()
		if time.Since(rc.lastDiskWarning) > time.Minute {
			slog.Warn("recording still paused, disk space low",
				"camera", rc.camera,
				"available_mb", avail/(1024*1024),
				"required_mb", threshold/(1024*1024),
			)
			rc.lastDiskWarning = time.Now()
		}
		return
	}

	slog.Info("recording resumed, disk space recovered",
		"camera", rc.camera,
		"available_mb", avail/(1024*1024),
	)
	rc.paused = false
	rc.pausedAtomic.Store(false)
	rc.writeErrors = 0
}

func (rc *RecordingConsumer) processVideo(pkt *rtp.Packet) {
	if err := rc.ensureSegment(); err != nil {
		rc.handleWriteError(err)
		return
	}

	err := rc.writer.WriteVideo(pkt)
	if errors.Is(err, ErrTimestampGap) {
		// The stream stalled (or its clock jumped) mid-segment without an
		// RTSP disconnect. Close the segment at its honest end and start a
		// fresh file so wall time and media time stay 1:1 within every
		// segment; the DB then records the coverage gap instead of papering
		// over it. The packet is re-fed to the fresh writer, which accepts
		// any starting timestamp (and waits for a keyframe as usual).
		slog.Warn("video timestamp gap, rotating segment",
			"camera", rc.camera, "path", rc.segPath)
		rc.closeCurrentSegment()
		if err := rc.ensureSegment(); err != nil {
			rc.handleWriteError(err)
			return
		}
		err = rc.writer.WriteVideo(pkt)
	}
	if err != nil {
		rc.handleWriteError(err)
		return
	}

	rc.writeErrors = 0
	rc.maybeRotate()
}

func (rc *RecordingConsumer) processAudio(pkt *rtp.Packet) {
	if rc.writer == nil {
		return
	}

	if err := rc.writer.WriteAudio(pkt); err != nil {
		rc.handleWriteError(err)
	}
}

// handleWriteError handles write failures. On repeated errors (likely disk full),
// it closes the segment and pauses recording. Called with mu held.
func (rc *RecordingConsumer) handleWriteError(err error) {
	rc.writeErrors++

	if rc.writeErrors >= 3 {
		slog.Error("repeated write failures, pausing recording",
			"camera", rc.camera,
			"error", err,
			"consecutive_errors", rc.writeErrors,
		)
		rc.closeCurrentSegment()
		rc.paused = true
		rc.pausedAtomic.Store(true)
		rc.pausedSince = time.Now()
		rc.lastDiskWarning = time.Now()
		return
	}

	slog.Error("write failed", "camera", rc.camera, "error", err)
}

func (rc *RecordingConsumer) ensureSegment() error {
	if rc.writer != nil {
		return nil
	}

	// A create that timed out is still wedged in the kernel, holding two
	// goroutines: the one blocked in os.Create and the one waiting to reap it.
	// Recording retries every diskPauseRetryInterval for as long as the volume
	// is out, so starting a fresh attempt each time makes that leak unbounded.
	// One outstanding create is enough to tell whether the volume has come
	// back.
	if rc.createPending.Load() {
		return fmt.Errorf("segment writer creation still outstanding after %s (stalled volume)", rc.createTimeout)
	}

	// Check disk space before creating a new segment
	avail := rc.disk.Available()
	threshold := rc.disk.MinRequired()
	if avail < threshold {
		if time.Since(rc.lastDiskWarning) > time.Minute {
			slog.Warn("recording paused, insufficient disk space",
				"camera", rc.camera,
				"available_mb", avail/(1024*1024),
				"required_mb", threshold/(1024*1024),
			)
			rc.lastDiskWarning = time.Now()
		}
		rc.paused = true
		rc.pausedAtomic.Store(true)
		rc.pausedSince = time.Now()
		return fmt.Errorf("insufficient disk space: %d MB available", avail/(1024*1024))
	}

	now := time.Now()
	rc.segStart = now
	// Filenames have second resolution; a gap rotation can open the next
	// segment within the same second, which would silently truncate the file
	// just closed. Suffix same-second names with a counter (no Stat: a probe
	// on a stalled volume would hang the processLoop, see
	// segmentWriterCreateTimeout).
	base := now.Format("2006-01-02_15-04-05")
	if base == rc.lastSegBase {
		rc.segDupCount++
		base = fmt.Sprintf("%s_%d", base, rc.segDupCount)
	} else {
		rc.lastSegBase = base
		rc.segDupCount = 0
	}
	rc.segPath = filepath.Join(rc.segDir, base+".mp4")

	type writerResult struct {
		w   *SegmentWriter
		err error
	}
	path := rc.segPath
	resultCh := make(chan writerResult, 1)
	rc.createPending.Store(true)
	go func() {
		w, err := rc.newWriter(path, rc.videoTrack, rc.audioTrack)
		resultCh <- writerResult{w, err}
	}()

	select {
	case res := <-resultCh:
		rc.createPending.Store(false)
		if res.err != nil {
			return fmt.Errorf("create segment writer: %w", res.err)
		}
		rc.writer = res.w
	case <-time.After(rc.createTimeout):
		// Stalled volume: os.Create is wedged in the kernel and will not
		// return until the device errors or recovers. Abandon it (one
		// orphaned goroutine, freed when the syscall finally fails) and
		// surface a write error so handleWriteError pauses recording; the
		// pause/resume path then retries and self-heals once I/O works.
		// createPending stays set until this reaper runs, so the retries do
		// not stack up more stranded goroutines behind the same wedge.
		go func() {
			res := <-resultCh
			rc.createPending.Store(false)
			if res.err == nil && res.w != nil {
				_, _ = res.w.Close()
				_ = os.Remove(path)
			}
		}()
		return fmt.Errorf("segment writer creation timed out after %s (stalled volume)", rc.createTimeout)
	}
	rc.currentPath = rc.segPath

	slog.Debug("started new segment", "camera", rc.camera, "path", rc.segPath)

	// Register segment in DB immediately so it's queryable before rotation.
	// Uses projected end time; closeCurrentSegment will update with actual values.
	if rc.onSegment != nil {
		rc.onSegment(SegmentInfo{
			Camera:    rc.camera,
			Path:      rc.segPath,
			StartTime: rc.segStart,
			EndTime:   rc.segStart.Add(rc.segLen),
			SizeBytes: 0,
		})
	}

	return nil
}

func (rc *RecordingConsumer) maybeRotate() {
	if time.Since(rc.segStart) < rc.segLen {
		return
	}
	rc.closeCurrentSegment()
}

func (rc *RecordingConsumer) closeCurrentSegment() {
	if rc.writer == nil {
		return
	}

	duration, closeErr := rc.writer.Close()
	// Honest times: the writer's first sample (the keyframe that opened the
	// file, media tick 0) plus the media duration actually written. The
	// projected record from ensureSegment used file-creation time and the
	// nominal segment length; this upsert corrects both.
	start := rc.writer.StartTime()
	if closeErr != nil {
		// The final flush failed, so every sample since the last successful
		// flush is gone. Trim the segment to the media that actually reached
		// the file instead of registering coverage this file does not have:
		// playback of the tail would run off the end, and the gap would be
		// invisible to every query.
		durable := rc.writer.FlushedDuration()
		slog.Error("close segment failed, trimming to the last durable part",
			"camera", rc.camera, "path", rc.segPath, "error", closeErr,
			"written", duration.Round(time.Millisecond),
			"durable", durable.Round(time.Millisecond))
		duration = durable
	}

	info, statErr := os.Stat(rc.segPath)
	switch {
	case statErr != nil || info.Size() == 0:
		rc.removeSegment()
	case closeErr != nil && duration == 0:
		// Nothing survived the failed flush: the file holds at most an init
		// segment, which carries no media.
		rc.removeSegment()
	default:
		if rc.onSegment != nil {
			rc.onSegment(SegmentInfo{
				Camera:    rc.camera,
				Path:      rc.segPath,
				StartTime: start,
				EndTime:   start.Add(duration),
				SizeBytes: info.Size(),
			})
		}
		slog.Debug("segment completed", "camera", rc.camera, "path", rc.segPath,
			"duration", duration.Round(time.Second), "size", info.Size())
	}

	rc.writer = nil
	rc.currentPath = ""
}

// Camera returns the camera name this consumer records for.
func (rc *RecordingConsumer) Camera() string {
	return rc.camera
}

// CurrentSegmentPath returns the absolute path of the segment currently
// being written, or "" if no segment is open. Safe for concurrent use.
func (rc *RecordingConsumer) CurrentSegmentPath() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.currentPath
}

// SetTestState seeds the consumer's identity and open-path fields for
// tests. Do not call from production code.
func (rc *RecordingConsumer) SetTestState(camera, path string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.camera = camera
	rc.currentPath = path
}
