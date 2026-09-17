package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/snapshot"
	"github.com/rvben/vedetta/internal/storage"
)

const pushoverEndpoint = "https://api.pushover.net/1/messages.json"

type pushoverJob struct {
	event   camera.Event
	payload pushPayload
	queued  time.Time
}

// Pushover delivers directly to a configured Pushover user or group, without a
// web push subscription. Its bounded queue never stalls camera processing.
type Pushover struct {
	cfg      config.PushoverConfig
	token    string
	user     string
	client   *http.Client
	endpoint string
	jobs     chan pushoverJob
	logger   *slog.Logger
	wg       sync.WaitGroup
}

func NewPushover(cfg config.PushoverConfig) (*Pushover, error) {
	token, err := readPushoverCredential(cfg.AppTokenFile)
	if err != nil {
		return nil, fmt.Errorf("Pushover application token: %w", err)
	}
	user, err := readPushoverCredential(cfg.UserKeyFile)
	if err != nil {
		return nil, fmt.Errorf("Pushover user key: %w", err)
	}
	if len(cfg.Events) == 0 {
		cfg.Events = []string{"doorbell", "activity"}
	}
	return &Pushover{
		cfg: cfg, token: token, user: user, endpoint: pushoverEndpoint,
		jobs: make(chan pushoverJob, 32), logger: slog.Default(),
		client: &http.Client{
			Timeout: 10 * time.Second,
			// A redirect must never forward the credential-bearing body.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func readPushoverCredential(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read credential file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128))
	value := strings.TrimSpace(string(b))
	if err != nil || len(value) != 30 || strings.ContainsFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) {
		return "", errors.New("credential must contain 30 alphanumeric characters")
	}
	return value, nil
}

func (p *Pushover) accepts(cameraName, kind string) bool {
	return slices.Contains(p.cfg.Events, kind) &&
		(len(p.cfg.Cameras) == 0 || slices.Contains(p.cfg.Cameras, cameraName))
}

func (p *Pushover) Enqueue(ev camera.Event) {
	if ev.Kind == camera.EventKindDoorbell {
		p.EnqueueDoorbell(ev)
	} else if ev.Category != camera.CategoryDetection && p.accepts(ev.CameraName, "activity") {
		p.enqueue(ev, BuildPayload(ev, nil))
	}
}

func (p *Pushover) EnqueueDoorbell(ev camera.Event) bool {
	if !p.accepts(ev.CameraName, "doorbell") {
		return true // intentionally filtered; not a delivery failure
	}
	return p.enqueue(ev, BuildPayload(ev, nil))
}

// EnqueueDoorbellEarly starts upload from a prepared attachment while the
// full-resolution snapshot is saved independently. A full queue gets one later
// enqueue attempt after storage; successful early enqueues are never repeated.
func (p *Pushover) EnqueueDoorbellEarly(ev camera.Event) func(camera.Event) bool {
	if !p.accepts(ev.CameraName, "doorbell") {
		return nil
	}
	// Normally prepared before the press. With no scored candidate, encode the
	// latest frame once here, still before snapshot storage or MQTT preparation.
	if !snapshot.ValidNotificationJPEG(ev.NotificationJPEG) && ev.SnapshotImage != nil {
		ev.NotificationJPEG, _ = snapshot.EncodeNotificationJPEG(ev.SnapshotImage)
	}
	if snapshot.ValidNotificationJPEG(ev.NotificationJPEG) {
		if p.EnqueueDoorbell(ev) {
			return nil
		}
		// Retain only the preview if a full queue needs one later enqueue attempt.
		preview := ev.NotificationJPEG
		return func(saved camera.Event) bool {
			saved.NotificationJPEG = preview
			return p.EnqueueDoorbell(saved)
		}
	}
	return p.EnqueueDoorbell
}

func (p *Pushover) EnqueueActivity(activity storage.Activity) bool {
	if activity.Category == camera.CategoryDetection || !p.accepts(activity.CameraName, "activity") {
		return true
	}
	// Pushover cannot replace a notification by tag as web push can. A ring
	// already sent immediately must not generate a second push at finalization.
	if activity.HasDoorbell && slices.Contains(p.cfg.Events, "doorbell") {
		return true
	}
	return p.enqueue(activity.PrimaryEvent, BuildActivityPayload(activity, nil))
}

func (p *Pushover) enqueue(ev camera.Event, data []byte) bool {
	var payload pushPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	// Retain only the bounded, immutable JPEG, never decoded camera frames.
	if len(ev.NotificationJPEG) > snapshot.NotificationJPEGMaxBytes {
		ev.NotificationJPEG = nil
	}
	ev.SnapshotImage, ev.AnnotatedImage = nil, nil
	select {
	case p.jobs <- pushoverJob{event: ev, payload: payload, queued: time.Now()}:
		return true
	default:
		p.logger.Warn("Pushover queue full", "event", ev.ID, "camera", ev.CameraName)
		return false
	}
}

func (p *Pushover) Start(ctx context.Context) {
	for range 2 {
		p.wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-p.jobs:
					// Don't ring someone's phone minutes after a provider outage.
					if time.Since(job.queued) > time.Minute {
						p.logger.Warn("Pushover notification expired in queue", "event", job.event.ID)
						continue
					}
					if err := p.send(ctx, job); err != nil {
						p.logger.Error("Pushover delivery failed", "event", job.event.ID, "error", err)
					}
				}
			}
		})
	}
}

