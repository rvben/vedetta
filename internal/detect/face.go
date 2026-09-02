package detect

import (
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/detect/onnxruntime"
)

const (
	scrfdInputSize     = 640
	faceNetInputSize   = 112
	embeddingDim       = 512
	defaultMinFaceSize = 60
	defaultMatchThresh = 0.55
)

// FaceResult holds a single detected and embedded face.
type FaceResult struct {
	Box        [4]int        // face bounding box in original frame coords [x1,y1,x2,y2]
	Landmarks  [5][2]float32 // 5 facial keypoints in original frame coords
	Confidence float32       // face detection confidence
	Embedding  []float32     // 512-dim L2-normalized embedding
	CropPath   string        // path to saved 112x112 aligned JPEG
}

// FaceRecognizer detects faces and computes embeddings using SCRFD + MobileFaceNet.
// Safe for concurrent use — serializes access via mutex.
type FaceRecognizer struct {
	mu             sync.Mutex
	detector       *onnxruntime.Session
	embedder       *onnxruntime.Session
	detInputNames  []string
	detOutputNames []string
	embInputNames  []string
	embOutputNames []string
	minFaceSize    int
	matchThreshold float64
	detInputBuf    []float32 // reusable SCRFD input buffer
	embInputBuf    []float32 // reusable MobileFaceNet input buffer
}

// FaceRecognizerConfig holds configuration for the face recognition pipeline.
type FaceRecognizerConfig struct {
	SCRFDModelPath    string  // path to SCRFD ONNX model (optional, auto-download)
	MobileFaceNetPath string  // path to MobileFaceNet ONNX model (optional, auto-download)
	MinFaceSize       int     // minimum face height in pixels (default 60)
	MatchThreshold    float64 // cosine similarity threshold (default 0.55)
	CropDir           string  // directory to save aligned face crops
}

// NewFaceRecognizer creates a face recognition pipeline with SCRFD + MobileFaceNet.
func NewFaceRecognizer(cfg FaceRecognizerConfig) (*FaceRecognizer, error) {
	detData, err := loadFaceModel(cfg.SCRFDModelPath, scrfdFileName, downloadSCRFD)
	if err != nil {
		return nil, fmt.Errorf("load SCRFD model: %w", err)
	}

	embData, err := loadFaceModel(cfg.MobileFaceNetPath, mobileFaceNetFileName, downloadMobileFaceNet)
	if err != nil {
		return nil, fmt.Errorf("load MobileFaceNet model: %w", err)
	}

	detSession, err := onnxruntime.NewSession(detData)
	if err != nil {
		return nil, fmt.Errorf("create SCRFD session: %w", err)
	}

	embSession, err := onnxruntime.NewSession(embData)
	if err != nil {
		return nil, fmt.Errorf("create MobileFaceNet session: %w", err)
	}

	minFace := cfg.MinFaceSize
	if minFace <= 0 {
		minFace = defaultMinFaceSize
	}

	matchThresh := cfg.MatchThreshold
	if matchThresh <= 0 {
		matchThresh = defaultMatchThresh
	}

	fr := &FaceRecognizer{
		detector:       detSession,
		embedder:       embSession,
		detInputNames:  detSession.InputNames(),
		detOutputNames: detSession.OutputNames(),
		embInputNames:  embSession.InputNames(),
		embOutputNames: embSession.OutputNames(),
		minFaceSize:    minFace,
		matchThreshold: matchThresh,
	}

	slog.Info("face recognizer initialized",
		"scrfd_inputs", fr.detInputNames,
		"scrfd_outputs", fr.detOutputNames,
		"facenet_inputs", fr.embInputNames,
		"facenet_outputs", fr.embOutputNames,
		"min_face_size", minFace,
		"match_threshold", matchThresh,
	)

	return fr, nil
}

// Close releases resources held by the face recognizer.
func (fr *FaceRecognizer) Close() {}

// MatchThreshold returns the cosine similarity threshold for face matching.
func (fr *FaceRecognizer) MatchThreshold() float64 {
	return fr.matchThreshold
}

