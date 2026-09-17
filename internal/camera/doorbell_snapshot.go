package camera

import (
	"context"
	"image"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/snapshot"
)

const doorbellSampleInterval = 500 * time.Millisecond

type doorbellFrameJob struct {
	frame      *image.RGBA
	at         time.Time
	detections []detect.Detection
	main       *image.RGBA
	mainAt     time.Time
}

type doorbellSnapshots struct {
	buffer    snapshot.DoorbellBuffer
	pending   chan doorbellFrameJob
	active    atomic.Bool
	mu        sync.Mutex
	lastOffer time.Time
	detector  *detect.Detector
	faces     *detect.FaceRecognizer
}

func newDoorbellSnapshots(detector *detect.Detector, faces *detect.FaceRecognizer) *doorbellSnapshots {
	return &doorbellSnapshots{pending: make(chan doorbellFrameJob, 1), detector: detector, faces: faces}
}

func (d *doorbellSnapshots) run(ctx context.Context) {
	d.active.Store(true)
	expiry := time.NewTicker(doorbellSampleInterval)
	defer expiry.Stop()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.active.Store(false)
		d.buffer.Clear()
		for {
			select {
			case <-d.pending:
			default:
				return
			}
		}
	}()
	var lastMain time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-expiry.C:
			d.buffer.Expire(now)
		case job := <-d.pending:
			if ctx.Err() != nil {
				return
			}
			// Analyse a main-stream image only once, using detections from that image
			// itself. Detection-stream boxes must never be applied to an older frame.
			if job.main != nil && job.mainAt.After(lastMain) && job.at.Sub(job.mainAt) <= snapshot.DoorbellWindow {
				lastMain = job.mainAt
				img := job.main
				if img != nil {
					var detections []detect.Detection
					if d.detector.Available() {
						detections = d.detector.TryDetect(img)
					}
					d.buffer.Add(img, job.mainAt, d.subjects(img, detections))
				}
			}
			if ctx.Err() != nil {
				return
			}
			// Substream candidates reuse the detections already computed for these
			// exact pixels, adding half-second choices between main-stream keyframes.
			if job.frame != nil && len(job.detections) > 0 {
				d.buffer.Add(job.frame, job.at, d.subjects(job.frame, job.detections))
			}
		}
	}
}

func (d *doorbellSnapshots) subjects(img *image.RGBA, detections []detect.Detection) []snapshot.Subject {
	people := make([]detect.Detection, 0, len(detections))
	for _, detection := range detections {
		if detection.Label == "person" && detection.Score >= 0.5 && boxArea(detection.Box) > 0 {
			people = append(people, detection)
		}
	}
	// Bound face analysis in a crowded scene; the largest nearby visitors are
	// the useful subjects for a doorbell photo.
	sort.SliceStable(people, func(i, j int) bool { return boxArea(people[i].Box) > boxArea(people[j].Box) })
	if len(people) > 3 {
		people = people[:3]
	}
	subjects := make([]snapshot.Subject, 0, len(people)*2)
	for _, person := range people {
		box := image.Rect(person.Box[0], person.Box[1], person.Box[2], person.Box[3])
		subjects = append(subjects, snapshot.Subject{Box: box, Confidence: float64(person.Score)})
		subjects = append(subjects, d.faceSubjects(img, person.Box)...)
	}
	if len(people) == 0 {
		b := img.Bounds()
		subjects = append(subjects, d.faceSubjects(img, [4]int{b.Min.X, b.Min.Y, b.Max.X, b.Max.Y})...)
	}
	return subjects
}

func (d *doorbellSnapshots) faceSubjects(img *image.RGBA, box [4]int) []snapshot.Subject {
	if d.faces == nil {
		return nil
	}
	faces := d.faces.TryDetectFaces(img, box)
	subjects := make([]snapshot.Subject, 0, len(faces))
	for _, face := range faces {
		subjects = append(subjects, snapshot.Subject{
			Box:        image.Rect(face.Box[0], face.Box[1], face.Box[2], face.Box[3]),
			Confidence: float64(face.Confidence), Face: true, Frontalness: faceFrontalness(face.Landmarks),
		})
	}
	return subjects
}

// A centred nose between separated eyes is a lightweight preference for a
// frontal view, not an identity match or a guarantee that a face is unobscured.
func faceFrontalness(points [5][2]float32) float64 {
	left, right, nose := points[0], points[1], points[2]
	eyeSpan := math.Hypot(float64(right[0]-left[0]), float64(right[1]-left[1]))
	if eyeSpan < 1 {
		return 0
	}
	midX, midY := float64(left[0]+right[0])/2, float64(left[1]+right[1])/2
	// Project the nose offset along the eye line; this also handles a tilted head.
	offset := math.Abs((float64(nose[0])-midX)*float64(right[0]-left[0])+(float64(nose[1])-midY)*float64(right[1]-left[1])) / (eyeSpan * eyeSpan)
	return max(0, 1-2*offset)
}

func boxArea(b [4]int) int { return max(0, b[2]-b[0]) * max(0, b[3]-b[1]) }

func (c *Camera) offerDoorbellFrame(buf []byte, w, h int, at time.Time, detections []detect.Detection) {
	d := c.doorbellSnapshots
	if d == nil || !d.active.Load() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active.Load() || at.Sub(d.lastOffer) < doorbellSampleInterval || len(d.pending) > 0 {
		return
	}
	d.lastOffer = at
	job := doorbellFrameJob{at: at}
	if sc := c.snapshotConsumer(); sc != nil {
		job.main, job.mainAt = sc.LastFrameAt()
	}
	for _, detection := range detections {
		if detection.Label == "person" && detection.Score >= 0.5 && boxArea(detection.Box) > 0 {
			// Raw detection buffers are reused by the decoder: own the copied pixels.
			job.frame = rawToRGBA(buf, w, h)
			job.detections = append([]detect.Detection(nil), detections...)
			break
		}
	}
	select {
	case d.pending <- job:
	default:
	}
}

func (c *Camera) doorbellFrame(at time.Time) (*image.RGBA, []byte) {
	if d := c.doorbellSnapshots; d != nil {
		if img, preview, frameAt, kind := d.buffer.BestWithPreview(at); img != nil {
			slog.Info("doorbell snapshot selected", "camera", c.config.Name, "selection", kind, "age_ms", at.Sub(frameAt).Milliseconds())
			return img, preview
		}
	}
	return c.LiveFrame(), nil
}
