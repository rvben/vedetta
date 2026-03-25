package camera

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/snapshot"
)

// Event represents a detected object event from a camera.
type Event struct {
	ID                string      `json:"id"`
	CameraName        string      `json:"camera"`
	Label             string      `json:"label"`
	Score             float32     `json:"score"`
	Box               [4]int      `json:"box"` // x1, y1, x2, y2
	Timestamp         time.Time   `json:"timestamp"`
	EndTime           time.Time   `json:"end_time,omitempty"` // when the tracked object left the frame
	SnapshotPath      string      `json:"snapshot_path,omitempty"`
	SnapshotAvailable bool        `json:"snapshot_available"`
	ClipPath          string      `json:"clip_path,omitempty"`
	ClipAvailable     bool        `json:"clip_available"`
	ZoneName          string      `json:"zone_name,omitempty"`
	ObjectName        string      `json:"object_name,omitempty"`
	SubLabel          string      `json:"sub_label,omitempty"`
	SnapshotImage     *image.RGBA `json:"-"` // clean frame for disk/embeddings
	AnnotatedImage    *image.RGBA `json:"-"` // annotated frame for MQTT display
}

// EventEnd signals that a tracked object has left the frame.
type EventEnd struct {
	EventID    string
	CameraName string
	EndTime    time.Time
}

// MotionActivity carries per-minute motion intensity for timeline display.
type MotionActivity struct {
	CameraName string
	Bucket     time.Time
	Score      float64
}

// FaceEvent carries face detection results from the camera detection loop.
type FaceEvent struct {
	Camera  string
	EventID string
	Results []detect.FaceResult
}

// Camera manages a single RTSP camera stream.
type Camera struct {
	config               config.CameraConfig
	detector             *detect.Detector
	tracker              *detect.Tracker
	motionDetector       *detect.MotionDetector
	events               chan<- Event
	eventEnds            chan<- EventEnd
	presenceEvents       chan<- PresenceEvent
	hub                  *rtsp.Hub
	eventSnapDir         string
	eventSnapQuality     int
	latestSnapshotPath   string
	snapConsumer         *media.SnapshotConsumer
	detectEnabled        bool
	motionMinRegionScore float64
	motionActivity       chan<- MotionActivity
	motionBucketTime     time.Time
	motionBucketMax      float64

	mu               sync.RWMutex
	rawFrame         []byte // RGB24 frame data, guarded by mu
	frameW, frameH   int
	lastMotion       time.Time
	lastFrameTime    time.Time
	lastSnapshotSave time.Time
	confirmedTracks  map[int]string // trackID → eventID

	zones           []Zone
	presenceTracker *PresenceTracker

	faceRecognizer *detect.FaceRecognizer
	faceEvents     chan<- FaceEvent
	faceCropDir    string
	faceProcessed  map[int]time.Time
	degradedReason string
}

// CameraStatus represents the current status of a camera.
type CameraStatus struct {
	Name           string    `json:"name"`
	Online         bool      `json:"online"`
	HasMotion      bool      `json:"has_motion"`
	LastFrame      time.Time `json:"last_frame"`
	Degraded       bool      `json:"degraded"`
	DegradedReason string    `json:"degraded_reason,omitempty"`
	PTZ            bool      `json:"ptz"`
}

func NewCamera(cfg config.CameraConfig, detector *detect.Detector, motion config.MotionConfig, events chan<- Event, eventEnds chan<- EventEnd, presenceEvents chan<- PresenceEvent, hub *rtsp.Hub, snapshotPath string, snapshotQuality int, recordingPath string, faceRecognizer *detect.FaceRecognizer, faceEvents chan<- FaceEvent, faceCropDir string, motionActivity chan<- MotionActivity) *Camera {
	if snapshotQuality <= 0 {
		snapshotQuality = 85
	}
	cam := &Camera{
		config:               cfg,
		detector:             detector,
		tracker:              detect.NewTracker(30, 3),
		motionDetector:       detect.NewMotionDetector(motion.PixelThreshold, motion.MinArea, motion.BackgroundAlpha),
		events:               events,
		eventEnds:            eventEnds,
		presenceEvents:       presenceEvents,
		hub:                  hub,
		eventSnapDir:         snapshotPath,
		eventSnapQuality:     snapshotQuality,
		latestSnapshotPath:   filepath.Join(recordingPath, cfg.Name, "latest.jpg"),
		detectEnabled:        cfg.DetectEnabled(),
		motionMinRegionScore: motion.MinRegionScore,
		motionActivity:       motionActivity,
		confirmedTracks:      make(map[int]string),
		presenceTracker:      NewPresenceTracker(),
		faceRecognizer:       faceRecognizer,
		faceEvents:           faceEvents,
		faceCropDir:          faceCropDir,
		faceProcessed:        make(map[int]time.Time),
	}
	if cam.detectEnabled && !detector.Available() {
		cam.degradedReason = "object detector unavailable"
	}
	return cam
}