// DetectAndEmbed detects faces within a person bounding box and computes embeddings.
// frame is the full-resolution RGBA frame, personBox is [x1,y1,x2,y2] of the person.
// cropDir is the directory to save aligned face crops (empty string to skip saving).
func (fr *FaceRecognizer) DetectAndEmbed(frame *image.RGBA, personBox [4]int, cropDir string) (results []FaceResult) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("face recognition panic recovered", "error", r)
			results = nil
		}
	}()

	fr.mu.Lock()
	defer fr.mu.Unlock()

	// Crop the person region from the frame
	crop := cropRegion(frame, personBox)
	if crop.Bounds().Dx() < fr.minFaceSize || crop.Bounds().Dy() < fr.minFaceSize {
		return nil
	}

	// Run SCRFD face detection on the person crop
	faces := fr.detectFaces(crop)
	if len(faces) == 0 {
		return nil
	}

	for _, face := range faces {
		faceH := face.box[3] - face.box[1]
		if faceH < fr.minFaceSize {
			continue
		}

		frameBox, frameLandmarks, ok := faceInFrame(face, crop, frame.Bounds())
		if !ok {
			continue
		}

		// Align face using landmarks in full frame coordinates
		aligned := alignFace(frame, frameLandmarks)

		// Compute embedding
		embedding := fr.computeEmbedding(aligned)
		if embedding == nil {
			continue
		}

		result := FaceResult{
			Box:        frameBox,
			Landmarks:  frameLandmarks,
			Confidence: face.score,
			Embedding:  embedding,
		}

		// Save aligned crop as JPEG
		if cropDir != "" {
			cropPath := fr.saveCrop(aligned, cropDir)
			result.CropPath = cropPath
		}

		results = append(results, result)
	}

	return results
}

// faceInFrame maps a face detected inside a person crop back to frame
// coordinates and clamps the box to the frame. It reports false when the box
// covers no pixel of the frame, which leaves nothing to align, embed or draw.
//
// The offset is the crop's own origin. SCRFD reads the crop as an image
// starting at (0,0), and cropRegion clamps the requested person box to the
// frame, so a person box reaching outside the frame yields a crop that starts
// somewhere other than where the box asked.
func faceInFrame(face scrfdFace, crop *image.RGBA, frame image.Rectangle) ([4]int, [5][2]float32, bool) {
	origin := crop.Bounds().Min

	var landmarks [5][2]float32
	for i := range face.landmarks {
		landmarks[i][0] = face.landmarks[i][0] + float32(origin.X)
		landmarks[i][1] = face.landmarks[i][1] + float32(origin.Y)
	}

	box, ok := clampBoxToRect([4]int{
		face.box[0] + origin.X,
		face.box[1] + origin.Y,
		face.box[2] + origin.X,
		face.box[3] + origin.Y,
	}, frame)

	return box, landmarks, ok
}

// scrfdFace holds intermediate face detection results from SCRFD.
type scrfdFace struct {
	box       [4]int
	landmarks [5][2]float32
	score     float32
}

// detectFaces runs SCRFD on a cropped person region and returns detected faces.
func (fr *FaceRecognizer) detectFaces(crop *image.RGBA) []scrfdFace {
	input, scale, padX, padY := fr.prepareSCRFDInput(crop)

	inputTensor := onnxruntime.NewTensor(
		[]int64{1, 3, scrfdInputSize, scrfdInputSize}, input,
	)

	inputs := make(map[string]*onnxruntime.Tensor)
	for _, name := range fr.detInputNames {
		inputs[name] = inputTensor
	}

	outputs, err := fr.detector.Run(inputs)
	if err != nil {
		slog.Error("SCRFD inference failed", "error", err)
		return nil
	}

	return fr.postprocessSCRFD(outputs, scale, padX, padY)
}

// prepareSCRFDInput converts an RGBA image to the SCRFD input tensor.
// Resizes to 640x640 with letterboxing, normalizes: (pixel - 127.5) / 128.0.
// Returns the tensor data, scale factor, and padding offsets.
func (fr *FaceRecognizer) prepareSCRFDInput(img *image.RGBA) ([]float32, float64, float64, float64) {
	if fr.detInputBuf == nil {
		fr.detInputBuf = make([]float32, 3*scrfdInputSize*scrfdInputSize)
	}
	buf := fr.detInputBuf

	bounds := img.Bounds()
	origW := float64(bounds.Dx())
	origH := float64(bounds.Dy())

	scale := math.Min(float64(scrfdInputSize)/origW, float64(scrfdInputSize)/origH)
	newW := int(origW * scale)
	newH := int(origH * scale)

	padX := (scrfdInputSize - newW) / 2
	padY := (scrfdInputSize - newH) / 2

	// Fill with normalized zero: (0 - 127.5) / 128.0 ≈ -0.9961
	fillVal := float32(-127.5 / 128.0)
	for i := range buf {
		buf[i] = fillVal
	}

	channelStride := scrfdInputSize * scrfdInputSize

	for y := range newH {
		srcY := int(float64(y) / scale)
		if srcY >= bounds.Dy() {
			srcY = bounds.Dy() - 1
		}
		for x := range newW {
			srcX := int(float64(x) / scale)
			if srcX >= bounds.Dx() {
				srcX = bounds.Dx() - 1
			}

			// img may be a SubImage crop: its Pix is already re-sliced to
			// start at bounds.Min and its Rect.Min equals bounds.Min, so the
			// crop-local (srcX,srcY) indexes Pix directly. Adding bounds.Min
			// here would double-count the origin and read past the slice.
			srcIdx := srcY*img.Stride + srcX*4
			r := (float32(img.Pix[srcIdx+0]) - 127.5) / 128.0
			g := (float32(img.Pix[srcIdx+1]) - 127.5) / 128.0
			b := (float32(img.Pix[srcIdx+2]) - 127.5) / 128.0

			dstY := y + padY
			dstX := x + padX
			dstIdx := dstY*scrfdInputSize + dstX

			buf[0*channelStride+dstIdx] = r
			buf[1*channelStride+dstIdx] = g
			buf[2*channelStride+dstIdx] = b
		}
	}

	return buf, scale, float64(padX), float64(padY)
}

