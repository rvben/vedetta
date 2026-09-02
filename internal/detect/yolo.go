package detect

import (
	"image"
	"log/slog"
	"math"
	"sort"
)

const (
	modelInputSize = 640
	numClasses     = 80
	numDetections  = 8400 // YOLOv8 outputs 8400 candidate detections
	// yoloOutputSize is the element count of the (1, 84, 8400) YOLOv8 head:
	// 4 box coordinates plus one score per class, for every candidate.
	yoloOutputSize = (4 + numClasses) * numDetections
)

// prepareInput converts an RGBA image to a float32 tensor in CHW format
// normalized to [0, 1], resized to 640x640 with letterboxing.
func prepareInput(img *image.RGBA) ([]float32, float64, float64, float64) {
	buf := make([]float32, 3*modelInputSize*modelInputSize)
	return prepareInputInto(buf, img)
}

// prepareInputInto is like prepareInput but writes into the provided buffer,
// avoiding allocation. buf must have length >= 3*640*640.
func prepareInputInto(buf []float32, img *image.RGBA) ([]float32, float64, float64, float64) {
	bounds := img.Bounds()
	origW := float64(bounds.Dx())
	origH := float64(bounds.Dy())

	scale := math.Min(float64(modelInputSize)/origW, float64(modelInputSize)/origH)
	newW := int(origW * scale)
	newH := int(origH * scale)

	padX := (modelInputSize - newW) / 2
	padY := (modelInputSize - newH) / 2

	// Fill with gray (0.5) for letterbox padding.
	for i := range buf {
		buf[i] = 0.5
	}

	channelStride := modelInputSize * modelInputSize

	for y := 0; y < newH; y++ {
		srcY := int(float64(y) / scale)
		if srcY >= bounds.Dy() {
			srcY = bounds.Dy() - 1
		}
		for x := 0; x < newW; x++ {
			srcX := int(float64(x) / scale)
			if srcX >= bounds.Dx() {
				srcX = bounds.Dx() - 1
			}

			srcIdx := (srcY*bounds.Dx() + srcX) * 4 // RGBA stride
			r := float32(img.Pix[srcIdx+0]) / 255.0
			g := float32(img.Pix[srcIdx+1]) / 255.0
			b := float32(img.Pix[srcIdx+2]) / 255.0

			dstY := y + padY
			dstX := x + padX
			dstIdx := dstY*modelInputSize + dstX

			buf[0*channelStride+dstIdx] = r
			buf[1*channelStride+dstIdx] = g
			buf[2*channelStride+dstIdx] = b
		}
	}

	return buf, scale, float64(padX), float64(padY)
}

// prepareInputFromRGB24Into builds a float32 CHW tensor directly from RGB24 data,
// skipping the intermediate RGBA conversion. buf must have length >= 3*640*640.
func prepareInputFromRGB24Into(buf []float32, data []byte, w, h int) ([]float32, float64, float64, float64) {
	origW := float64(w)
	origH := float64(h)

	scale := math.Min(float64(modelInputSize)/origW, float64(modelInputSize)/origH)
	newW := int(origW * scale)
	newH := int(origH * scale)

	padX := (modelInputSize - newW) / 2
	padY := (modelInputSize - newH) / 2

	for i := range buf {
		buf[i] = 0.5
	}

	channelStride := modelInputSize * modelInputSize

	for y := 0; y < newH; y++ {
		srcY := int(float64(y) / scale)
		if srcY >= h {
			srcY = h - 1
		}
		for x := 0; x < newW; x++ {
			srcX := int(float64(x) / scale)
			if srcX >= w {
				srcX = w - 1
			}

			srcIdx := (srcY*w + srcX) * 3 // RGB24 stride
			r := float32(data[srcIdx+0]) / 255.0
			g := float32(data[srcIdx+1]) / 255.0
			b := float32(data[srcIdx+2]) / 255.0

			dstIdx := (y+padY)*modelInputSize + (x + padX)

			buf[0*channelStride+dstIdx] = r
			buf[1*channelStride+dstIdx] = g
			buf[2*channelStride+dstIdx] = b
		}
	}

	return buf, scale, float64(padX), float64(padY)
}

