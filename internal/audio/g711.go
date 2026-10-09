package audio

import (
	"encoding/binary"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
)

// DecodeG711 returns owned PCM16 samples. G.711 has one byte per mono sample.
func DecodeG711(payload []byte, aLaw bool) []int16 {
	var raw []byte
	if aLaw {
		var pcm g711.Alaw
		pcm.Unmarshal(payload)
		raw = pcm
	} else {
		var pcm g711.Mulaw
		pcm.Unmarshal(payload)
		raw = pcm
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.BigEndian.Uint16(raw[i*2 : i*2+2]))
	}
	return pcm
}
