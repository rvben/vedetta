package detect

import "image"

// clampBox confines box [x1,y1,x2,y2] to a frame of w by h pixels whose origin
// is (0,0), and reports whether the result still covers at least one pixel.
func clampBox(box [4]int, w, h int) ([4]int, bool) {
	return clampBoxToRect(box, image.Rect(0, 0, w, h))
}

// clampBoxToRect confines box [x1,y1,x2,y2] to r, in r's own coordinate space,
// and reports whether the result still covers at least one pixel. Consumers
// index frame pixels with these coordinates, so a box is clamped where it is
// built rather than at each use. The far edges are exclusive, matching
// cropRegion, so x2 may equal r.Max.X and y2 may equal r.Max.Y.
//
// A box that clamps away to nothing is not a detection any consumer can use: it
// crops an empty image, draws no overlay and has no area to compare, so callers
// drop it.
func clampBoxToRect(box [4]int, r image.Rectangle) ([4]int, bool) {
	clamped := [4]int{
		min(max(box[0], r.Min.X), r.Max.X),
		min(max(box[1], r.Min.Y), r.Max.Y),
		min(max(box[2], r.Min.X), r.Max.X),
		min(max(box[3], r.Min.Y), r.Max.Y),
	}
	ok := clamped[2] > clamped[0] && clamped[3] > clamped[1]
	return clamped, ok
}
