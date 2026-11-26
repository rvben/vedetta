package camera

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
)

// colourFrame returns a planar frame whose luma and chroma all differ from
// mid-grey, so its RGB24 pixels are distinguishable from those of any other
// seed and from a zeroed buffer.
func colourFrame(w, h int, seed byte) media.RawFrame {
	f := media.NewTestRawFrame(w, h, 0)
	for i := range f.Data {
		f.Data[i] = seed + byte(i*7)
	}
	return f
}

func lazyTestCamera() *Camera {
	cam := newTestCamera(config.CameraConfig{
		Name:   "lazy",
		Detect: config.DetectStreamConfig{Width: 16, Height: 16, FPS: 5},
	}, nil)
	// No region qualifies, so the detection goroutine has no use for RGB.
	cam.motionMinRegionScore = 1e9
	return cam
}

func readRGB(t *testing.T, cam *Camera, w, h int) []byte {
	t.Helper()
	dst := make([]byte, w*h*3)
	gw, gh, ok := cam.SnapshotRGB24(dst)
	if !ok || gw != w || gh != h {
		t.Fatalf("SnapshotRGB24 = %dx%d ok=%v, want %dx%d", gw, gh, ok, w, h)
	}
	return dst
}

func TestProcessFrameConvertsNothingUntilAReaderAsks(t *testing.T) {
	cam := lazyTestCamera()
	raw := colourFrame(16, 16, 3)
	cam.processFrame(raw)

	if cam.rawFrame != nil || cam.detectRGB != nil {
		t.Fatalf("a frame without motion was converted to RGB: reader copy %d bytes, detection copy %d bytes",
			len(cam.rawFrame), len(cam.detectRGB))
	}
	if got, want := readRGB(t, cam, 16, 16), raw.RGB24(nil); !bytes.Equal(got, want) {
		t.Fatal("the frame a reader got is not the conversion of the frame that was processed")
	}
}

func TestReadersSeeTheNewestFrameAfterAnEarlierRead(t *testing.T) {
	cam := lazyTestCamera()
	first, second := colourFrame(16, 16, 3), colourFrame(16, 16, 90)
	cam.processFrame(first)
	if got := readRGB(t, cam, 16, 16); !bytes.Equal(got, first.RGB24(nil)) {
		t.Fatal("setup: the first read is not the first frame")
	}
	cam.processFrame(second)
	if got := readRGB(t, cam, 16, 16); !bytes.Equal(got, second.RGB24(nil)) {
		t.Fatal("a read after a newer frame returned the pixels converted for the older one")
	}
	if img := cam.LastSnapshot(); img == nil || img.Bounds().Dx() != 16 {
		t.Fatalf("LastSnapshot = %v, want a 16 pixel wide picture", img)
	}
}

func TestProcessFrameKeepsItsOwnCopyOfTheFrame(t *testing.T) {
	cam := lazyTestCamera()
	raw := colourFrame(16, 16, 3)
	want := raw.RGB24(nil)
	cam.processFrame(raw)
	// The consumer recycles the buffer for the next decoded picture.
	for i := range raw.Data {
		raw.Data[i] = 0
	}
	if got := readRGB(t, cam, 16, 16); !bytes.Equal(got, want) {
		t.Fatal("a read after the frame's buffer was reused shows the reused pixels")
	}
}

func TestDetectionConversionIsSharedWithReaders(t *testing.T) {
	cam := lazyTestCamera()
	raw := colourFrame(16, 16, 3)
	cam.processFrame(raw)
	frame := cam.newDetectFrame(raw, cam.rawSeq)
	want := raw.RGB24(nil)

	got := frame.RGB24()
	if !bytes.Equal(got, want) {
		t.Fatal("the detection frame's RGB is not the conversion of the frame")
	}
	if cam.rawStale || !bytes.Equal(cam.rawFrame, want) {
		t.Fatal("the conversion made for detection was not handed to readers")
	}
	if &got[0] == &cam.rawFrame[0] {
		t.Fatal("detection and readers share one buffer: a reader's conversion of the next frame would overwrite pixels detection still uses")
	}
	if again := frame.RGB24(); &again[0] != &got[0] {
		t.Fatal("the frame was converted a second time")
	}
}

