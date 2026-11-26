package media

import (
	"bytes"
	"image"
	"math/rand/v2"
	"testing"
)

// ycbcrToRGB24Scaled converts a picture straight to RGB24 at the target
// resolution, one source pixel per target pixel. It is the reference the
// two-step path (scaleToRawFrame, then RawFrame.RGB24) is held to.
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

// noiseYCbCr returns a 4:2:0 picture of random samples, with strides wider
// than the picture as a decoder's output has.
func noiseYCbCr(w, h int, seed uint64) *image.YCbCr {
	rng := rand.New(rand.NewPCG(seed, 99))
	cw, ch := (w+1)/2, (h+1)/2
	img := &image.YCbCr{
		YStride:        w + 5,
		CStride:        cw + 3,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, w, h),
	}
	img.Y = make([]byte, img.YStride*h)
	img.Cb = make([]byte, img.CStride*ch)
	img.Cr = make([]byte, img.CStride*ch)
	for _, plane := range [][]byte{img.Y, img.Cb, img.Cr} {
		for i := range plane {
			plane[i] = byte(rng.IntN(256))
		}
	}
	return img
}

// A camera whose stream has the detection resolution is the normal case. Its
// frames must reach the detector with exactly the colours a direct conversion
// of the decoded picture gives.
func TestRawFrameRGB24MatchesADirectConversionAtTheSameSize(t *testing.T) {
	for i, size := range [][2]int{{640, 360}, {16, 16}, {2, 2}, {7, 5}, {1, 1}, {6, 3}} {
		w, h := size[0], size[1]
		img := noiseYCbCr(w, h, uint64(i))
		frame := scaleToRawFrame(nil, img, w, h)
		if !frame.Valid() {
			t.Fatalf("%dx%d: the frame's data is %d bytes, which does not fit its size", w, h, len(frame.Data))
		}
		if got, want := frame.RGB24(nil), ycbcrToRGB24Scaled(img, w, h); !bytes.Equal(got, want) {
			t.Fatalf("%dx%d: the frame's colours differ from the picture's", w, h)
		}
		for y := range h {
			if !bytes.Equal(frame.Luma()[y*w:(y+1)*w], img.Y[y*img.YStride:y*img.YStride+w]) {
				t.Fatalf("%dx%d: luma row %d differs from the picture's", w, h, y)
			}
		}
	}
}

// When the stream is scaled, every pixel keeps the luma sample the direct
// conversion picks, and takes its colour from the source pixel of the top-left
// corner of its two-by-two block.
func TestRawFrameRGB24OfAScaledPicture(t *testing.T) {
	for i, c := range []struct{ srcW, srcH, w, h int }{
		{1280, 720, 640, 360}, {640, 480, 640, 360}, {64, 48, 20, 14}, {20, 14, 64, 48}, {33, 17, 9, 7},
	} {
		img := noiseYCbCr(c.srcW, c.srcH, uint64(100+i))
		frame := scaleToRawFrame(nil, img, c.w, c.h)
		if !frame.Valid() {
			t.Fatalf("case %d: the frame's data does not fit its size", i)
		}
		got := frame.RGB24(nil)
		for y := range c.h {
			for x := range c.w {
				sy, sx := y*c.srcH/c.h, x*c.srcW/c.w
				by, bx := (y&^1)*c.srcH/c.h, (x&^1)*c.srcW/c.w
				ci := (by/2)*img.CStride + bx/2
				one := &image.YCbCr{
					Y: []byte{img.Y[sy*img.YStride+sx]}, Cb: []byte{img.Cb[ci]}, Cr: []byte{img.Cr[ci]},
					YStride: 1, CStride: 1, SubsampleRatio: image.YCbCrSubsampleRatio420, Rect: image.Rect(0, 0, 1, 1),
				}
				want := ycbcrToRGB24Scaled(one, 1, 1)
				if o := (y*c.w + x) * 3; !bytes.Equal(got[o:o+3], want) {
					t.Fatalf("case %d pixel (%d,%d): %v, want %v", i, x, y, got[o:o+3], want)
				}
			}
		}
	}
}

func TestRawFrameCloneOwnsItsPixels(t *testing.T) {
	frame := scaleToRawFrame(nil, noiseYCbCr(6, 4, 3), 6, 4)
	want := bytes.Clone(frame.Data)

	spare := make([]byte, len(frame.Data)+8)
	clone := frame.Clone(spare)
	if !samePixels(clone.Data, spare) || !clone.Valid() || !bytes.Equal(clone.Data, want) {
		t.Fatal("a clone into a buffer with room must use it and hold the frame")
	}
	clear(frame.Data)
	if !bytes.Equal(clone.Data, want) {
		t.Fatal("the clone changed with the frame it was made from")
	}
	if small := clone.Clone(make([]byte, 3)); !small.Valid() || !bytes.Equal(small.Data, want) {
		t.Fatal("a clone into a buffer without room must allocate")
	}
}

func BenchmarkScaleToRawFrame(b *testing.B) {
	img := noiseYCbCr(640, 360, 1)
	buf := make([]byte, rawFrameSize(640, 360))
	b.ReportAllocs()
	for b.Loop() {
		scaleToRawFrame(buf, img, 640, 360)
	}
}

func BenchmarkDirectRGB24Conversion(b *testing.B) {
	img := noiseYCbCr(640, 360, 1)
	b.ReportAllocs()
	for b.Loop() {
		ycbcrToRGB24Scaled(img, 640, 360)
	}
}

func BenchmarkRawFrameRGB24(b *testing.B) {
	frame := scaleToRawFrame(nil, noiseYCbCr(640, 360, 1), 640, 360)
	buf := make([]byte, 640*360*3)
	b.ReportAllocs()
	for b.Loop() {
		frame.RGB24(buf)
	}
}

func TestRawFrameValidWantsThePlanesItsSizeCallsFor(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		size int
		want bool
	}{
		{"even", 4, 4, 16 + 2*4, true},
		{"odd, chroma rounded up", 5, 3, 15 + 2*3*2, true},
		{"odd, chroma rounded down", 5, 3, 15 + 2*2*1, false},
		{"packed RGB24", 4, 4, 48, false},
		{"luma only", 4, 4, 16, false},
		{"empty", 0, 0, 0, false},
	}
	for _, tc := range cases {
		f := RawFrame{Data: make([]byte, tc.size), Width: tc.w, Height: tc.h}
		if got := f.Valid(); got != tc.want {
			t.Errorf("%s: Valid() = %v for %dx%d with %d bytes, want %v", tc.name, got, tc.w, tc.h, tc.size, tc.want)
		}
	}
}
