package media

import (
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	openh264 "github.com/y9o/go-openh264"
)

var (
	openh264StateMu       sync.Mutex
	openh264Attempt       bool
	openh264Loaded        bool
	openh264LoadErr       error
	openh264Source        string
	openh264Path          string
	openh264LoadedVersion string

	openh264LoadLibrary  = openh264.Open
	openh264CloseLibrary = openh264.Close
	openh264CodecVersion = func() string {
		ver := openh264.WelsGetCodecVersion()
		return fmt.Sprintf("%d.%d.%d", ver.UMajor, ver.UMinor, ver.URevision)
	}
	openh264LibPathsFn = openH264LibPaths

	// openh264Mu serializes all OpenH264 C library calls. The purego
	// bindings use dlopen'd function pointers that share global state,
	// making concurrent calls from multiple goroutines unsafe.
	openh264Mu sync.Mutex
)

// openH264LibPaths returns candidate paths for the OpenH264 shared library.
func openH264LibPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"libopenh264.dylib",
			"/opt/homebrew/lib/libopenh264.dylib",
			"/usr/local/lib/libopenh264.dylib",
		}
	case "linux":
		return []string{
			"libopenh264.so",
			"/usr/lib/libopenh264.so",
			"/usr/lib/x86_64-linux-gnu/libopenh264.so",
			"/usr/lib/aarch64-linux-gnu/libopenh264.so",
			"/usr/local/lib/libopenh264.so",
		}
	default:
		return []string{"libopenh264.so"}
	}
}

// tryLoadOpenH264 attempts to load the library from a given path.
func tryLoadOpenH264(path, source string) error {
	if err := openh264LoadLibrary(path); err != nil {
		return err
	}

	version := openh264CodecVersion()
	slog.Info("OpenH264 loaded", "path", path, "source", source, "version", version)
	slog.Info("OpenH264 Video Codec provided by Cisco Systems, Inc.")

	openh264Loaded = true
	openh264LoadErr = nil
	openh264Source = source
	openh264Path = path
	openh264LoadedVersion = version
	return nil
}