// postprocessSCRFD decodes SCRFD outputs into face detections.
// SCRFD det_500m outputs 9 tensors: 3 score + 3 bbox + 3 landmark tensors
// at stride levels 8, 16, 32.
func (fr *FaceRecognizer) postprocessSCRFD(outputs map[string]*onnxruntime.Tensor, scale, padX, padY float64) []scrfdFace {
	strides := []int{8, 16, 32}
	scoreThreshold := float32(0.5)

	// Collect output tensors in order by name
	// SCRFD outputs are named: score_8, score_16, score_32, bbox_8, bbox_16, bbox_32, kps_8, kps_16, kps_32
	// But actual names depend on the model. We sort by output order.
	orderedOutputs := make([]*onnxruntime.Tensor, len(fr.detOutputNames))
	for i, name := range fr.detOutputNames {
		orderedOutputs[i] = outputs[name]
	}

	if len(orderedOutputs) != 9 {
		slog.Warn("unexpected SCRFD output count", "count", len(orderedOutputs))
		return nil
	}

	// Outputs are ordered: score_8, score_16, score_32, bbox_8, bbox_16, bbox_32, kps_8, kps_16, kps_32
	scoreTensors := orderedOutputs[0:3]
	bboxTensors := orderedOutputs[3:6]
	kpsTensors := orderedOutputs[6:9]

	var faces []scrfdFace

	for level, stride := range strides {
		scores := scoreTensors[level]
		bboxes := bboxTensors[level]
		kps := kpsTensors[level]

		if scores == nil || bboxes == nil || kps == nil {
			continue
		}

		featH := scrfdInputSize / stride
		featW := scrfdInputSize / stride
		numAnchors := int(scores.Shape[0])

		anchorsPerPos := numAnchors / (featH * featW)
		if anchorsPerPos == 0 {
			anchorsPerPos = 1
		}

		for anchorIdx := range numAnchors {
			score := scores.Data[anchorIdx]
			if score < scoreThreshold {
				continue
			}

			// Compute anchor center
			posIdx := anchorIdx / anchorsPerPos
			cy := float64(posIdx/featW)*float64(stride) + float64(stride)/2.0
			cx := float64(posIdx%featW)*float64(stride) + float64(stride)/2.0

			// Decode bbox: offset from anchor center, scaled by stride
			bboxOff := anchorIdx * 4
			x1 := cx - float64(bboxes.Data[bboxOff+0])*float64(stride)
			y1 := cy - float64(bboxes.Data[bboxOff+1])*float64(stride)
			x2 := cx + float64(bboxes.Data[bboxOff+2])*float64(stride)
			y2 := cy + float64(bboxes.Data[bboxOff+3])*float64(stride)

			// Remove letterbox padding and scale to original coordinates
			x1 = (x1 - padX) / scale
			y1 = (y1 - padY) / scale
			x2 = (x2 - padX) / scale
			y2 = (y2 - padY) / scale

			// Decode landmarks
			var landmarks [5][2]float32
			kpsOff := anchorIdx * 10
			for k := range 5 {
				lx := cx + float64(kps.Data[kpsOff+k*2])*float64(stride)
				ly := cy + float64(kps.Data[kpsOff+k*2+1])*float64(stride)
				landmarks[k][0] = float32((lx - padX) / scale)
				landmarks[k][1] = float32((ly - padY) / scale)
			}

			faces = append(faces, scrfdFace{
				box:       [4]int{int(x1), int(y1), int(x2), int(y2)},
				landmarks: landmarks,
				score:     score,
			})
		}
	}

	// NMS
	faces = nmsFaces(faces, 0.4)

	return faces
}

