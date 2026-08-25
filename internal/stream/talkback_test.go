package stream

import (
	"context"
	"errors"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/webrtc/v4"
)

func TestTalkbackCodecName(t *testing.T) {
	if got := codecName(&format.G711{MULaw: true}); got != "PCMU" {
		t.Fatalf("mu-law codec = %q", got)
	}
	if got := codecName(&format.G711{MULaw: false}); got != "PCMA" {
		t.Fatalf("A-law codec = %q", got)
	}
}

func TestTalkbackManagerRejectsSecondSpeakerBeforeConnectingCamera(t *testing.T) {
	m := NewTalkbackManager(nil)
	m.active["front_door"] = struct{}{}
	_, err := m.HandleOffer(context.Background(), "front_door", "rtsp://camera.invalid/stream", webrtc.SessionDescription{})
	if !errors.Is(err, ErrTalkbackBusy) {
		t.Fatalf("error = %v, want ErrTalkbackBusy", err)
	}
}