// SetZones replaces the camera's zone list and returns the old zones.
func (c *Camera) SetZones(zones []Zone) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.zones = zones
}

// Zones returns the current zones for this camera.
func (c *Camera) Zones() []Zone {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Zone(nil), c.zones...)
}

// PresenceTracker returns the camera's presence tracker for API access.
func (c *Camera) PresenceTracker() *PresenceTracker {
	return c.presenceTracker
}

func (c *Camera) Name() string {
	return c.config.Name
}

func (c *Camera) RecordURL() string {
	if c.config.RecordURL != "" {
		return c.config.RecordURL
	}
	return c.config.URL
}

// LastSnapshot converts the stored RGB24 frame to RGBA on demand.
func (c *Camera) LastSnapshot() *image.RGBA {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.rawFrame == nil {
		return nil
	}
	return rawToRGBA(c.rawFrame, c.frameW, c.frameH)
}

// snapshotPath returns the path for the cached latest snapshot.
func (c *Camera) snapshotPath() string {
	return c.latestSnapshotPath
}

// loadCachedSnapshot loads the last saved snapshot from disk so offline cameras
// still have an image to show.
func (c *Camera) loadCachedSnapshot() {
	path := c.snapshotPath()
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	img, err := jpeg.Decode(f)
	if err != nil {
		slog.Warn("failed to decode cached snapshot", "camera", c.config.Name, "error", err)
		return
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	rgb := make([]byte, w*h*3)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			off := ((y-bounds.Min.Y)*w + (x - bounds.Min.X)) * 3
			rgb[off] = byte(r >> 8)
			rgb[off+1] = byte(g >> 8)
			rgb[off+2] = byte(b >> 8)
		}
	}

	c.mu.Lock()
	c.rawFrame = rgb
	c.frameW = w
	c.frameH = h
	c.mu.Unlock()
	slog.Info("loaded cached snapshot", "camera", c.config.Name)
}

// saveCachedSnapshot writes the current frame to disk (throttled to every 30s).
func (c *Camera) saveCachedSnapshot() {
	c.mu.RLock()
	if c.rawFrame == nil {
		c.mu.RUnlock()
		return
	}
	if time.Since(c.lastSnapshotSave) < 30*time.Second {
		c.mu.RUnlock()
		return
	}
	img := rawToRGBA(c.rawFrame, c.frameW, c.frameH)
	c.mu.RUnlock()

	path := c.snapshotPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 80}); err != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	f.Close()
	os.Rename(tmp, path)

	c.mu.Lock()
	c.lastSnapshotSave = time.Now()
	c.mu.Unlock()
}

// Start begins reading frames from the RTSP stream via the Hub.
func (c *Camera) Start(ctx context.Context) {
	slog.Info("starting camera", "name", c.config.Name, "url", rtsp.SanitizeURL(c.config.URL))
	c.loadCachedSnapshot()

	go c.readFrames(ctx)
}

// readFrames connects to the RTSP stream via the Hub and processes detection frames.
func (c *Camera) readFrames(ctx context.Context) {
	w := c.config.Detect.Width
	h := c.config.Detect.Height
	fps := c.config.Detect.FPS

	source := c.hub.GetOrCreate(c.config.URL)

	// Wait for the source to connect and provide track info
	var videoTrack *rtsp.TrackInfo
	for {
		videoTrack = source.VideoTrack()
		if videoTrack != nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}

	consumer := media.NewDetectConsumer(c.config.Name, w, h, fps, videoTrack)
	if !consumer.Available() {
		c.setDegraded("detect decoder unavailable")
		return
	}
	source.AddConsumer(consumer)
	defer source.RemoveConsumer(consumer)
	defer consumer.Close()

	// Attach snapshot consumer to the main (high-res) stream for event snapshots
	c.startSnapshotConsumer(ctx)

	defer c.flushMotionBucket()

	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-consumer.Frames():
			c.processFrame(frame.Data, frame.Width, frame.Height)
		}
	}
}