// nmsFaces applies Non-Maximum Suppression to face detections.
func nmsFaces(faces []scrfdFace, iouThreshold float64) []scrfdFace {
	if len(faces) == 0 {
		return nil
	}

	sort.Slice(faces, func(i, j int) bool {
		return faces[i].score > faces[j].score
	})

	keep := make([]bool, len(faces))
	for i := range keep {
		keep[i] = true
	}

	for i := range faces {
		if !keep[i] {
			continue
		}
		for j := i + 1; j < len(faces); j++ {
			if !keep[j] {
				continue
			}
			if iou(faces[i].box, faces[j].box) > iouThreshold {
				keep[j] = false
			}
		}
	}

	var result []scrfdFace
	for i, f := range faces {
		if keep[i] {
			result = append(result, f)
		}
	}
	return result
}

// computeEmbedding runs MobileFaceNet on a 112x112 aligned face image.
// Returns a 512-dim L2-normalized embedding vector.
func (fr *FaceRecognizer) computeEmbedding(aligned *image.RGBA) []float32 {
	input := fr.prepareFaceNetInput(aligned)

	inputTensor := onnxruntime.NewTensor(
		[]int64{1, 3, faceNetInputSize, faceNetInputSize}, input,
	)

	inputs := make(map[string]*onnxruntime.Tensor)
	for _, name := range fr.embInputNames {
		inputs[name] = inputTensor
	}

	outputs, err := fr.embedder.Run(inputs)
	if err != nil {
		slog.Error("MobileFaceNet inference failed", "error", err)
		return nil
	}

	// Get the first output tensor
	var embedding []float32
	for _, name := range fr.embOutputNames {
		if t, ok := outputs[name]; ok {
			embedding = make([]float32, len(t.Data))
			copy(embedding, t.Data)
			break
		}
	}

	if len(embedding) == 0 {
		return nil
	}

	// L2 normalize
	l2Normalize(embedding)

	return embedding
}

// prepareFaceNetInput converts a 112x112 aligned face to MobileFaceNet input tensor.
// Normalizes: (pixel - 127.5) / 127.5 (range [-1, 1]).
func (fr *FaceRecognizer) prepareFaceNetInput(img *image.RGBA) []float32 {
	if fr.embInputBuf == nil {
		fr.embInputBuf = make([]float32, 3*faceNetInputSize*faceNetInputSize)
	}
	buf := fr.embInputBuf

	channelStride := faceNetInputSize * faceNetInputSize
	bounds := img.Bounds()

	for y := range faceNetInputSize {
		for x := range faceNetInputSize {
			srcIdx := (y-bounds.Min.Y)*img.Stride + (x-bounds.Min.X)*4
			r := (float32(img.Pix[srcIdx+0]) - 127.5) / 127.5
			g := (float32(img.Pix[srcIdx+1]) - 127.5) / 127.5
			b := (float32(img.Pix[srcIdx+2]) - 127.5) / 127.5

			dstIdx := y*faceNetInputSize + x
			buf[0*channelStride+dstIdx] = r
			buf[1*channelStride+dstIdx] = g
			buf[2*channelStride+dstIdx] = b
		}
	}

	return buf
}

// l2Normalize normalizes a vector to unit L2 norm in-place.
func l2Normalize(v []float32) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm < 1e-10 {
		return
	}
	invNorm := float32(1.0 / norm)
	for i := range v {
		v[i] *= invNorm
	}
}

// CosineSimilarity computes the cosine similarity between two L2-normalized vectors.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// cropRegion extracts a sub-image from frame defined by box [x1,y1,x2,y2].
// The result shares pixels with the original where possible.
func cropRegion(frame *image.RGBA, box [4]int) *image.RGBA {
	bounds := frame.Bounds()

	// Clamp box to frame bounds
	x1 := max(box[0], bounds.Min.X)
	y1 := max(box[1], bounds.Min.Y)
	x2 := min(box[2], bounds.Max.X)
	y2 := min(box[3], bounds.Max.Y)

	if x2 <= x1 || y2 <= y1 {
		return image.NewRGBA(image.Rect(0, 0, 0, 0))
	}

	return frame.SubImage(image.Rect(x1, y1, x2, y2)).(*image.RGBA)
}

// saveCrop writes the aligned face crop as a JPEG file.
func (fr *FaceRecognizer) saveCrop(aligned *image.RGBA, dir string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("create face crop dir", "error", err)
		return ""
	}

	filename := fmt.Sprintf("face_%d.jpg", time.Now().UnixNano())
	path := filepath.Join(dir, filename)

	f, err := os.Create(path)
	if err != nil {
		slog.Error("create face crop file", "error", err)
		return ""
	}
	defer f.Close()

	if err := jpeg.Encode(f, aligned, &jpeg.Options{Quality: 90}); err != nil {
		slog.Error("encode face crop JPEG", "error", err)
		os.Remove(path)
		return ""
	}

	return path
}