// ensureOpenH264 tries to load the OpenH264 library once.
// Search order: OPENH264_LIB env → system paths → verified Vedetta install.
func ensureOpenH264() bool {
	openh264StateMu.Lock()
	defer openh264StateMu.Unlock()

	if openh264Attempt {
		return openh264Loaded
	}
	openh264Attempt = true

	var attempts []string
	recordFailure := func(label string, err error) {
		if err == nil {
			return
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", label, err))
	}

	if envPath := strings.TrimSpace(os.Getenv("OPENH264_LIB")); envPath != "" {
		if err := tryLoadOpenH264(envPath, "environment"); err == nil {
			return true
		} else {
			recordFailure("OPENH264_LIB", fmt.Errorf("failed to load %q: %w", envPath, err))
		}
	}

	for _, path := range openh264LibPathsFn() {
		if err := tryLoadOpenH264(path, "system"); err == nil {
			return true
		} else {
			recordFailure(path, err)
		}
	}

	if installedPath, installed, err := verifiedInstalledOpenH264Path(); err != nil {
		recordFailure("installed cache", err)
	} else if installed {
		if err := tryLoadOpenH264(installedPath, "installed"); err == nil {
			return true
		} else {
			recordFailure("installed cache", fmt.Errorf("failed to load %q: %w", installedPath, err))
		}
	}

	baseErr := "OpenH264 shared library not found; set OPENH264_LIB, install libopenh264 via the system package manager, or install it from the setup/system page"
	if len(attempts) > 0 {
		openh264LoadErr = fmt.Errorf("%s (attempts: %s)", baseErr, strings.Join(attempts, "; "))
	} else {
		openh264LoadErr = errors.New(baseErr)
	}
	slog.Warn("H264 decode unavailable — detection disabled", "error", openh264LoadErr)
	return openh264Loaded
}

func openH264StateSnapshot() (loaded bool, source, path, version string, loadErr error) {
	openh264StateMu.Lock()
	defer openh264StateMu.Unlock()
	return openh264Loaded, openh264Source, openh264Path, openh264LoadedVersion, openh264LoadErr
}

func resetOpenH264State() {
	openh264StateMu.Lock()
	defer openh264StateMu.Unlock()

	_ = openh264CloseLibrary()
	openh264Attempt = false
	openh264Loaded = false
	openh264LoadErr = nil
	openh264Source = ""
	openh264Path = ""
	openh264LoadedVersion = ""
}

// H264Decoder wraps OpenH264 for decoding H264 NAL units to YCbCr images.
type H264Decoder struct {
	decoder *openh264.ISVCDecoder
}

// NewH264Decoder creates a new H264 decoder. Returns nil if OpenH264 isn't available.
func NewH264Decoder() *H264Decoder {
	if !ensureOpenH264() {
		return nil
	}

	openh264Mu.Lock()
	defer openh264Mu.Unlock()

	var dec *openh264.ISVCDecoder
	if ret := openh264.WelsCreateDecoder(&dec); ret != 0 || dec == nil {
		slog.Error("WelsCreateDecoder failed", "ret", ret)
		return nil
	}

	param := openh264.SDecodingParam{}
	param.EEcActiveIdc = openh264.ERROR_CON_SLICE_MV_COPY_CROSS_IDR_FREEZE_RES_CHANGE
	if ret := dec.Initialize(&param); ret != 0 {
		slog.Error("OpenH264 Initialize failed", "ret", ret)
		openh264.WelsDestroyDecoder(dec)
		return nil
	}

	return &H264Decoder{decoder: dec}
}

// Decode decodes H264 NAL units (with start codes) and returns YCbCr 4:2:0 image.
// Returns nil if no frame was produced (need more data).
//
// Separate decoder instances are thread-safe — only create/destroy and
// encoder operations need the global mutex.
func (d *H264Decoder) Decode(nalData []byte) *image.YCbCr {
	if d == nil || d.decoder == nil || len(nalData) == 0 {
		return nil
	}

	var output rawDecoderOutput
	vtbl := *(**openh264.ISVCDecoderVtbl)(unsafe.Pointer(d.decoder))
	ret, _, _ := purego.SyscallN(
		uintptr(unsafe.Pointer(vtbl.DecodeFrameNoDelay)),
		uintptr(unsafe.Pointer(d.decoder)),
		uintptr(unsafe.Pointer(unsafe.SliceData(nalData))),
		uintptr(len(nalData)),
		uintptr(unsafe.Pointer(&output.planes[0])),
		uintptr(unsafe.Pointer(&output.bufferInfo[0])),
	)
	runtime.KeepAlive(nalData)
	runtime.KeepAlive(d.decoder)
	if ret != 0 {
		return nil
	}

	return frameFromRawDecoded(&output)
}

// rawDecoderOutput is deliberately pointer-free. OpenH264 writes three
// library-owned plane addresses and an SBufferInfo containing three more
// library-owned addresses during every decode call. The upstream purego
// wrapper represents those values as Go pointers and slices on the Go stack;
// a concurrent GC can then mistake a foreign address for a Go heap pointer
// and terminate the process while scanning it. uintptr and byte arrays carry
// no GC pointer metadata, so the addresses remain opaque until their bytes are
// synchronously copied into Go-owned image buffers.
type rawDecoderOutput struct {
	planes     [3]uintptr
	bufferInfo [unsafe.Sizeof(openh264.SBufferInfo{})]byte
}

func (o *rawDecoderOutput) info() *openh264.SBufferInfo {
	return (*openh264.SBufferInfo)(unsafe.Pointer(&o.bufferInfo[0]))
}

func frameFromRawDecoded(output *rawDecoderOutput) *image.YCbCr {
	if output == nil {
		return nil
	}
	bufInfo := output.info()
	if bufInfo.IBufferStatus != 1 {
		return nil
	}

	sysBuf := bufInfo.UsrData_sSystemBuffer()
	w := int(sysBuf.IWidth)
	h := int(sysBuf.IHeight)
	yStride := int(sysBuf.IStride[0])
	cStride := int(sysBuf.IStride[1])
	if !validDecodedGeometry(w, h, yStride, cStride) {
		return nil
	}

	yLen := yStride * h
	cLen := cStride * (h / 2)
	return copyDecodedFrame(output.planes, [3]int{yLen, cLen, cLen}, w, h, yStride, cStride)
}

// frameFromDecoded copies OpenH264's decoded planes into Go-owned buffers,
// validating the decoder-reported geometry against the actual length of the
// cgo-backed plane slices first.
//
// GC safety: the dst slice headers point into memory owned by OpenH264 (the
// library mallocs the plane buffers even in the purego/dlopen build). Holding
// such a foreign address in a GC-traced slice header lets Go's concurrent GC
// run span lookups on it; if the address collides with a Go heap arena, the GC
// mis-marks a real object and corrupts the heap. So the first thing we do is
// lift each plane to an opaque (uintptr, len) pair and clear *dst, after which
// no foreign pointer is live in any GC-scanned word across the allocations
// below. The bytes are read back through a slice materialized only as the
// immediate argument to copy (lowered to a non-preemptible memmove), so the
// transient foreign pointer is never observable at a GC safepoint.
//
// OpenH264 can hand back a frame whose reported stride*height exceeds the
// plane buffer it actually allocated - a transitional or corrupt frame, e.g.
// a mid-stream resolution change while error concealment is active. Slicing
// past the decoder-owned buffer and copying it then reads far into foreign
// memory across the cgo boundary; that read can fault and, worse, the bogus
// geometry propagates into a *image.YCbCr that out-of-bounds-indexes in
// pure-Go consumers. Rejecting such a frame (return nil = "no frame, need more
// data") is always safe; a real dropped frame is invisible next to a heap
// fault.
func frameFromDecoded(dst *[3][]byte, bufInfo *openh264.SBufferInfo) *image.YCbCr {
	var (
		planePtr [3]uintptr
		planeLen [3]int
	)
	for i := range dst {
		planePtr[i] = uintptr(unsafe.Pointer(unsafe.SliceData(dst[i]))) //nolint:govet // address held as uintptr so the GC never traces library-owned memory
		planeLen[i] = len(dst[i])
	}
	// Drop the C-backed slice headers from the GC-scanned frame before any
	// allocation below can trigger a garbage collection.
	*dst = [3][]byte{}

	if planePtr[0] == 0 || planePtr[1] == 0 || planePtr[2] == 0 {
		return nil
	}

	sysBuf := bufInfo.UsrData_sSystemBuffer()
	w := int(sysBuf.IWidth)
	h := int(sysBuf.IHeight)
	yStride := int(sysBuf.IStride[0])
	cStride := int(sysBuf.IStride[1])

	if !validDecodedGeometry(w, h, yStride, cStride) {
		return nil
	}

	yLen := yStride * h
	cLen := cStride * (h / 2)

	// The reported planes must fit within the buffers OpenH264 returned.
	if yLen <= 0 || cLen <= 0 ||
		yLen > planeLen[0] || cLen > planeLen[1] || cLen > planeLen[2] {
		return nil
	}
	return copyDecodedFrame(planePtr, planeLen, w, h, yStride, cStride)
}

func validDecodedGeometry(w, h, yStride, cStride int) bool {
	const maxDecodedDimension = 16_384
	return w > 0 && h > 0 && w <= maxDecodedDimension && h <= maxDecodedDimension &&
		h%2 == 0 && yStride >= w && yStride <= maxDecodedDimension &&
		cStride*2 >= w && cStride <= maxDecodedDimension
}

func copyDecodedFrame(planePtr [3]uintptr, planeLen [3]int, w, h, yStride, cStride int) *image.YCbCr {
	yLen := yStride * h
	cLen := cStride * (h / 2)
	if planePtr[0] == 0 || planePtr[1] == 0 || planePtr[2] == 0 ||
		yLen <= 0 || cLen <= 0 ||
		yLen > planeLen[0] || cLen > planeLen[1] || cLen > planeLen[2] {
		return nil
	}

	y := make([]byte, yLen)
	cb := make([]byte, cLen)
	cr := make([]byte, cLen)

	copyFromCPlane(y, planePtr[0], yLen)
	copyFromCPlane(cb, planePtr[1], cLen)
	copyFromCPlane(cr, planePtr[2], cLen)

	return &image.YCbCr{
		Y:              y,
		Cb:             cb,
		Cr:             cr,
		YStride:        yStride,
		CStride:        cStride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, w, h),
	}
}