func (c *Camera) flushMotionBucket() {
	if c.motionActivity == nil || c.motionBucketTime.IsZero() || c.motionBucketMax <= 0 {
		return
	}
	select {
	case c.motionActivity <- MotionActivity{
		CameraName: c.config.Name,
		Bucket:     c.motionBucketTime,
		Score:      c.motionBucketMax,
	}:
	default:
	}
}

// startSnapshotConsumer attaches a decoder to the main stream that caches
// the latest full-resolution frame for use in event snapshots.
func (c *Camera) startSnapshotConsumer(ctx context.Context) {
	recordURL := c.RecordURL()
	if recordURL == c.config.URL {
		// Same stream for detect and record — no benefit from a separate consumer
		return
	}

	mainSource := c.hub.GetOrCreate(recordURL)

	// Wait briefly for track info (main stream may already be connected for recording)
	var videoTrack *rtsp.TrackInfo
	for i := 0; i < 10; i++ {
		videoTrack = mainSource.VideoTrack()
		if videoTrack != nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if videoTrack == nil {
		slog.Warn("snapshot consumer: main stream not available", "camera", c.config.Name)
		return
	}

	sc := media.NewSnapshotConsumer(c.config.Name, videoTrack)
	if sc == nil {
		return
	}

	c.snapConsumer = sc
	mainSource.AddConsumer(sc)

	go func() {
		<-ctx.Done()
		mainSource.RemoveConsumer(sc)
		sc.Close()
	}()
}

// processFrame handles a decoded RGB24 frame — motion detection + YOLO.
func (c *Camera) processFrame(buf []byte, w, h int) {
	frameSize := w * h * 3

	// Store raw RGB24 frame for on-demand snapshot conversion.
	c.mu.Lock()
	if c.rawFrame == nil || len(c.rawFrame) != frameSize {
		c.rawFrame = make([]byte, frameSize)
	}
	copy(c.rawFrame, buf)
	c.frameW = w
	c.frameH = h
	c.lastFrameTime = time.Now()
	c.mu.Unlock()

	// Periodically save snapshot to disk for offline display
	c.saveCachedSnapshot()

	if !c.detectEnabled {
		return
	}

	// Contour-based motion detection
	motionRegions := c.motionDetector.Detect(buf, w, h)

	if c.motionActivity != nil {
		coverage := c.motionDetector.FrameCoverage()
		now := time.Now().Truncate(time.Minute)
		if !c.motionBucketTime.IsZero() && now != c.motionBucketTime {
			select {
			case c.motionActivity <- MotionActivity{
				CameraName: c.config.Name,
				Bucket:     c.motionBucketTime,
				Score:      c.motionBucketMax,
			}:
			default:
			}
			c.motionBucketMax = 0
		}
		c.motionBucketTime = now
		if coverage > c.motionBucketMax {
			c.motionBucketMax = coverage
		}
	}

	qualifiedMotion := false
	for _, region := range motionRegions {
		if region.Score >= c.motionMinRegionScore {
			qualifiedMotion = true
			break
		}
	}
	if qualifiedMotion {
		c.mu.Lock()
		c.lastMotion = time.Now()
		c.mu.Unlock()

		detections := c.detector.DetectRGB24(buf, w, h)
		tracked := c.tracker.Update(detections)

		// Collect all current detections for annotation
		allDetections := make([]detect.Detection, len(tracked))
		for i, obj := range tracked {
			allDetections[i] = detect.Detection{
				Label: obj.Label,
				Score: obj.Score,
				Box:   obj.Box,
			}
		}

		// Generate one annotated frame with ALL bounding boxes (reused for all new events).
		// Prefer the full-resolution main stream frame; fall back to the detection frame.
		var cleanFrame *image.RGBA   // clean snapshot for disk (embeddings, crops)
		var annotatedFrame *image.RGBA // annotated snapshot for display (MQTT)
		if c.eventSnapDir != "" {
			hasNewTrack := false
			for _, obj := range tracked {
				if _, active := c.confirmedTracks[obj.TrackID]; !active {
					hasNewTrack = true
					break
				}
			}
			if hasNewTrack {
				var fullRes *image.RGBA
				if sc := c.snapConsumer; sc != nil {
					fullRes = sc.LastFrame()
				}
				if fullRes != nil {
					// Clean copy for disk storage (no annotations)
					cleanFrame = image.NewRGBA(fullRes.Bounds())
					copy(cleanFrame.Pix, fullRes.Pix)
					// Annotated copy for display
					annotatedFrame = image.NewRGBA(fullRes.Bounds())
					copy(annotatedFrame.Pix, fullRes.Pix)
					// Scale detection boxes from detect resolution to full resolution
					frameW := annotatedFrame.Bounds().Dx()
					frameH := annotatedFrame.Bounds().Dy()
					scaled := make([]detect.Detection, len(allDetections))
					for i, d := range allDetections {
						scaled[i] = detect.Detection{
							Label: d.Label,
							Score: d.Score,
							Box: [4]int{
								d.Box[0] * frameW / w,
								d.Box[1] * frameH / h,
								d.Box[2] * frameW / w,
								d.Box[3] * frameH / h,
							},
						}
					}
					snapshot.DrawDetectionsInPlace(annotatedFrame, scaled)
				} else {
					// No full-res frame available, use detection frame
					cleanFrame = rawToRGBA(buf, w, h)
					annotatedFrame = image.NewRGBA(cleanFrame.Bounds())
					copy(annotatedFrame.Pix, cleanFrame.Pix)
					snapshot.DrawDetectionsInPlace(annotatedFrame, allDetections)
				}
			}
		}

		// Zone matching: tag each tracked object with matching zones and update presence
		c.mu.RLock()
		zones := c.zones
		c.mu.RUnlock()

		zoneMatches := make(map[PresenceKey]bool)
		trackZones := make(map[int][]Zone) // trackID → matched zones
		if len(zones) > 0 {
			for _, obj := range tracked {
				matched := MatchZones(zones, obj.Box, obj.Label, w, h)
				trackZones[obj.TrackID] = matched
				for _, z := range matched {
					if z.TrackPresence {
						zoneMatches[PresenceKey{ZoneID: z.ID, Label: obj.Label}] = true
					}
				}
			}

			// Update presence state machine
			zoneNameMap := make(map[int]string, len(zones))
			for _, z := range zones {
				zoneNameMap[z.ID] = z.Name
			}
			presenceEvts := c.presenceTracker.Update(zoneMatches, zoneNameMap)
			for _, pe := range presenceEvts {
				select {
				case c.presenceEvents <- pe:
				default:
					slog.Warn("presence event channel full, dropping", "zone", pe.ZoneName, "label", pe.Label, "type", pe.Type)
				}
			}
		}

		// Emit events for newly confirmed tracks
		for _, obj := range tracked {
			if _, active := c.confirmedTracks[obj.TrackID]; !active {
				// If zones are configured, only emit events for objects in at least one zone
				if len(zones) > 0 && len(trackZones[obj.TrackID]) == 0 {
					continue
				}

				eventID := fmt.Sprintf("%s-t%d-%d", c.config.Name, obj.TrackID, time.Now().UnixMilli())
				c.confirmedTracks[obj.TrackID] = eventID

				// Pick the first matched zone name for the event
				var zoneName string
				if matched := trackZones[obj.TrackID]; len(matched) > 0 {
					zoneName = matched[0].Name
				}

				box := obj.Box
				ev := Event{
					ID:                eventID,
					CameraName:        c.config.Name,
					Label:             obj.Label,
					Score:             obj.Score,
					Box:               box,
					Timestamp:         time.Now(),
					ZoneName:          zoneName,
					SnapshotAvailable: false,
					ClipAvailable:     false,
				}

				if cleanFrame != nil {
					snapFile := filepath.Join(c.eventSnapDir, c.config.Name, eventID+".jpg")
					ev.SnapshotPath = snapFile
					ev.SnapshotImage = cleanFrame // clean frame for disk (embeddings, crops)
					ev.AnnotatedImage = annotatedFrame // annotated for MQTT display
					// Scale box to snapshot resolution if different from detect resolution
					snapW := cleanFrame.Bounds().Dx()
					snapH := cleanFrame.Bounds().Dy()
					if snapW != w || snapH != h {
						ev.Box = [4]int{
							box[0] * snapW / w,
							box[1] * snapH / h,
							box[2] * snapW / w,
							box[3] * snapH / h,
						}
					}
				}

				c.events <- ev
			}
		}

		// Face recognition for person detections in face_recognition zones.
		// Runs after event IDs are assigned so every saved face can point at a real event row.
		if c.faceRecognizer != nil {
			now := time.Now()
			var rgbaFrame *image.RGBA
			for _, obj := range tracked {
				if obj.Label != "person" {
					continue
				}
				if lastRun, ok := c.faceProcessed[obj.TrackID]; ok && now.Sub(lastRun) < 5*time.Second {
					continue
				}
				inFaceZone := false
				if matched := trackZones[obj.TrackID]; len(matched) > 0 {
					for _, z := range matched {
						if z.FaceRecognition {
							inFaceZone = true
							break
						}
					}
				}
				if !inFaceZone {
					continue
				}
				eventID, ok := c.confirmedTracks[obj.TrackID]
				if !ok {
					continue
				}
				if rgbaFrame == nil {
					rgbaFrame = rawToRGBA(buf, w, h)
				}
				results := c.faceRecognizer.DetectAndEmbed(rgbaFrame, obj.Box, c.faceCropDir)
				c.faceProcessed[obj.TrackID] = now
				if len(results) > 0 {
					select {
					case c.faceEvents <- FaceEvent{
						Camera:  c.config.Name,
						EventID: eventID,
						Results: results,
					}:
					default:
						slog.Warn("face event channel full, dropping", "camera", c.config.Name)
					}
				}
			}
			for id, t := range c.faceProcessed {
				if now.Sub(t) > 2*time.Minute {
					delete(c.faceProcessed, id)
				}
			}
		}

		// Notify when tracked objects leave the frame
		for _, obj := range c.tracker.DeletedTracks() {
			if eventID, ok := c.confirmedTracks[obj.TrackID]; ok {
				c.eventEnds <- EventEnd{
					EventID:    eventID,
					CameraName: c.config.Name,
					EndTime:    time.Now(),
				}
				delete(c.confirmedTracks, obj.TrackID)
			}
		}
	}
}

// IsOnline returns true if the camera's detection RTSP source is connected.
func (c *Camera) IsOnline() bool {
	if c.hub == nil {
		return false
	}
	if src := c.hub.Get(c.config.URL); src != nil {
		return src.Connected()
	}
	return false
}

// HasMotion returns true if motion was detected within the last 5 seconds.
func (c *Camera) HasMotion() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.lastMotion.IsZero() && time.Since(c.lastMotion) < 5*time.Second
}

