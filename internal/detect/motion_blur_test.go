package detect

import (
	"fmt"
	"testing"
)

// definitionBlur is the 3x3 box blur written straight from its definition:
// every neighbour of every pixel tested against the image bounds.
func definitionBlur(src, dst []uint8, w, h int) {
	for y := range h {
		for x := range w {
			var sum, count int
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= w || ny < 0 || ny >= h {
						continue
					}
					sum += int(src[ny*w+nx])
					count++
				}
			}
			dst[y*w+x] = uint8(sum / count)
		}
	}
}

// The blur treats interior and border pixels separately. Both must give what
// the definition gives at every size, including images too small to have an
// interior and ones where a row or column is entirely border.
func TestBoxBlur3x3MatchesTheDefinition(t *testing.T) {
	sizes := [][2]int{{0, 0}, {2, 0}, {0, 2}, {1, 1}, {1, 7}, {7, 1}, {2, 2}, {2, 9}, {9, 2}, {3, 3}, {3, 8}, {4, 4}, {17, 5}, {64, 48}, {641, 359}}
	for _, size := range sizes {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			src := make([]uint8, w*h)
			state := uint32(w*1000 + h)
			for i := range src {
				state = state*1664525 + 1013904223
				src[i] = uint8(state >> 24)
			}
			// Saturated pixels would overflow a sum held in too narrow a type.
			for i := 0; i < len(src) && i < 3*w; i++ {
				src[i] = 255
			}
			want := make([]uint8, w*h)
			got := make([]uint8, w*h)
			for i := range got {
				got[i] = 7 // a pixel the blur fails to write stays visible
			}
			definitionBlur(src, want, w, h)
			boxBlur3x3(src, got, w, h)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("pixel (%d,%d) is %d, want %d", i%w, i/w, got[i], want[i])
				}
			}
		})
	}
}

func BenchmarkBoxBlur3x3_640x360(b *testing.B) {
	const w, h = 640, 360
	src := make([]uint8, w*h)
	state := uint32(1)
	for i := range src {
		state = state*1664525 + 1013904223
		src[i] = uint8(state >> 24)
	}
	dst := make([]uint8, w*h)
	for b.Loop() {
		boxBlur3x3(src, dst, w, h)
	}
}
