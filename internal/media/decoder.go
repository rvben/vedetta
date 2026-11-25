package media

import (
	"image"
	"strings"
)

// FrameDecoder decodes H.264 NAL streams to YCbCr frames.
// Implementations must be safe for use from a single goroutine.
type FrameDecoder interface {
	// Decode decodes NAL data (Annex B with start codes) and returns a frame or nil.
	Decode(nalData []byte) *image.YCbCr
	// Flush retrieves any buffered frame without feeding new data.
	Flush() *image.YCbCr
	// Close releases decoder resources.
	Close()
}

// reusingFrameDecoder is a FrameDecoder that can write a picture into planes
// the caller owns.
type reusingFrameDecoder interface {
	DecodeInto(nalData []byte, dst *image.YCbCr) *image.YCbCr
}

// decodeReusing decodes nalData for a caller that is finished with dst, the
// picture it got from the previous call. A decoder that supports it overwrites
// dst in place; any other decoder returns a picture of its own. The result is
// nil when no picture was produced, and dst is then still intact.
func decodeReusing(dec FrameDecoder, nalData []byte, dst *image.YCbCr) *image.YCbCr {
	if r, ok := dec.(reusingFrameDecoder); ok {
		return r.DecodeInto(nalData, dst)
	}
	return dec.Decode(nalData)
}

// HWAccel specifies the hardware-decode preference.
type HWAccel string

const (
	HWAccelAuto     HWAccel = "auto"         // default: bundled software (HW measured no benefit for detection)
	HWAccelVT       HWAccel = "videotoolbox" // macOS VideoToolbox (opt-in)
	HWAccelVAAPI    HWAccel = "vaapi"        // Linux Intel/AMD (opt-in, -tags hwaccel)
	HWAccelNVDEC    HWAccel = "nvdec"        // Linux NVIDIA (opt-in, -tags hwaccel)
	HWAccelSoftware HWAccel = "software"     // bundled OpenH264 software decode
)

// ParseHWAccel normalizes a config string into a HWAccel preference. An empty
// string maps to HWAccelAuto. Unknown values also map to HWAccelAuto but report
// ok=false, so callers can warn while still degrading gracefully; config
// validation rejects unknown values before this is reached in production.
//
// vaapi and nvdec are accepted on every build, but their backends are only
// compiled into binaries built with the matching build tag. On a binary without
// that backend the factory finds no hardware decoder and the choice resolves to
// no decoder (explicit backend, no software fallback).
func ParseHWAccel(s string) (pref HWAccel, ok bool) {
	switch HWAccel(strings.ToLower(strings.TrimSpace(s))) {
	case "", HWAccelAuto:
		return HWAccelAuto, true
	case HWAccelVT:
		return HWAccelVT, true
	case HWAccelVAAPI:
		return HWAccelVAAPI, true
	case HWAccelNVDEC:
		return HWAccelNVDEC, true
	case HWAccelSoftware:
		return HWAccelSoftware, true
	default:
		return HWAccelAuto, false
	}
}