func TestDetectionConversionLeavesANewerReaderFrameAlone(t *testing.T) {
	cam := lazyTestCamera()
	raw := colourFrame(16, 16, 3)
	cam.processFrame(raw)
	frame := cam.newDetectFrame(raw, cam.rawSeq)
	// A reader converted the frame first.
	fromReader := readRGB(t, cam, 16, 16)
	cam.rawFrame[0] ^= 0xFF
	marked := cam.rawFrame[0]

	if got := frame.RGB24(); !bytes.Equal(got, fromReader) {
		t.Fatal("the detection frame's RGB is not the conversion of the frame")
	}
	if cam.rawFrame[0] != marked {
		t.Fatal("detection overwrote pixels the reader side already held")
	}
}

// Motion detection reads the luma plane of the frame, and nothing else of it.
func TestProcessFrameFindsMotionInTheLumaPlane(t *testing.T) {
	cam := lazyTestCamera()
	const w, h = 64, 64
	cam.processFrame(media.NewTestRawFrame(w, h, 0))
	if got := cam.motionDetector.FrameCoverage(); got != 0 {
		t.Fatalf("setup: coverage after the first frame = %v, want 0", got)
	}

	moved := media.NewTestRawFrame(w, h, 0)
	luma := moved.Luma()
	for y := 16; y < 48; y++ {
		for x := 16; x < 48; x++ {
			luma[y*w+x] = 255
		}
	}
	cam.processFrame(moved)

	// A 32 by 32 block is a quarter of the frame; the blur softens its edge.
	if got := cam.motionDetector.FrameCoverage(); got < 0.2 || got > 0.3 {
		t.Fatalf("coverage after a quarter of the frame changed = %v, want about 0.25", got)
	}
}

func TestProcessFrameIgnoresAFrameOfTheWrongSize(t *testing.T) {
	cam := lazyTestCamera()
	good := colourFrame(16, 16, 3)
	cam.processFrame(good)

	cam.processFrame(media.RawFrame{Data: make([]byte, 16*16*3), Width: 16, Height: 16})
	cam.processFrame(media.RawFrame{Data: make([]byte, 16*16), Width: 16, Height: 16})
	cam.processFrame(media.RawFrame{})

	if got := readRGB(t, cam, 16, 16); !bytes.Equal(got, good.RGB24(nil)) {
		t.Fatal("a frame of the wrong size replaced the last good one")
	}
}

// A frame that arrives as RGB replaces a detection frame no reader has
// converted yet; the next read must not convert the older frame over it.
func TestAnRGBFrameReplacesAnUnreadDetectionFrame(t *testing.T) {
	red := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := 0; i < len(red.Pix); i += 4 {
		red.Pix[i], red.Pix[i+3] = 255, 255
	}

	t.Run("test frame", func(t *testing.T) {
		cam := lazyTestCamera()
		cam.processFrame(colourFrame(16, 16, 3))
		cam.SetTestFrame(red)
		if got := readRGB(t, cam, 16, 16); got[0] != 255 || got[1] != 0 || got[2] != 0 {
			t.Fatalf("first pixel = %v, want the red test frame", got[:3])
		}
	})

	t.Run("snapshot cached on disk", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "latest.jpg")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := jpeg.Encode(f, red, &jpeg.Options{Quality: 95}); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		cam := lazyTestCamera()
		cam.latestSnapshotPath = path
		// Keep processFrame from rewriting the file under test.
		cam.lastSnapshotSave = time.Now()
		cam.processFrame(media.NewTestRawFrame(16, 16, 0))
		cam.loadCachedSnapshot()

		got := color.RGBAModel.Convert(cam.LastSnapshot().At(8, 8)).(color.RGBA)
		if got.R < 240 || got.G > 15 || got.B > 15 {
			t.Fatalf("centre pixel = %v, want the red of the snapshot on disk", got)
		}
	})
}

// The detection goroutine may ask for a frame's colours after a later frame
// has arrived, as when a restarted camera's previous loop is still finishing.
// Its pixels then belong to no reader.
func TestDetectionConversionOfAnOlderFrameIsNotHandedToReaders(t *testing.T) {
	cam := lazyTestCamera()
	older, newer := colourFrame(16, 16, 3), colourFrame(16, 16, 90)
	cam.processFrame(older)
	frame := cam.newDetectFrame(older, cam.rawSeq)
	cam.processFrame(newer)

	if got := frame.RGB24(); !bytes.Equal(got, older.RGB24(nil)) {
		t.Fatal("the detection frame's RGB is not the conversion of its own frame")
	}
	if got := readRGB(t, cam, 16, 16); !bytes.Equal(got, newer.RGB24(nil)) {
		t.Fatal("a reader got the older frame's pixels as the latest frame")
	}
}