// copyFromCPlane copies n bytes from a library-owned buffer (addressed by a
// uintptr, never a GC-traced pointer) into a Go-owned slice. The source slice
// header is materialized only as the immediate argument to copy, which lowers
// to a non-preemptible memmove, so the foreign address is never live at a GC
// safepoint and never stored where the GC could trace it.
func copyFromCPlane(dst []byte, src uintptr, n int) {
	copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(src)), n)) //nolint:govet // address held as uintptr; the slice is transient and consumed by memmove
}

// Flush retrieves any buffered frame from the decoder without feeding new data.
func (d *H264Decoder) Flush() *image.YCbCr {
	if d == nil || d.decoder == nil {
		return nil
	}

	var output rawDecoderOutput
	vtbl := *(**openh264.ISVCDecoderVtbl)(unsafe.Pointer(d.decoder))
	ret, _, _ := purego.SyscallN(
		uintptr(unsafe.Pointer(vtbl.FlushFrame)),
		uintptr(unsafe.Pointer(d.decoder)),
		uintptr(unsafe.Pointer(&output.planes[0])),
		uintptr(unsafe.Pointer(&output.bufferInfo[0])),
	)
	runtime.KeepAlive(d.decoder)
	if ret != 0 {
		return nil
	}

	return frameFromRawDecoded(&output)
}

