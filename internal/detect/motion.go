package detect

import "math"

// MotionScore compares two raw RGB frames and returns a normalized score
// between 0.0 (identical) and 1.0 (completely different).
// Both frames must be the same length (width * height * 3 for RGB24).
func MotionScore(prev, curr []byte) float64 {
	if len(prev) != len(curr) || len(prev) == 0 {
		return 0
	}

	var totalDiff uint64
	pixels := len(prev) / 3

	for i := 0; i < len(prev); i += 3 {
		// Average the RGB channels for a simple luminance approximation
		prevLum := int(prev[i]) + int(prev[i+1]) + int(prev[i+2])
		currLum := int(curr[i]) + int(curr[i+1]) + int(curr[i+2])
		diff := prevLum - currLum
		if diff < 0 {
			diff = -diff
		}
		totalDiff += uint64(diff)
	}

	// Normalize: max diff per pixel is 255*3 = 765
	maxDiff := float64(pixels) * 765.0
	score := float64(totalDiff) / maxDiff

	return math.Min(score, 1.0)
}

// MotionRegion represents a contiguous area of detected motion.
type MotionRegion struct {
	Box   [4]int // x1, y1, x2, y2
	Area  int
	Score float64 // fraction of pixels above threshold within the region
}

// MotionDetector performs contour-based motion detection using a running
// background model with adaptive alpha blending.
type MotionDetector struct {
	threshold uint8
	minArea   int
	// bgAlpha is the blending factor and bg the background model, one grey
	// level per pixel, both in fixed point with bgFracBits fractional bits.
	bgAlpha int64
	bg      []int32

	// Pre-allocated working buffers, sized on first Detect call.
	blurred      []uint8
	binary       []uint8
	labels       []int
	parent       []int
	lastCoverage float64
}

// fixedBlend returns the blending factor as a fixed-point fraction, held to
// the range 0 to 1. A positive factor too small to represent becomes the
// smallest step, so a background meant to adapt slowly is never frozen. A
// factor that is not a number gives a static background.
func fixedBlend(alpha float64) int64 {
	if math.IsNaN(alpha) || alpha <= 0 {
		return 0
	}
	return max(int64(math.Round(min(alpha, 1)*(1<<bgFracBits))), 1)
}

// NewMotionDetector creates a MotionDetector.
// threshold: pixel difference threshold for binary mask (0-255).
// minArea: minimum number of pixels for a motion region to be reported.
// bgAlpha: blending factor for background update (0.0 = static, 1.0 = instant).
func NewMotionDetector(threshold uint8, minArea int, bgAlpha float64) *MotionDetector {
	return &MotionDetector{
		threshold: threshold,
		minArea:   minArea,
		bgAlpha:   fixedBlend(bgAlpha),
	}
}

// bgFracBits is the number of fractional bits in the background model. A grey
// level shifted by it stays inside an int32, and with 16 bits the smallest
// blending step is far below one grey level for any usable blending factor.
const bgFracBits = 16

// Detect processes a frame of one grey level per pixel, such as the luma plane
// of a decoded picture, and returns bounding boxes of motion regions. It does
// not keep the frame.
func (m *MotionDetector) Detect(gray []byte, width, height int) []MotionRegion {
	pixels := width * height
	if pixels <= 0 || len(gray) != pixels {
		return nil
	}

	// Ensure working buffers are allocated and correctly sized.
	if cap(m.blurred) < pixels {
		m.blurred = make([]uint8, pixels)
		m.binary = make([]uint8, pixels)
		m.labels = make([]int, pixels)
	}
	m.blurred = m.blurred[:pixels]
	m.binary = m.binary[:pixels]
	m.labels = m.labels[:pixels]

	// Initialize background model on first frame
	if m.bg == nil {
		m.bg = make([]int32, pixels)
		for i, v := range gray {
			m.bg[i] = int32(v) << bgFracBits
		}
		m.lastCoverage = 0
		return nil
	}

	// Apply 3x3 box blur to reduce noise
	boxBlur3x3(gray, m.blurred, width, height)

	// One pass marks every pixel that differs from the background by more
	// than the threshold and then blends the pixel into the background. The
	// pass is free of branches: on a quiet scene the sign of the difference is
	// sensor noise, which a branch predictor cannot follow.
	bg := m.bg[:pixels]
	blurred := m.blurred[:pixels]
	binary := m.binary[:pixels]
	threshold := int32(m.threshold) << bgFracBits
	var totalFG int
	for i, v := range blurred {
		level := bg[i]
		diff := int32(v)<<bgFracBits - level
		sign := diff >> 31
		magnitude := (diff ^ sign) - sign
		// threshold-magnitude is negative exactly when the pixel is foreground.
		fg := uint8(uint32(threshold-magnitude) >> 31)
		binary[i] = fg
		totalFG += int(fg)
		// The step is rounded away from zero, so the background arrives at a
		// level that holds instead of stopping a fraction short of it.
		awayFromZero := int64(^sign) & (1<<bgFracBits - 1)
		bg[i] = level + int32((int64(diff)*m.bgAlpha+awayFromZero)>>bgFracBits)
	}

	m.lastCoverage = float64(totalFG) / float64(pixels)

	if totalFG == 0 {
		m.lastCoverage = 0
		return nil
	}

	// Connected component labeling to find contiguous motion regions
	regions := m.connectedComponents(width, height)

	// Compute score for each region
	for i := range regions {
		r := &regions[i]
		regionPixels := (r.Box[2] - r.Box[0]) * (r.Box[3] - r.Box[1])
		if regionPixels > 0 {
			r.Score = float64(r.Area) / float64(regionPixels)
		}
	}

	return regions
}

