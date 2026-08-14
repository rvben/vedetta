package media

import (
	"syscall"
	"testing"
	"unsafe"

	openh264 "github.com/y9o/go-openh264"
)

func rawOutputWith(planes [3][]byte, status, w, h, yStride, cStride int32) rawDecoderOutput {
	var output rawDecoderOutput
	for i := range planes {
		output.planes[i] = uintptr(unsafe.Pointer(unsafe.SliceData(planes[i])))
	}
	info := output.info()
	info.IBufferStatus = status
	sysBuf := info.UsrData_sSystemBuffer()
	sysBuf.IWidth = w
	sysBuf.IHeight = h
	sysBuf.IStride[0] = yStride
	sysBuf.IStride[1] = cStride
	return output
}

// offHeapPlanes returns three slices carved out of a single anonymous mmap
// region, mimicking the libopenh264-owned (off-heap, non-Go-managed) plane
// buffers that frameFromDecoded copies from in production. Using off-heap
// memory keeps the test faithful and lets it run under -d=checkptr: the
// uintptr round-trip in frameFromDecoded is only flagged when it lands on a
// Go-heap object, which a real decoder buffer never is. The region is
// unmapped on test cleanup.
func offHeapPlanes(t *testing.T, yLen, cLen int) [3][]byte {
	t.Helper()
	region, err := syscall.Mmap(-1, 0, yLen+2*cLen,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Munmap(region) })
	return [3][]byte{region[:yLen], region[yLen : yLen+cLen], region[yLen+cLen : yLen+2*cLen]}
}

// bufInfoWith builds an SBufferInfo whose reported system-buffer geometry is
// exactly w/h/yStride/cStride, mirroring what OpenH264 fills in on a decoded
// frame.
func bufInfoWith(w, h, yStride, cStride int32) *openh264.SBufferInfo {
	bi := &openh264.SBufferInfo{}
	sb := bi.UsrData_sSystemBuffer()
	sb.IWidth = w
	sb.IHeight = h
	sb.IStride[0] = yStride
	sb.IStride[1] = cStride
	return bi
}

// A well-formed frame must round-trip: the helper copies the decoder's planes
// into Go-owned buffers and reports the geometry verbatim.
func TestFrameFromDecodedCopiesValidFrame(t *testing.T) {
	const w, h, yStride, cStride = 4, 4, 4, 2
	yLen := yStride * h
	cLen := cStride * (h / 2)

	// Off-heap planes mirror the libopenh264-owned buffers; keep the Y backing
	// slice separately since frameFromDecoded clears *dst, so the aliasing
	// check below must mutate the original buffer, not dst[0].
	dst := offHeapPlanes(t, yLen, cLen)
	yBacking := dst[0]
	for i := range yBacking {
		yBacking[i] = byte(i + 1)
	}

	img := frameFromDecoded(&dst, bufInfoWith(w, h, yStride, cStride))
	if img == nil {
		t.Fatal("frameFromDecoded returned nil for a valid frame")
	}
	if img.Rect.Dx() != w || img.Rect.Dy() != h {
		t.Fatalf("rect = %v, want %dx%d", img.Rect, w, h)
	}
	if img.YStride != yStride || img.CStride != cStride {
		t.Fatalf("strides = %d/%d, want %d/%d", img.YStride, img.CStride, yStride, cStride)
	}
	if len(img.Y) != yLen || img.Y[0] != 1 || img.Y[yLen-1] != byte(yLen) {
		t.Fatalf("Y plane not copied correctly: len=%d first=%d last=%d", len(img.Y), img.Y[0], img.Y[len(img.Y)-1])
	}
	// Must be a Go-owned copy, not an alias of the cgo-backed input.
	yBacking[0] = 0xFF
	if img.Y[0] == 0xFF {
		t.Fatal("Y plane aliases the decoder buffer; mutation leaked into the returned frame")
	}
}

// The production decode path receives plane addresses through a pointer-free
// rawDecoderOutput so a concurrent GC never scans OpenH264-owned addresses.
// Verify that the raw representation still produces a detached Go image.
func TestFrameFromRawDecodedCopiesValidFrame(t *testing.T) {
	const w, h, yStride, cStride = 4, 4, 4, 2
	planes := offHeapPlanes(t, yStride*h, cStride*(h/2))
	for i := range planes[0] {
		planes[0][i] = byte(i + 1)
	}
	output := rawOutputWith(planes, 1, w, h, yStride, cStride)

	img := frameFromRawDecoded(&output)
	if img == nil {
		t.Fatal("frameFromRawDecoded returned nil for a valid frame")
	}
	if img.Rect.Dx() != w || img.Rect.Dy() != h || img.Y[0] != 1 {
		t.Fatalf("unexpected decoded frame: rect=%v firstY=%d", img.Rect, img.Y[0])
	}
	planes[0][0] = 0xFF
	if img.Y[0] == 0xFF {
		t.Fatal("raw decoded frame aliases the OpenH264-owned plane")
	}
}

func TestFrameFromRawDecodedRejectsMissingFrame(t *testing.T) {
	output := rawOutputWith([3][]byte{}, 0, 4, 4, 4, 2)
	if img := frameFromRawDecoded(&output); img != nil {
		t.Fatalf("expected nil without a decoded frame, got %v", img.Rect)
	}
}

// The corruption-prevention case: OpenH264 can report a geometry whose
// stride*height exceeds the plane buffer it actually handed back (a
// transitional/corrupt frame, e.g. mid-stream resolution change with error
// concealment active). The old code did dst[0][:yStride*h] + copy, reading
// far past the cgo-owned buffer into foreign memory. The helper MUST reject
// the frame instead.
func TestFrameFromDecodedRejectsGeometryLargerThanBuffer(t *testing.T) {
	// Buffers sized for a 4x4 frame, but the decoder reports 64x64.
	dst := [3][]byte{make([]byte, 16), make([]byte, 4), make([]byte, 4)}

	if img := frameFromDecoded(&dst, bufInfoWith(64, 64, 64, 32)); img != nil {
		t.Fatalf("expected nil for geometry exceeding buffer length, got %v", img.Rect)
	}
}

// Degenerate or garbage geometry (zero/negative dimensions, stride narrower
// than the frame) must be rejected before it can produce an image.YCbCr that
// out-of-bounds-indexes in a pure-Go consumer.
func TestFrameFromDecodedRejectsDegenerateGeometry(t *testing.T) {
	cases := []struct {
		name                   string
		w, h, yStride, cStride int32
	}{
		{"zero width", 0, 4, 4, 2},
		{"zero height", 4, 0, 4, 2},
		{"negative height", 4, -4, 4, 2},
		{"y stride narrower than width", 8, 4, 4, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Fresh non-nil planes per case: frameFromDecoded clears *dst, and
			// we want the geometry check to reject, not the nil-plane check.
			big := [3][]byte{make([]byte, 1<<20), make([]byte, 1<<20), make([]byte, 1<<20)}
			if img := frameFromDecoded(&big, bufInfoWith(c.w, c.h, c.yStride, c.cStride)); img != nil {
				t.Fatalf("expected nil for %s, got %v", c.name, img.Rect)
			}
		})
	}
}

// A nil plane (OpenH264 produced no frame) is not an error - it just means
// "need more data"; the helper returns nil without touching the slices.
func TestFrameFromDecodedNilPlane(t *testing.T) {
	dst := [3][]byte{nil, nil, nil}
	if img := frameFromDecoded(&dst, bufInfoWith(4, 4, 4, 2)); img != nil {
		t.Fatal("expected nil when no plane was produced")
	}
}
