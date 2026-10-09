package aac

import (
	"math"
	"os"
	"testing"
)

// ensureFDKForTest skips the test when libfdk-aac is not loadable on the
// host. Unlike OpenH264 there is no managed installer, so absence is a
// clean skip (the production path degrades to video-only) rather than a
// failure. On the dev Macs and the deploy target the library is present,
// so this exercises the real native binding there.
func ensureFDKForTest(t *testing.T) {
	t.Helper()
	if !ensureFDK() {
		if os.Getenv("VEDETTA_REQUIRE_AAC") == "1" {
			t.Fatalf("required libfdk-aac unavailable: %v", fdkLoadErr)
		}
		t.Skipf("libfdk-aac not available, skipping native encoder test: %v", fdkLoadErr)
	}
}

// TestFDKEncodesPCMToAAC drives the real libfdk-aac binding end to end:
// a 1 kHz sine at 8 kHz mono (the G.711 camera profile) must encode into
// non-empty AAC-LC access units, one per 1024 input samples, with no
// native crash. This is the regression guard for the purego struct ABI
// in aac_fdk.go (fdkBufDesc padding, in/out arg layout).
func TestFDKEncodesPCMToAAC(t *testing.T) {
	ensureFDKForTest(t)

	enc, err := newFDKAACEncoder(8000, 1)
	if err != nil {
		t.Fatalf("newFDKAACEncoder(8000, 1): %v", err)
	}
	defer enc.Close()

	// 8000 samples = 1 s of audio = ~7-8 AAC frames of 1024 samples.
	const n = 8000
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/8000))
	}

	frames, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// The encoder primes internally, so the exact count varies by a frame
	// or two; demand that it produced multiple non-trivial AAC AUs.
	if len(frames) < 5 {
		t.Fatalf("expected several AAC frames from 1 s of audio, got %d", len(frames))
	}
	for i, f := range frames {
		if len(f) == 0 {
			t.Fatalf("AAC frame %d is empty", i)
		}
	}
}

// TestFDKRejectsNonMono guards the deliberate mono-only contract: camera
// G.711 is always single channel, and a misconfigured multi-channel track
// must fail loudly rather than emit a broken AAC track.
func TestFDKRejectsNonMono(t *testing.T) {
	if _, err := newFDKAACEncoder(8000, 2); err == nil {
		t.Fatal("newFDKAACEncoder must reject a non-mono channel count")
	}
}

func TestFDKRecordingDrain(t *testing.T) {
	ensureFDKForTest(t)
	enc, err := New(48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	t.Logf("AAC priming: %d samples", enc.Delay())
	if enc.Delay() < 1024 || enc.Delay() > 4096 {
		t.Fatalf("unexpected priming: %d", enc.Delay())
	}
	frames, err := enc.Encode(make([]int16, 48000))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := enc.Flush()
	if err != nil {
		t.Fatal(err)
	}
	count := len(frames) + len(tail) - enc.Delay()/1024
	if count < 47 {
		t.Fatalf("lost audio tail: %d frames", count)
	}
	if again, err := enc.Flush(); err != nil || len(again) != 0 {
		t.Fatalf("second flush: %d %v", len(again), err)
	}
	if _, err := enc.Encode([]int16{1}); err == nil {
		t.Fatal("encode after flush succeeded")
	}
}
