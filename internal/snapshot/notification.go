package snapshot

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"

	"golang.org/x/image/draw"
)

// NotificationJPEGMaxBytes bounds an inline photo retained in notification
// queues. The 1280px preview normally occupies only a fraction of this limit.
const NotificationJPEGMaxBytes = 2 << 20
const notificationMaxSide = 1280

// EncodeNotificationJPEG prepares a compact attachment without touching disk.
// The returned bytes are immutable and may be shared with notification workers.
func EncodeNotificationJPEG(img image.Image) ([]byte, error) {
	if img == nil || img.Bounds().Empty() {
		return nil, errors.New("notification image unavailable")
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if longest := max(width, height); longest > notificationMaxSide {
		width, height = max(1, width*notificationMaxSide/longest), max(1, height*notificationMaxSide/longest)
		resized := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.ApproxBiLinear.Scale(resized, resized.Bounds(), img, b, draw.Src, nil)
		img = resized
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, errors.New("cannot encode notification image")
	}
	if out.Len() > NotificationJPEGMaxBytes {
		return nil, errors.New("notification image exceeds size limit")
	}
	return out.Bytes(), nil
}

// ValidNotificationJPEG checks the size and dimensions of an internally
// prepared attachment before it enters a bounded delivery queue.
func ValidNotificationJPEG(data []byte) bool {
	if len(data) == 0 || len(data) > NotificationJPEGMaxBytes {
		return false
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	return err == nil && cfg.Width > 0 && cfg.Height > 0 && cfg.Width <= notificationMaxSide && cfg.Height <= notificationMaxSide
}
