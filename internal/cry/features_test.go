package cry

import (
	"math"
	"math/cmplx"
	"testing"
)

// definitionFeatures is the YAMNet frontend written straight from its
// definition: every resampling tap tested, the window recomputed, and every
// spectrum bin multiplied into every mel band.
func definitionFeatures(meta manifest, pcm []int16) [][]float32 {
	wave := make([]float64, len(pcm)*2)
	for n := range wave {
		var sample float32
		for j, h := range meta.Resample {
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
	for frame := range frames {
		var bins [512]complex128
		for i := range 400 {
			bins[i] = complex(padded[frame*160+i]*(.5-.5*math.Cos(2*math.Pi*float64(i)/400)), 0)
		}
		fft(bins[:])
		var sums [64]float64
		for bin := 0; bin <= 256; bin++ {
			magnitude := cmplx.Abs(bins[bin])
			for band := range 64 {
				sums[band] += magnitude * float64(meta.Mel[bin*64+band])
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

// testManifest builds frontend weights shaped like the real bundle's:
// overlapping triangular mel bands that leave most of each row zero, with the
// lowest and highest bins in no band at all, and a 41-tap resampling filter.
func testManifest() manifest {
	meta := manifest{Mel: make([]float32, 257*64), Resample: make([]float64, 41)}
	for band := range 64 {
		centre := 6 + float64(band)*3.8
		for bin := range 257 {
			if d := math.Abs(float64(bin) - centre); d < 5 {
				meta.Mel[bin*64+band] = float32(1 - d/5)
			}
		}
	}
	// A band that is not contiguous with its neighbours in one row.
	meta.Mel[100*64+2] = 0.25
	for j := range meta.Resample {
		x := float64(j-20) / 2
		meta.Resample[j] = 0.5
		if x != 0 {
			meta.Resample[j] = 0.5 * math.Sin(math.Pi*x) / (math.Pi * x) * (0.54 + 0.46*math.Cos(math.Pi*float64(j-20)/20))
		}
	}
	return meta
}

func testPCM(seed uint32) []int16 {
	pcm := make([]int16, 16000)
	state := seed
	for i := range pcm {
		state = state*1664525 + 1013904223
		tone := 9000 * math.Sin(2*math.Pi*440*float64(i)/8000)
		pcm[i] = int16(tone + float64(int16(state>>16))/4)
	}
	return pcm
}

// The frontend precomputes its window and skips the work a zero weight cannot
// change. The reference fixtures that pin it to YAMNet are not in the
// repository, so this pins it to the definition instead, bit for bit: any
// reordering of the arithmetic shows up here.
func TestFeaturesMatchTheDefinitionExactly(t *testing.T) {
	meta := testManifest()
	m := &Model{manifest: meta, frontend: newFrontend(meta)}
	inputs := map[string][]int16{
		"noisy tone": testPCM(1),
		"silence":    make([]int16, 16000),
		"full scale": func() []int16 {
			pcm := testPCM(2)
			pcm[0], pcm[1], pcm[15999] = math.MinInt16, math.MaxInt16, math.MinInt16
			return pcm
		}(),
	}
	for name, pcm := range inputs {
		t.Run(name, func(t *testing.T) {
			want := definitionFeatures(meta, pcm)
			got := m.features(pcm)
			if len(got) != len(want) {
				t.Fatalf("%d patches, want %d", len(got), len(want))
			}
			distinct := map[float32]bool{}
			for p := range want {
				if len(got[p]) != len(want[p]) {
					t.Fatalf("patch %d has %d values, want %d", p, len(got[p]), len(want[p]))
				}
				for i := range want[p] {
					distinct[want[p][i]] = true
					if math.Float32bits(got[p][i]) != math.Float32bits(want[p][i]) {
						t.Fatalf("patch %d value %d is %g, want %g", p, i, got[p][i], want[p][i])
					}
				}
			}
			if name != "silence" && len(distinct) < 1000 {
				t.Fatalf("reference has only %d distinct values, so agreeing with it proves little", len(distinct))
			}
		})
	}
}
