package aac

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// This file binds the encoder entry points of the Fraunhofer FDK AAC
// library (libfdk-aac) via purego, mirroring the OpenH264 integration: the
// shared library is discovered and loaded once, lazily, and its absence
// degrades gracefully to video-only HLS rather than failing the stream.
//
// Bind encoding, configuration and encoder-info symbols. Recording needs
// aacEncInfo to remove codec priming before muxing its raw AAC-LC frames.

// FDK AACENC_ERROR codes.
const (
	fdkAACEncOK        = 0x0000
	fdkAACEncEncodeEOF = 0x0080
)

// FDK AACENC_PARAM identifiers.
const (
	fdkParamAOT         = 0x0100
	fdkParamBitrate     = 0x0101
	fdkParamSampleRate  = 0x0103
	fdkParamChannelMode = 0x0106
	fdkParamAfterburner = 0x0200
	fdkParamTransmux    = 0x0300
)

// FDK constants: AAC-LC object type, raw transport (no ADTS, fMP4 wants raw
// access units), mono channel mode, and the in/out buffer identifiers.
const (
	fdkAOTAACLC       = 2
	fdkTTMP4Raw       = 0
	fdkChannelModeOne = 1
	fdkInAudioData    = 0
	fdkOutBitstream   = 3

	// 8 kHz mono speech at AAC-LC: 64 kbps is transparent for the band-
	// limited G.711 source. Higher would only spend bits on absent content.
	fdkBitrate = 64000
)

// C struct mirrors. Layout matches the LP64 / little-endian ABI used on the
// arm64 and amd64 targets vedetta builds for.
type fdkBufDesc struct {
	numBufs           int32
	_                 int32 // pad: void** must be 8-byte aligned
	bufs              *unsafe.Pointer
	bufferIdentifiers *int32
	bufSizes          *int32
	bufElSizes        *int32
}

type fdkInArgs struct {
	numInSamples int32
	numAncBytes  int32
}

type fdkOutArgs struct {
	numOutBytes  int32
	numInSamples int32
	numAncBytes  int32
	bitResState  int32
}

type fdkInfo struct {
	_                                 [3]uint32 // maxOutBufBytes, maxAncBytes, inBufFillLevel
	inputChannels, frameLength, delay uint32
	_                                 uint32   // delayCore
	_                                 [64]byte // configBuf
	_                                 uint32   // configSize
}

var (
	fdkMu      sync.Mutex
	fdkAttempt bool
	fdkLoaded  bool
	fdkLoadErr error

	fdkAacEncOpen         func(ph *uintptr, encModules uint32, maxChannels uint32) int32
	fdkAacEncClose        func(ph *uintptr) int32
	fdkAacEncoderSetParam func(h uintptr, param uint32, value uint32) int32
	fdkAacEncEncode       func(h uintptr, inDesc, outDesc, inArgs, outArgs unsafe.Pointer) int32
	fdkAacEncInfo         func(h uintptr, info *fdkInfo) int32

	// Overridable for tests.
	fdkDlopen = func(path string) (uintptr, error) {
		return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	}
	fdkLibPathsFn = fdkLibPaths
)

func fdkLibPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"libfdk-aac.dylib",
			"libfdk-aac.2.dylib",
			"/opt/homebrew/lib/libfdk-aac.dylib",
			"/opt/homebrew/lib/libfdk-aac.2.dylib",
			"/usr/local/lib/libfdk-aac.dylib",
		}
	case "linux":
		return []string{
			"libfdk-aac.so",
			"libfdk-aac.so.2",
			"/usr/lib/libfdk-aac.so.2",
			"/usr/lib/x86_64-linux-gnu/libfdk-aac.so.2",
			"/usr/lib/aarch64-linux-gnu/libfdk-aac.so.2",
			"/usr/local/lib/libfdk-aac.so.2",
		}
	default:
		return []string{"libfdk-aac.so"}
	}
}

