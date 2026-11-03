package cry

import (
	"math"
	"math/cmplx"
)

// features matches the official YAMNet frontend: periodic Hann, magnitude
// (not power) STFT, 64 TensorFlow mel bands and natural log with .001 offset.
// Reference waveform padding completes the last patch, never an RTP outage.
func (m *Model) features(pcm []int16) [][]float32 {
	wave := make([]float64, len(pcm)*2)
	for n := range wave {
		var sample float32
		for j, h := range m.manifest.Resample {
			index := n + 20 - j
			if index >= 0 && index%2 == 0 && index/2 < len(pcm) {
				sample += float32(pcm[index/2]) / 32768 * float32(h)
			}
		}
		wave[n] = float64(sample)
	}
	const first = 15600
	hops := (len(wave) - first + 7679) / 7680
	padded := make([]float64, first+hops*7680)
	copy(padded, wave)
	frames := (len(padded)-400)/160 + 1
	mel := make([]float32, frames*64)
	var hann [400]float64
	for i := range hann {
		hann[i] = .5 - .5*math.Cos(2*math.Pi*float64(i)/400)
	}
	for frame := range frames {
		var bins [512]complex128
		for i := range hann {
			bins[i] = complex(padded[frame*160+i]*hann[i], 0)
		}
		fft(bins[:])
		var sums [64]float64
		for bin := 0; bin <= 256; bin++ {
			magnitude := cmplx.Abs(bins[bin])
			for band := range 64 {
				sums[band] += magnitude * float64(m.manifest.Mel[bin*64+band])
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
