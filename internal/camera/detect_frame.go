package camera

// detectFrame is one frame on its way through the detection goroutine. Motion
// detection needs only the grey levels, so the RGB24 pixels are produced the
// first time something asks for them: object detection, an event snapshot,
// face recognition or the doorbell. Most frames never do.
type detectFrame struct {
	w, h int
	rgb  []byte
	// convert produces the RGB24 pixels. It is nil once they exist and for a
	// frame built from RGB24 pixels.
	convert func() []byte
}

// rgbFrame returns a frame over RGB24 pixels that already exist.
func rgbFrame(rgb []byte, w, h int) *detectFrame {
	return &detectFrame{w: w, h: h, rgb: rgb}
}

// RGB24 returns the frame's packed RGB24 pixels. They are valid until the
// detection goroutine takes the next frame; a caller that keeps them copies.
func (f *detectFrame) RGB24() []byte {
	if f.convert != nil {
		f.rgb = f.convert()
		f.convert = nil
	}
	return f.rgb
}