// Status returns the current status of the camera.
func (c *Camera) Status() CameraStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	online := false
	if c.hub != nil {
		if src := c.hub.Get(c.config.URL); src != nil {
			online = src.Connected()
		}
	}
	return CameraStatus{
		Name:           c.config.Name,
		Online:         online,
		HasMotion:      !c.lastMotion.IsZero() && time.Since(c.lastMotion) < 5*time.Second,
		LastFrame:      c.lastFrameTime,
		Degraded:       c.degradedReason != "",
		DegradedReason: c.degradedReason,
	}
}

// SnapshotRGB24 copies the raw RGB24 frame into dst and returns dimensions.
func (c *Camera) SnapshotRGB24(dst []byte) (w, h int, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.rawFrame == nil {
		return 0, 0, false
	}
	needed := len(c.rawFrame)
	if len(dst) < needed {
		return 0, 0, false
	}
	copy(dst, c.rawFrame)
	return c.frameW, c.frameH, true
}

// FrameSize returns the expected RGB24 frame size based on detect config.
func (c *Camera) FrameSize() int {
	return c.config.Detect.Width * c.config.Detect.Height * 3
}

func rawToRGBA(data []byte, w, h int) *image.RGBA {
	n := w * h
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pix := img.Pix
	for i := range n {
		si := i * 3
		di := i * 4
		pix[di+0] = data[si+0]
		pix[di+1] = data[si+1]
		pix[di+2] = data[si+2]
		pix[di+3] = 255
	}
	return img
}

func (c *Camera) setDegraded(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degradedReason = reason
}
