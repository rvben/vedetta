package snapshot

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
	"time"
)

func TestPreparedNotificationPhotoMatchesSelectedFrame(t *testing.T) {
	var buffer DoorbellBuffer
	at := time.Now()
	earlier := visitorFrame(true)
	later := visitorFrame(false)
	buffer.Add(earlier, at.Add(-500*time.Millisecond), []Subject{visitorSubject(true)})
	buffer.Add(later, at, []Subject{visitorSubject(false)})
	img, preview, _, _ := buffer.BestWithPreview(at)
	if img != earlier || !ValidNotificationJPEG(preview) {
		t.Fatal("selected visitor lacks matching prepared photo")
	}
	want, err := EncodeNotificationJPEG(earlier)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preview, want) {
		t.Fatal("prepared photo came from a different candidate")
	}
}
func TestNotificationPhotoBoundsAndValidation(t *testing.T) {
	encoded, err := EncodeNotificationJPEG(image.NewRGBA(image.Rect(0, 0, 2560, 1920)))
	if err != nil || !ValidNotificationJPEG(encoded) {
		t.Fatal("large frame did not produce a bounded attachment")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || cfg.Width != 1280 || cfg.Height != 960 {
		t.Fatal("unexpected attachment dimensions")
	}
	for _, invalid := range [][]byte{nil, []byte("not a photo"), make([]byte, NotificationJPEGMaxBytes+1)} {
		if ValidNotificationJPEG(invalid) {
			t.Fatal("invalid/oversized preview accepted")
		}
	}
	if _, err := EncodeNotificationJPEG(nil); err == nil {
		t.Fatal("missing image should not produce a preview")
	}
}
