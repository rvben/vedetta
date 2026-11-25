package cry

import (
	"math"
	"math/cmplx"
)

// frontend holds what the feature extraction derives once from a bundle's
// weights: the analysis window, and for each spectrum bin the span of mel bands
// it contributes to. A mel filterbank is triangular, so a bin feeds only a few
// neighbouring bands and the rest of its row is zero.
type frontend struct {
	hann   [400]float64
	melLow [257]uint8 // first band with a non-zero weight for the bin
	melEnd [257]uint8 // one past the last such band; equal to melLow when none
}

func newFrontend(meta manifest) frontend {
	var f frontend
	for i := range f.hann {
		f.hann[i] = .5 - .5*math.Cos(2*math.Pi*float64(i)/400)
	}
	for bin := range f.melLow {
		row := meta.Mel[bin*64 : (bin+1)*64]
		for band, weight := range row {
			if weight == 0 {
				continue
			}
			if f.melEnd[bin] == 0 {
				f.melLow[bin] = uint8(band)
			}
			f.melEnd[bin] = uint8(band + 1)
		}
	}
	return f
}

// features matches the official YAMNet frontend: periodic Hann, magnitude
// (not power) STFT, 64 TensorFlow mel bands and natural log with .001 offset.
// Reference waveform padding completes the last patch, never an RTP outage.
func (m *Model) features(pcm []int16) [][]float32 {
	// Upsampling by two interleaves zeros with the samples, so only every other
	// filter tap meets a sample: output n takes the taps j of n's parity, where
	// tap j reads pcm[(n+20-j)/2].
	wave := make([]float64, len(pcm)*2)
	taps := m.manifest.Resample
	for n := range wave {
		var sample float32
		first := n & 1
		if lowest := n + 20 - 2*(len(pcm)-1); lowest > first {
			first = lowest
		}
		last := min(n+20, len(taps)-1)
		for j := first; j <= last; j += 2 {
			sample += float32(pcm[(n+20-j)/2]) / 32768 * float32(taps[j])
		}
		wave[n] = float64(sample)
	}
	const first = 15600
	hops := (len(wave) - first + 7679) / 7680
	padded := make([]float64, first+hops*7680)
	copy(padded, wave)
	frames := (len(padded)-400)/160 + 1
	mel := make([]float32, frames*64)
	f := &m.frontend
	for frame := range frames {
		var bins [512]complex128
		window := padded[frame*160:][:len(f.hann)]
		for i, h := range f.hann {
			bins[i] = complex(window[i]*h, 0)
		}
		fft(bins[:])
		var sums [64]float64
		for bin := 0; bin <= 256; bin++ {
			magnitude := cmplx.Abs(bins[bin])
			weights := m.manifest.Mel[bin*64 : (bin+1)*64]
			for band := int(f.melLow[bin]); band < int(f.melEnd[bin]); band++ {
				sums[band] += magnitude * float64(weights[band])
			}
		}
		for band, v := range sums {
			mel[frame*64+band] = float32(math.Log(v + .001))
		}
	}
	patches := make([][]float32, hops+1)
	for i := range patches {
		patches[i] = mel[i*48*64 : (i*48+96)*64]
	}
	return patches
}

func fft(a []complex128) {
	for i, j := 1, 0; i < len(a); i++ {
		bit := len(a) >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= len(a); size *= 2 {
		step := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for base := 0; base < len(a); base += size {
			w := complex(1, 0)
			for j := 0; j < size/2; j++ {
				u, v := a[base+j], a[base+j+size/2]*w
				a[base+j] = u + v
				a[base+j+size/2] = u - v
				w *= step
			}
		}
	}
}
