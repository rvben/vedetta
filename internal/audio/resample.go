package audio

import "math"

// Upsampler interpolates mono PCM at an integer ratio with a 64-tap,
// Blackman-windowed low-pass filter. It retains history and 32 input samples
// of lookahead across calls; RTP packet boundaries never become audio edges.
// Flush supplies only the lookahead, preserving exactly N*factor output samples.
type Upsampler struct {
	factor        int
	coefficients  [][64]float64
	history       [64]int16
	input, output int64
	flushed       bool
}

func NewUpsampler(factor int) *Upsampler {
	if factor < 1 || factor > 6 {
		panic("audio upsample factor must be 1..6")
	}
	r := &Upsampler{factor: factor, coefficients: make([][64]float64, factor)}
	for phase := range factor {
		var sum float64
		for tap := range 64 {
			x := float64(tap-31) - float64(phase)/float64(factor)
			v := 0.94
			if x != 0 {
				v = math.Sin(math.Pi*0.94*x) / (math.Pi * x)
			}
			window := 0.42 - 0.5*math.Cos(2*math.Pi*float64(tap)/63) + 0.08*math.Cos(4*math.Pi*float64(tap)/63)
			r.coefficients[phase][tap] = v * window
			sum += v * window
		}
		for tap := range 64 {
			r.coefficients[phase][tap] /= sum
		}
	}
	return r
}

func (r *Upsampler) Process(pcm []int16) []int16 {
	if r.flushed || len(pcm) == 0 {
		return nil
	}
	if r.factor == 1 {
		r.input += int64(len(pcm))
		r.output = r.input
		return append([]int16(nil), pcm...)
	}
	out := make([]int16, 0, len(pcm)*r.factor)
	for _, value := range pcm {
		r.history[r.input%64] = value
		r.input++
		if r.output+32 >= r.input {
			continue
		}
		for phase := range r.factor {
			var value float64
			for tap, coefficient := range r.coefficients[phase] {
				index := r.output + int64(tap) - 31
				if index >= 0 {
					value += float64(r.history[index%64]) * coefficient
				}
			}
			value = math.Round(value)
			value = math.Max(-32768, math.Min(32767, value))
			out = append(out, int16(value))
		}
		r.output++
	}
	return out
}

func (r *Upsampler) Flush() []int16 {
	if r.flushed {
		return nil
	}
	var out []int16
	if r.factor != 1 && r.input > 0 {
		out = r.Process(make([]int16, 32))
	}
	r.flushed = true
	return out
}