// boxBlur3x3 writes into dst the mean of each src pixel and its neighbours.
// A pixel on the border averages only the neighbours that exist.
//
// Nearly every pixel is interior, with all nine neighbours present, so those
// are summed from three row slices without a bounds or edge test per
// neighbour; the border goes through blurBorderPixel.
func boxBlur3x3(src, dst []uint8, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	// A single column has no interior and no second column to start the sums from.
	if w < 2 {
		for y := range h {
			for x := range w {
				dst[y*w+x] = blurBorderPixel(src, w, h, x, y)
			}
		}
		return
	}
	for x := range w {
		dst[x] = blurBorderPixel(src, w, h, x, 0)
		dst[(h-1)*w+x] = blurBorderPixel(src, w, h, x, h-1)
	}
	for y := 1; y < h-1; y++ {
		above := src[(y-1)*w : y*w]
		row := src[y*w : (y+1)*w]
		below := src[(y+1)*w : (y+2)*w]
		out := dst[y*w : (y+1)*w]
		out[0] = blurBorderPixel(src, w, h, 0, y)
		out[w-1] = blurBorderPixel(src, w, h, w-1, y)
		// Each column's three pixels are summed once and reused by the three
		// output pixels that column contributes to.
		left := uint32(above[0]) + uint32(row[0]) + uint32(below[0])
		mid := uint32(above[1]) + uint32(row[1]) + uint32(below[1])
		for x := 1; x < w-1; x++ {
			right := uint32(above[x+1]) + uint32(row[x+1]) + uint32(below[x+1])
			out[x] = uint8((left + mid + right) / 9)
			left, mid = mid, right
		}
	}
}

// blurBorderPixel returns the mean of the pixel at (x, y) and whichever of its
// eight neighbours lie inside the image.
func blurBorderPixel(src []uint8, w, h, x, y int) uint8 {
	var sum, count int
	for ny := max(y-1, 0); ny <= min(y+1, h-1); ny++ {
		for nx := max(x-1, 0); nx <= min(x+1, w-1); nx++ {
			sum += int(src[ny*w+nx])
			count++
		}
	}
	return uint8(sum / count)
}

// connectedComponents performs two-pass connected component labeling on
// m.binary and returns bounding boxes of components with area >= m.minArea.
// It reuses m.labels and m.parent to avoid allocations.
func (m *MotionDetector) connectedComponents(w, h int) []MotionRegion {
	labels := m.labels
	// Zero the labels buffer
	for i := range labels {
		labels[i] = 0
	}

	// Reuse parent slice: reset length to 1 (index 0 is unused, labels start at 1).
	const initialParentCap = 64
	if cap(m.parent) < initialParentCap {
		m.parent = make([]int, 1, initialParentCap)
	} else {
		m.parent = m.parent[:1]
	}
	m.parent[0] = 0
	parent := m.parent
	nextLabel := 1

	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}

	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra > rb {
				ra, rb = rb, ra
			}
			parent[rb] = ra
		}
	}

	binary := m.binary

	// First pass: assign provisional labels
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if binary[idx] == 0 {
				continue
			}

			left := 0
			above := 0
			if x > 0 && labels[idx-1] > 0 {
				left = labels[idx-1]
			}
			if y > 0 && labels[idx-w] > 0 {
				above = labels[idx-w]
			}

			if left == 0 && above == 0 {
				labels[idx] = nextLabel
				parent = append(parent, nextLabel)
				nextLabel++
			} else if left > 0 && above == 0 {
				labels[idx] = find(left)
			} else if left == 0 && above > 0 {
				labels[idx] = find(above)
			} else {
				// Both neighbors labeled
				minLabel := left
				if find(above) < find(left) {
					minLabel = above
				}
				labels[idx] = find(minLabel)
				union(left, above)
			}
		}
	}

	// Save parent back in case append relocated it
	m.parent = parent

	// Second pass: resolve labels and collect bounding boxes
	type bbox struct {
		x1, y1, x2, y2, area int
	}
	boxes := map[int]*bbox{}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if labels[idx] == 0 {
				continue
			}
			root := find(labels[idx])
			labels[idx] = root

			b, ok := boxes[root]
			if !ok {
				b = &bbox{x1: x, y1: y, x2: x + 1, y2: y + 1}
				boxes[root] = b
			}
			if x < b.x1 {
				b.x1 = x
			}
			if y < b.y1 {
				b.y1 = y
			}
			if x+1 > b.x2 {
				b.x2 = x + 1
			}
			if y+1 > b.y2 {
				b.y2 = y + 1
			}
			b.area++
		}
	}

	// Filter by minimum area
	var regions []MotionRegion
	for _, b := range boxes {
		if b.area >= m.minArea {
			regions = append(regions, MotionRegion{
				Box:  [4]int{b.x1, b.y1, b.x2, b.y2},
				Area: b.area,
			})
		}
	}
	return regions
}

// FrameCoverage returns the fraction of foreground pixels from the last Detect call.
func (m *MotionDetector) FrameCoverage() float64 {
	return m.lastCoverage
}
