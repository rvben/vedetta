package api

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDoorbellAnswerSurfaceShipsItsInteractionContract(t *testing.T) {
	html, err := fs.ReadFile(staticFiles, "static/doorbell-answer.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, required := range []string{
		`id="doorstep-video"`, `id="doorstep-snapshot"`, `id="doorstep-listen"`,
		`id="doorstep-talk"`, `id="doorstep-end"`, `id="doorstep-listen-label"`, `id="doorstep-manual"`,
		`id="doorstep-ended-action"`, `aria-live="polite"`,
		`src="/doorbell-answer-state.js"`, `src="/doorbell-answer.js"`, `href="/doorbell-answer.css"`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("doorbell answer page missing %s", required)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Fatal("doorbell answer page must not require an inline script")
	}

	script, err := fs.ReadFile(staticFiles, "static/doorbell-answer.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"requestVideoFrameCallback", "VIDEO_STALL_TIMEOUT_MS", "Video stalled · reconnecting",
		"startHLSFallback(attempt)",
	} {
		if !strings.Contains(string(script), required) {
			t.Errorf("doorbell answer script missing frame-stall guard %q", required)
		}
	}
}
