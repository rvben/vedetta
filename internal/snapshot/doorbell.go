package snapshot

import (
	"image"
	"math"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

const (
	DoorbellWindow    = 2 * time.Second
	doorbellMaxFrames = 8
	doorbellMaxBytes  = 64 << 20
)

// Subject describes a visible person or face, without identifying them.
// Boxes use the candidate image's coordinates. Frontalness is in [0,1].
type Subject struct {
	Box         image.Rectangle
	Confidence  float64
	Face        bool
	Frontalness float64
}

type doorbellCandidate struct {
	image   *image.RGBA
	preview []byte
	at      time.Time
	score   float64
	face    bool
}

// DoorbellBuffer retains a small, bounded set of already-scored visitor images.
// Images are immutable after Add. Best never runs inference or waits for a frame.
type DoorbellBuffer struct {
	mu     sync.Mutex
	frames []doorbellCandidate
	bytes  int
}

// Add scores only the subjects, so sharp background detail cannot win over a
// clearer visitor. Empty scenes are left to the camera's latest-frame fallback.
func (b *DoorbellBuffer) Add(img *image.RGBA, at time.Time, subjects []Subject) {
	if img == nil || at.IsZero() || len(img.Pix) > doorbellMaxBytes {
		return
	}
	best := doorbellCandidate{image: img, at: at}
	for _, subject := range subjects {
		if score := doorbellSubjectScore(img, subject); score > best.score {
			best.score, best.face = score, subject.Face
		}
	}
	if best.score == 0 {
		return
	}
	// Encoding happens in the background producer, never in Best.
	best.preview, _ = EncodeNotificationJPEG(img)
	if len(img.Pix)+len(best.preview) > doorbellMaxBytes {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Producers can complete out of order. Prune against the newest timestamp,
	// and reject work that finished after it had already become irrelevant.
	newest := at
	for _, f := range b.frames {
		if f.at.After(newest) {
			newest = f.at
		}
	}
	b.prune(newest.Add(-DoorbellWindow))
	if at.Before(newest.Add(-DoorbellWindow)) {
		return
	}
	b.frames = append(b.frames, best)
	b.bytes += len(img.Pix) + len(best.preview)
	for len(b.frames) > doorbellMaxFrames || b.bytes > doorbellMaxBytes {
		oldest := 0
		for i := range b.frames {
			if b.frames[i].at.Before(b.frames[oldest].at) {
				oldest = i
			}
		}
		b.remove(oldest)
	}
}

func (b *DoorbellBuffer) remove(i int) {
	b.bytes -= len(b.frames[i].image.Pix) + len(b.frames[i].preview)
	copy(b.frames[i:], b.frames[i+1:])
	b.frames[len(b.frames)-1] = doorbellCandidate{}
	b.frames = b.frames[:len(b.frames)-1]
}

func (b *DoorbellBuffer) prune(cutoff time.Time) {
	for i := len(b.frames) - 1; i >= 0; i-- {
		if b.frames[i].at.Before(cutoff) {
			b.remove(i)
		}
	}
}

// Best selects a frame from the two seconds preceding receipt of the press.
// Face visibility wins first, then sharpness, size, confidence and frontalness.
// A small age penalty favours newer frames when quality is otherwise similar.
func (b *DoorbellBuffer) Best(at time.Time) (*image.RGBA, time.Time, string) {
	img, _, captured, kind := b.BestWithPreview(at)
	return img, captured, kind
}

// BestWithPreview returns the selected pixels and their matching pre-encoded
// attachment atomically, so the notification and archived photo cannot diverge.
func (b *DoorbellBuffer) BestWithPreview(at time.Time) (*image.RGBA, []byte, time.Time, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(at.Add(-DoorbellWindow))
	var best doorbellCandidate
	bestScore := 0.0
	for _, f := range b.frames {
		if f.at.After(at) {
			continue
		}
		score := f.score - 0.05*at.Sub(f.at).Seconds()/DoorbellWindow.Seconds()
		if score > bestScore || (score == bestScore && f.at.After(best.at)) {
			best, bestScore = f, score
		}
	}
	kind := "person"
	if best.face {
		kind = "face"
	}
	if best.image == nil {
		kind = ""
	}
	return best.image, best.preview, best.at, kind
}

// Expire releases old images even while the scene is empty or the stream is
// offline. Best independently enforces the same age limit at press time.
func (b *DoorbellBuffer) Expire(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(at.Add(-DoorbellWindow))
}

func (b *DoorbellBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames = nil
	b.bytes = 0
}

func doorbellSubjectScore(img *image.RGBA, subject Subject) float64 {
	box := subject.Box.Intersect(img.Bounds())
	if box.Dx() < 8 || box.Dy() < 8 || subject.Confidence < 0.5 || math.IsNaN(subject.Confidence) {
		return 0
	}
	// Compare crops at a common scale so resolution alone does not masquerade
	// as sharpness. Laplacian energy is bounded to keep noise from dominating.
	crop := image.NewGray(image.Rect(0, 0, 64, 64))
	draw.ApproxBiLinear.Scale(crop, crop.Bounds(), img, box, draw.Src, nil)
	var sum, sumSq float64
	var exposed int
	for y := 1; y < 63; y++ {
		for x := 1; x < 63; x++ {
			i := y*crop.Stride + x
			center := int(crop.Pix[i])
			lap := float64(4*center - int(crop.Pix[i-1]) - int(crop.Pix[i+1]) - int(crop.Pix[i-crop.Stride]) - int(crop.Pix[i+crop.Stride]))
			sum += lap
			sumSq += lap * lap
			if center > 12 && center < 244 {
				exposed++
			}
		}
	}
	const n = 62 * 62
	variance := max(0, sumSq/n-(sum/n)*(sum/n))
	sharpness := variance / (variance + 100)
	exposure := float64(exposed) / n
	size := min(1, math.Sqrt(float64(box.Dx()*box.Dy()))/160)
	visible := float64(box.Dx()*box.Dy()) / float64(subject.Box.Dx()*subject.Box.Dy())
	confidence := min(1, subject.Confidence)
	tier := 1.0
	pose := visible
	if subject.Face {
		tier = 2
		pose = visible * max(0, min(1, subject.Frontalness))
	}
	return tier + 0.45*sharpness + 0.15*size + 0.15*confidence + 0.15*pose + 0.10*exposure
}