func (p *Pushover) Wait() { p.wg.Wait() }

func (p *Pushover) send(ctx context.Context, job pushoverJob) error {
	started := time.Now()
	queueTime := started.Sub(job.queued)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fields := map[string]string{
		"token": p.token, "user": p.user,
		"title": job.payload.Title, "message": job.payload.Body,
		"timestamp": strconv.FormatInt(job.payload.TS, 10),
		"priority":  strconv.Itoa(p.cfg.Priority),
	}
	if p.cfg.Device != "" {
		fields["device"] = p.cfg.Device
	}
	if p.cfg.Sound != "" {
		fields["sound"] = p.cfg.Sound
	}
	if p.cfg.BaseURL != "" {
		fields["url"] = strings.TrimRight(p.cfg.BaseURL, "/") + job.payload.URL
		fields["url_title"] = "Open Vedetta"
	}
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			return fmt.Errorf("encode Pushover field: %w", err)
		}
	}
	photo, err := pushoverSnapshot(job.event)
	if err != nil {
		p.logger.Warn("Pushover sending without photo", "event", job.event.ID, "error", err)
	}
	if len(photo) > 0 {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="attachment"; filename="snapshot.jpg"`)
		h.Set("Content-Type", "image/jpeg")
		part, err := w.CreatePart(h)
		if err != nil {
			return fmt.Errorf("create Pushover attachment: %w", err)
		}
		if _, err := part.Write(photo); err != nil {
			return fmt.Errorf("encode Pushover attachment: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish Pushover message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, &body)
	if err != nil {
		return errors.New("cannot create Pushover request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", "Vedetta")
	prepared := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		// Do not retry an ambiguous transport failure: it may already have
		// delivered, and this API has no message idempotency key.
		return errors.New("Pushover request failed or timed out")
	}
	defer resp.Body.Close()
	var result struct {
		Status int `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil {
		return fmt.Errorf("invalid Pushover response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || result.Status != 1 {
		// Provider responses can echo credentials. Log only status codes.
		return fmt.Errorf("Pushover rejected notification (HTTP %d, status %d)", resp.StatusCode, result.Status)
	}
	p.logger.Info("Pushover notification delivered", "event", job.event.ID, "camera", job.event.CameraName, "photo", len(photo) > 0,
		"photo_bytes", len(photo), "prepared_photo", snapshot.ValidNotificationJPEG(job.event.NotificationJPEG),
		"queue_ms", max(0, queueTime.Milliseconds()), "prepare_ms", prepared.Sub(started).Milliseconds(),
		"api_ms", time.Since(prepared).Milliseconds(), "event_age_ms", max(0, time.Since(job.event.Timestamp).Milliseconds()))
	return nil
}

func pushoverSnapshot(ev camera.Event) ([]byte, error) {
	if snapshot.ValidNotificationJPEG(ev.NotificationJPEG) {
		return ev.NotificationJPEG, nil
	}
	if !ev.SnapshotAvailable || ev.SnapshotPath == "" {
		return nil, errors.New("event snapshot unavailable")
	}
	f, err := os.Open(ev.SnapshotPath)
	if err != nil {
		return nil, errors.New("cannot read event snapshot")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 32<<20))
	if err != nil {
		return nil, errors.New("cannot read event snapshot")
	}
	meta, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil || meta.Width <= 0 || meta.Height <= 0 || int64(meta.Width)*int64(meta.Height) > 40_000_000 {
		return nil, errors.New("invalid event snapshot")
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("cannot decode event snapshot")
	}
	return snapshot.EncodeNotificationJPEG(img)
}
