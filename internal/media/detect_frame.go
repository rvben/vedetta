package media

import "image"

// RawFrame holds a decoded frame for detection, at the detection resolution,
// as planar 4:2:0: Data is the luma plane (Width*Height bytes) followed by the
// Cb and Cr planes, each half the width and height, rounded up. Motion
// detection reads the luma plane as it is; RGB24 converts the frame for the
// consumers that need colour, which is far fewer frames.
//
// The receiver owns Data and hands it back with DetectConsumer.Recycle once it
// holds no reference to it, which lets the consumer scale a later frame into
// the same buffer.
type RawFrame struct {
	Data   []byte
	Width  int
	Height int
}

// rawFrameSize returns the length of RawFrame.Data for a frame of w by h.
func rawFrameSize(w, h int) int {
	return w*h + 2*((w+1)/2)*((h+1)/2)
}

// Valid reports whether Data has the length the dimensions call for.
func (f RawFrame) Valid() bool {
	return f.Width > 0 && f.Height > 0 && len(f.Data) == rawFrameSize(f.Width, f.Height)
}

// Luma returns the luma plane, one grey level per pixel. It aliases Data.
func (f RawFrame) Luma() []byte {
	return f.Data[:f.Width*f.Height]
}

// Clone copies the frame into dst when dst has room for it and into a new
// buffer otherwise.
func (f RawFrame) Clone(dst []byte) RawFrame {
	out := sizedPlane(dst, len(f.Data))
	copy(out, f.Data)
	return RawFrame{Data: out, Width: f.Width, Height: f.Height}
}

// RGB24 converts the frame to packed RGB24. It writes into dst when dst has
// room for the frame and allocates otherwise; every byte of the result is
// written, so dst may hold an earlier frame.
func (f RawFrame) RGB24(dst []byte) []byte {
	w, h := f.Width, f.Height
	cw, ch := (w+1)/2, (h+1)/2
	luma := f.Data[:w*h]
	cb := f.Data[w*h : w*h+cw*ch]
	cr := f.Data[w*h+cw*ch : w*h+2*cw*ch]
	rgb := sizedPlane(dst, w*h*3)

	for y := range h {
		row := luma[y*w : (y+1)*w]
		cbRow := cb[(y/2)*cw : (y/2+1)*cw]
		crRow := cr[(y/2)*cw : (y/2+1)*cw]
		out := rgb[y*w*3 : (y+1)*w*3]
		for x, level := range row {
			yy := int(level)
			cbb := int(cbRow[x/2]) - 128
			crr := int(crRow[x/2]) - 128

			out[x*3] = clampByte(yy + ((91881*crr + 32768) >> 16))
			out[x*3+1] = clampByte(yy - ((22554*cbb + 46802*crr + 32768) >> 16))
			out[x*3+2] = clampByte(yy + ((116130*cbb + 32768) >> 16))
		}
	}
	return rgb
}

func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// scaleToRawFrame scales a 4:2:0 picture to w by h by nearest neighbour. It
// writes into dst when dst has room for the frame and allocates otherwise;
// every byte of the result is written.
//
// Each chroma sample is the one the source holds for the top-left pixel of the
// two-by-two block it covers. For a picture that already has the target size
// that is the source's own chroma sample, so RGB24 gives exactly the colours
// of the picture.
func scaleToRawFrame(dst []byte, img *image.YCbCr, w, h int) RawFrame {
	srcW, srcH := img.Rect.Dx(), img.Rect.Dy()
	cw, ch := (w+1)/2, (h+1)/2
	data := sizedPlane(dst, rawFrameSize(w, h))
	luma := data[:w*h]
	cb := data[w*h : w*h+cw*ch]
	cr := data[w*h+cw*ch:]

	for y := range h {
		src := img.Y[(y*srcH/h)*img.YStride:]
		out := luma[y*w : (y+1)*w]
		if srcW == w {
			copy(out, src[:w])
			continue
		}
		for x := range out {
			out[x] = src[x*srcW/w]
		}
	}
	for cy := range ch {
		row := (cy * srcH / h) * img.CStride
		cbOut := cb[cy*cw : (cy+1)*cw]
		crOut := cr[cy*cw : (cy+1)*cw]
		if srcW == w {
			copy(cbOut, img.Cb[row:row+cw])
			copy(crOut, img.Cr[row:row+cw])
			continue
		}
		for cx := range cw {
			col := cx * srcW / w
			cbOut[cx] = img.Cb[row+col]
			crOut[cx] = img.Cr[row+col]
		}
	}
	return RawFrame{Data: data, Width: w, Height: h}
}