// Close releases the decoder resources.
func (d *H264Decoder) Close() {
	if d != nil && d.decoder != nil {
		openh264Mu.Lock()
		defer openh264Mu.Unlock()
		d.decoder.Uninitialize()
		openh264.WelsDestroyDecoder(d.decoder)
		d.decoder = nil
	}
}

// OpenH264Available returns whether the OpenH264 library was loaded.
func OpenH264Available() bool {
	return ensureOpenH264()
}

// OpenH264Lock acquires the global OpenH264 mutex. Callers that use the
// encoder (or any other OpenH264 API not wrapped by H264Decoder) must
// hold this lock for the duration of each C library call.
func OpenH264Lock()   { openh264Mu.Lock() }
func OpenH264Unlock() { openh264Mu.Unlock() }

// ycbcrToRGB24Scaled converts a YCbCr image to RGB24 at the target resolution.
func ycbcrToRGB24Scaled(img *image.YCbCr, targetW, targetH int) []byte {
	srcW := img.Rect.Dx()
	srcH := img.Rect.Dy()
	rgb := make([]byte, targetW*targetH*3)

	for dy := range targetH {
		sy := dy * srcH / targetH
		for dx := range targetW {
			sx := dx * srcW / targetW

			yi := sy*img.YStride + sx
			ci := (sy/2)*img.CStride + (sx / 2)

			yy := int(img.Y[yi])
			cbb := int(img.Cb[ci]) - 128
			crr := int(img.Cr[ci]) - 128

			r := yy + ((91881*crr + 32768) >> 16)
			g := yy - ((22554*cbb + 46802*crr + 32768) >> 16)
			b := yy + ((116130*cbb + 32768) >> 16)

			if r < 0 {
				r = 0
			} else if r > 255 {
				r = 255
			}
			if g < 0 {
				g = 0
			} else if g > 255 {
				g = 255
			}
			if b < 0 {
				b = 0
			} else if b > 255 {
				b = 255
			}

			di := (dy*targetW + dx) * 3
			rgb[di] = byte(r)
			rgb[di+1] = byte(g)
			rgb[di+2] = byte(b)
		}
	}

	return rgb
}