// ensureFDK loads libfdk-aac exactly once. Search order: FDKAAC_LIB env →
// platform system paths. Returns whether the library is usable.
func ensureFDK() bool {
	fdkMu.Lock()
	defer fdkMu.Unlock()

	if fdkAttempt {
		return fdkLoaded
	}
	fdkAttempt = true

	var attempts []string
	tryLoad := func(path, source string) bool {
		lib, err := fdkDlopen(path)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", path, err))
			return false
		}
		if err := bindFDK(lib); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", path, err))
			return false
		}
		fdkLoaded = true
		slog.Info("libfdk-aac loaded", "path", path, "source", source)
		slog.Info("AAC audio codec provided by the Fraunhofer FDK AAC library")
		return true
	}

	if envPath := strings.TrimSpace(os.Getenv("FDKAAC_LIB")); envPath != "" {
		if tryLoad(envPath, "environment") {
			return true
		}
	}
	for _, path := range fdkLibPathsFn() {
		if tryLoad(path, "system") {
			return true
		}
	}

	base := "libfdk-aac not found; G.711 cameras cannot encode AAC audio. Set FDKAAC_LIB or install fdk-aac"
	if len(attempts) > 0 {
		fdkLoadErr = fmt.Errorf("%s (attempts: %s)", base, strings.Join(attempts, "; "))
	} else {
		fdkLoadErr = errors.New(base)
	}
	slog.Warn("G.711->AAC transcode unavailable", "error", fdkLoadErr)
	return false
}

func bindFDK(lib uintptr) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("binding libfdk-aac symbols: %v", r)
		}
	}()
	purego.RegisterLibFunc(&fdkAacEncOpen, lib, "aacEncOpen")
	purego.RegisterLibFunc(&fdkAacEncClose, lib, "aacEncClose")
	purego.RegisterLibFunc(&fdkAacEncoderSetParam, lib, "aacEncoder_SetParam")
	purego.RegisterLibFunc(&fdkAacEncEncode, lib, "aacEncEncode")
	purego.RegisterLibFunc(&fdkAacEncInfo, lib, "aacEncInfo")
	return nil
}

// fdkAACEncoder is one libfdk-aac encoder instance. It is not safe for
// concurrent use; the HLS consumer serializes calls under its own lock.
type fdkAACEncoder struct {
	handle  uintptr
	scratch []byte // reusable PCM byte buffer
	out     []byte // reusable bitstream buffer
	delay   int
	drained bool
}

func newFDKAACEncoder(sampleRate, channels int) (*fdkAACEncoder, error) {
	if channels != 1 {
		// Camera G.711 is always mono; refuse anything else rather than
		// emit a misconfigured track.
		return nil, fmt.Errorf("fdk-aac: unsupported channel count %d", channels)
	}
	if !ensureFDK() {
		return nil, fdkLoadErr
	}

	var handle uintptr
	if rc := fdkAacEncOpen(&handle, 0, uint32(channels)); rc != fdkAACEncOK {
		return nil, fmt.Errorf("aacEncOpen failed: 0x%x", rc)
	}

	set := func(param, value uint32) error {
		if rc := fdkAacEncoderSetParam(handle, param, value); rc != fdkAACEncOK {
			return fmt.Errorf("aacEncoder_SetParam(0x%x=%d) failed: 0x%x", param, value, rc)
		}
		return nil
	}
	for _, kv := range []struct{ p, v uint32 }{
		{fdkParamAOT, fdkAOTAACLC},
		{fdkParamSampleRate, uint32(sampleRate)},
		{fdkParamChannelMode, fdkChannelModeOne},
		{fdkParamBitrate, fdkBitrate},
		{fdkParamTransmux, fdkTTMP4Raw},
		{fdkParamAfterburner, 1},
	} {
		if err := set(kv.p, kv.v); err != nil {
			fdkAacEncClose(&handle)
			return nil, err
		}
	}

	// A NULL encode call applies the parameters and validates the config.
	if rc := fdkAacEncEncode(handle, nil, nil, nil, nil); rc != fdkAACEncOK {
		fdkAacEncClose(&handle)
		return nil, fmt.Errorf("aacEncEncode init failed: 0x%x", rc)
	}
	var info fdkInfo
	if rc := fdkAacEncInfo(handle, &info); rc != fdkAACEncOK || info.inputChannels != 1 || info.frameLength != 1024 || info.delay > 16384 {
		fdkAacEncClose(&handle)
		return nil, fmt.Errorf("aacEncInfo: invalid AAC-LC configuration (code=0x%x, frame=%d, delay=%d)", rc, info.frameLength, info.delay)
	}

	return &fdkAACEncoder{
		handle: handle,
		out:    make([]byte, 8192),
		delay:  int(info.delay),
	}, nil
}

