package media

import (
	"unsafe"
)

// Layout of the OpenH264 structs the encoder call passes across the ABI
// boundary, mirrored from codec/api/wels/codec_app_def.h at v2.6.0.
//
// WHY THESE ARE DECLARED HERE AND NOT TAKEN FROM THE BINDING.
//
// github.com/y9o/go-openh264 v0.2.0 declares SLayerBSInfo without the
// trailing `float rPsnr[3]`, so its struct is 40 bytes where the library
// uses 56, and SSourcePicture without the trailing three bools, 72 where
// the library uses 80. SFrameBSInfo embeds 128 layer entries, so the
// missing 16 bytes per entry compound: 5144 bytes declared against 7192
// written. Handing EncodeFrame a buffer of the binding's size let the
// library write 2038 bytes past the end of a Go allocation on every
// recompressed frame, put the layer array on the wrong stride so only
// layer 0 could be read, and put eFrameType 2048 bytes away from where
// it was read, so it always came back zero.
//
// The types below are never allocated. They exist so the compiler derives
// the offsets and padding from field declarations that can be read line by
// line against the header, instead of from hand-counted literals. They are
// projected onto the byte buffers transcodeFile owns.
//
// Verified against the library three ways: clang sizeof/offsetof on the
// v2.6.0 header, a runtime canary that watches where EncodeFrame writes
// (TestEncodeFrameWritesNothingPastDeclaredBufferSizes), and a read-back
// of the frame trailer (TestFrameBSInfoTrailerIsReadableAtDeclaredOffset).
// If OpenH264 ever changes this layout, those two tests fail rather than
// the recorder corrupting its own heap.

// maxEncoderLayers is MAX_LAYER_NUM_OF_FRAME: the fixed capacity of the
// layer array inside SFrameBSInfo.
const maxEncoderLayers = 128

// cLayerBSInfo mirrors SLayerBSInfo.
//
// pNalLengthInByte and pBsBuf are declared as uintptr rather than as Go
// pointer types on purpose. They hold addresses OpenH264 allocated, valid
// only until the next EncodeFrame or destroy call. A Go pointer type there
// would make the collector run span lookups against foreign memory.
type cLayerBSInfo struct {
	uiTemporalId     uint8
	uiSpatialId      uint8
	uiQualityId      uint8
	eFrameType       uint32
	uiLayerType      uint8
	iSubSeqId        int32
	iNalCount        int32
	pNalLengthInByte uintptr
	pBsBuf           uintptr
	rPsnr            [3]float32
}

// cFrameBSInfo mirrors SFrameBSInfo, the encoder's per-frame output.
type cFrameBSInfo struct {
	iLayerNum         int32
	sLayerInfo        [maxEncoderLayers]cLayerBSInfo
	eFrameType        uint32
	iFrameSizeInBytes int32
	uiTimeStamp       int64
}

// cSourcePicture mirrors SSourcePicture, the encoder's per-frame input.
//
// pData points at Go-owned plane data that the caller pins for the
// duration of the call, so Go pointer types are correct here.
//
// The three trailing bools ask the encoder to compute per-plane PSNR. We
// never set them, but they must be part of the allocation: without them
// the library reads three bytes of whatever Go memory follows, and a
// non-zero byte there turns on PSNR computation.
type cSourcePicture struct {
	iColorFormat int32
	iStride      [4]int32
	pData        [4]*uint8
	iPicWidth    int32
	iPicHeight   int32
	uiTimeStamp  int64
	bPsnrY       bool
	bPsnrU       bool
	bPsnrV       bool
}

const (
	sourcePictureSize = int(unsafe.Sizeof(cSourcePicture{}))
	frameBSInfoSize   = int(unsafe.Sizeof(cFrameBSInfo{}))
)

// asFrameBSInfo projects an encoder output buffer onto the frame layout.
// The buffer must be at least frameBSInfoSize bytes.
func asFrameBSInfo(buf []byte) *cFrameBSInfo {
	_ = buf[frameBSInfoSize-1] // bounds check the projection, not each field
	return (*cFrameBSInfo)(unsafe.Pointer(&buf[0]))
}

// asSourcePicture projects an encoder input buffer onto the picture layout.
// The buffer must be at least sourcePictureSize bytes.
func asSourcePicture(buf []byte) *cSourcePicture {
	_ = buf[sourcePictureSize-1]
	return (*cSourcePicture)(unsafe.Pointer(&buf[0]))
}

// layerCount returns iLayerNum clamped to the array's real capacity, so a
// garbled value cannot walk the read off the end of the buffer.
func (f *cFrameBSInfo) layerCount() int {
	n := int(f.iLayerNum)
	if n < 0 {
		return 0
	}
	if n > maxEncoderLayers {
		return maxEncoderLayers
	}
	return n
}
