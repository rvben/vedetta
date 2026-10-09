package audio

import (
	"math"
	"slices"
	"testing"
)

func TestUpsamplerIndependentOfPacketBoundaries(t *testing.T) {
	pcm := make([]int16, 1001)
	for i := range pcm {
		pcm[i] = int16(12000 * math.Sin(2*math.Pi*1000*float64(i)/8000))
	}
	full := NewUpsampler(6)
	want := append(full.Process(pcm), full.Flush()...)
	fragmented := NewUpsampler(6)
	var got []int16
	for start := 0; start < len(pcm); {
		end := min(start+17, len(pcm))
		got = append(got, fragmented.Process(pcm[start:end])...)
		start = end
	}
	got = append(got, fragmented.Flush()...)
	if len(got) != len(pcm)*6 || !slices.Equal(got, want) {
		t.Fatal("packet boundaries changed interpolated audio or duration")
	}
	if len(fragmented.Flush()) != 0 {
		t.Fatal("flush emitted a second tail")
	}
}

func TestUpsamplerRejectsSpectralImagesAndPreservesSpeech(t *testing.T) {
	pcm := make([]int16, 8000)
	for i := range pcm {
		pcm[i] = int16(10000 * math.Sin(2*math.Pi*3000*float64(i)/8000))
	}
	r := NewUpsampler(6)
	out := append(r.Process(pcm), r.Flush()...)[1000:47000]
	amplitude := func(hz float64) float64 {
		var re, im float64
		for i, v := range out {
			a := 2 * math.Pi * hz * float64(i) / 48000
			re += float64(v) * math.Cos(a)
			im += float64(v) * math.Sin(a)
		}
		return 2 * math.Hypot(re, im) / float64(len(out))
	}
	if amplitude(3000) < 9000 {
		t.Fatal("speech-band tone was attenuated")
	}
	if amplitude(5000)/amplitude(3000) > 0.01 {
		t.Fatal("first interpolation image exceeds -40 dB")
	}
}