func (e *fdkAACEncoder) Encode(pcm []int16) ([][]byte, error) {
	if e.handle == 0 || e.drained {
		return nil, errors.New("AAC encoder is closed or drained")
	}
	if len(pcm) == 0 {
		return nil, nil
	}

	need := len(pcm) * 2
	if cap(e.scratch) < need {
		e.scratch = make([]byte, need)
	}
	e.scratch = e.scratch[:need]
	// INT_PCM is host-endian 16-bit; arm64/amd64 are little-endian.
	for i, s := range pcm {
		u := uint16(s)
		e.scratch[i*2] = byte(u)
		e.scratch[i*2+1] = byte(u >> 8)
	}

	var frames [][]byte
	totalSamples := int32(len(pcm))
	var consumed int32

	for consumed < totalSamples {
		inPtr := unsafe.Pointer(&e.scratch[consumed*2])
		inSize := (totalSamples - consumed) * 2
		inElSize := int32(2)
		inID := int32(fdkInAudioData)
		inBufs := [1]unsafe.Pointer{inPtr}
		inDesc := fdkBufDesc{
			numBufs:           1,
			bufs:              &inBufs[0],
			bufferIdentifiers: &inID,
			bufSizes:          &inSize,
			bufElSizes:        &inElSize,
		}

		outPtr := unsafe.Pointer(&e.out[0])
		outSize := int32(len(e.out))
		outElSize := int32(1)
		outID := int32(fdkOutBitstream)
		outBufs := [1]unsafe.Pointer{outPtr}
		outDesc := fdkBufDesc{
			numBufs:           1,
			bufs:              &outBufs[0],
			bufferIdentifiers: &outID,
			bufSizes:          &outSize,
			bufElSizes:        &outElSize,
		}

		inArgs := fdkInArgs{numInSamples: totalSamples - consumed}
		var outArgs fdkOutArgs

		rc := fdkAacEncEncode(e.handle,
			unsafe.Pointer(&inDesc), unsafe.Pointer(&outDesc),
			unsafe.Pointer(&inArgs), unsafe.Pointer(&outArgs))
		runtime.KeepAlive(e.scratch)
		runtime.KeepAlive(e.out)

		if rc == fdkAACEncEncodeEOF {
			break
		}
		if rc != fdkAACEncOK {
			return frames, fmt.Errorf("aacEncEncode failed: 0x%x", rc)
		}

		if outArgs.numOutBytes < 0 || outArgs.numOutBytes > int32(len(e.out)) {
			return frames, errors.New("AAC encoder returned invalid output size")
		}
		if outArgs.numOutBytes > 0 {
			au := make([]byte, outArgs.numOutBytes)
			copy(au, e.out[:outArgs.numOutBytes])
			frames = append(frames, au)
		}
		if outArgs.numInSamples <= 0 || outArgs.numInSamples > totalSamples-consumed {
			return frames, errors.New("AAC encoder did not consume valid input")
		}
		consumed += outArgs.numInSamples
	}
	return frames, nil
}

func (e *fdkAACEncoder) Delay() int { return e.delay }

// Flush drains buffered PCM and the codec tail. FDK requires numInSamples=-1
// until EOF. Bound the calls so a broken codec cannot spin in the recorder.
func (e *fdkAACEncoder) Flush() ([][]byte, error) {
	if e.handle == 0 {
		return nil, errors.New("AAC encoder is closed")
	}
	if e.drained {
		return nil, nil
	}
	e.drained = true
	var frames [][]byte
	for range 20 {
		outPtr := unsafe.Pointer(&e.out[0])
		outSize, outElSize, outID := int32(len(e.out)), int32(1), int32(fdkOutBitstream)
		outDesc := fdkBufDesc{numBufs: 1, bufs: &outPtr, bufferIdentifiers: &outID, bufSizes: &outSize, bufElSizes: &outElSize}
		inArgs := fdkInArgs{numInSamples: -1}
		var outArgs fdkOutArgs
		rc := fdkAacEncEncode(e.handle, nil, unsafe.Pointer(&outDesc), unsafe.Pointer(&inArgs), unsafe.Pointer(&outArgs))
		runtime.KeepAlive(e.out)
		if rc == fdkAACEncEncodeEOF {
			return frames, nil
		}
		if rc != fdkAACEncOK {
			return frames, fmt.Errorf("flush AAC encoder: 0x%x", rc)
		}
		if outArgs.numOutBytes < 0 || outArgs.numOutBytes > int32(len(e.out)) {
			return frames, errors.New("AAC encoder returned invalid drain output")
		}
		if outArgs.numOutBytes > 0 {
			frames = append(frames, append([]byte(nil), e.out[:outArgs.numOutBytes]...))
		}
	}
	return frames, errors.New("AAC encoder drain did not terminate")
}

func (e *fdkAACEncoder) Close() {
	if e.handle != 0 {
		fdkAacEncClose(&e.handle)
		e.handle = 0
	}
}