// processOutput extracts detections from the raw YOLOv8 output tensor.
// The output shape is (1, 84, 8400): 4 bbox coords + 80 class scores per detection.
// frameW and frameH are the dimensions of the frame the boxes are mapped back
// onto; boxes are clamped to it and empty ones are dropped.
//
// An output of any other size belongs to a different architecture that this
// decoder cannot read, so it yields no detections instead of indexing past the
// end of the tensor.
func processOutput(output []float32, scoreThreshold float32, scale, padX, padY float64, frameW, frameH int) []Detection {
	if len(output) != yoloOutputSize {
		slog.Error("unexpected model output size, expected the YOLOv8 (1,84,8400) layout",
			"got", len(output), "want", yoloOutputSize)
		return nil
	}

	var detections []Detection

	for i := 0; i < numDetections; i++ {
		// Find the class with highest confidence
		maxScore := float32(0)
		maxClass := 0
		for c := 0; c < numClasses; c++ {
			// Output layout: row-major (84, 8400), so element [row][col] = output[row*8400 + col]
			score := output[(4+c)*numDetections+i]
			if score > maxScore {
				maxScore = score
				maxClass = c
			}
		}

		if maxScore < scoreThreshold {
			continue
		}

		// Extract bounding box (center_x, center_y, width, height)
		cx := float64(output[0*numDetections+i])
		cy := float64(output[1*numDetections+i])
		w := float64(output[2*numDetections+i])
		h := float64(output[3*numDetections+i])

		// Convert from center format to corner format
		x1 := cx - w/2
		y1 := cy - h/2
		x2 := cx + w/2
		y2 := cy + h/2

		// Remove letterbox padding and scale back to original image coordinates
		x1 = (x1 - padX) / scale
		y1 = (y1 - padY) / scale
		x2 = (x2 - padX) / scale
		y2 = (y2 - padY) / scale

		box, ok := clampBox([4]int{int(x1), int(y1), int(x2), int(y2)}, frameW, frameH)
		if !ok {
			continue
		}

		label := "unknown"
		if maxClass < len(CocoLabels) {
			label = CocoLabels[maxClass]
		}

		detections = append(detections, Detection{
			Label: label,
			Score: maxScore,
			Box:   box,
		})
	}

	// Apply Non-Maximum Suppression
	detections = nms(detections, 0.5)

	return detections
}

// nms applies Non-Maximum Suppression to filter overlapping detections.
func nms(detections []Detection, iouThreshold float64) []Detection {
	if len(detections) == 0 {
		return nil
	}

	// Sort by score descending
	sort.Slice(detections, func(i, j int) bool {
		return detections[i].Score > detections[j].Score
	})

	keep := make([]bool, len(detections))
	for i := range keep {
		keep[i] = true
	}

	for i := 0; i < len(detections); i++ {
		if !keep[i] {
			continue
		}
		for j := i + 1; j < len(detections); j++ {
			if !keep[j] {
				continue
			}
			if detections[i].Label == detections[j].Label {
				if iou(detections[i].Box, detections[j].Box) > iouThreshold {
					keep[j] = false
				}
			}
		}
	}

	var result []Detection
	for i, d := range detections {
		if keep[i] {
			result = append(result, d)
		}
	}
	return result
}

// iou computes Intersection over Union between two bounding boxes.
func iou(a, b [4]int) float64 {
	x1 := math.Max(float64(a[0]), float64(b[0]))
	y1 := math.Max(float64(a[1]), float64(b[1]))
	x2 := math.Min(float64(a[2]), float64(b[2]))
	y2 := math.Min(float64(a[3]), float64(b[3]))

	intersection := math.Max(0, x2-x1) * math.Max(0, y2-y1)
	areaA := float64(a[2]-a[0]) * float64(a[3]-a[1])
	areaB := float64(b[2]-b[0]) * float64(b[3]-b[1])
	union := areaA + areaB - intersection

	if union == 0 {
		return 0
	}
	return intersection / union
}
