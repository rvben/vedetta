package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rtspProbeServer mimics a bridge whose battery camera is asleep: it speaks
// RTSP and is reachable, but the stream path does not exist. It records the
// time of every connection attempt so a test can measure the retry cadence.
type rtspProbeServer struct {
	ln       net.Listener
	attempts atomic.Int64
	start    time.Time

	mu sync.Mutex
	at []time.Duration // offset from start of each connection attempt
}

func (s *rtspProbeServer) offsets() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.at...)
}

func newRTSPProbeServer(t *testing.T) *rtspProbeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &rtspProbeServer{ln: ln, start: time.Now()}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *rtspProbeServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.attempts.Add(1)
		s.mu.Lock()
		s.at = append(s.at, time.Since(s.start))
		s.mu.Unlock()
		go s.handle(conn)
	}
}

// handle completes the RTSP handshake and then refuses DESCRIBE, which is what
// an eufy HomeBase does while its camera sleeps. Answering promptly rather than
// stalling until a read timeout matters for the timing tests: it keeps each
// attempt short, so the gaps they measure reflect the retry policy instead of
// the stub's own latency.
func (s *rtspProbeServer) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		method, cseq, err := readRTSPRequest(br)
		if err != nil {
			return
		}
		if method == "OPTIONS" {
			_, _ = fmt.Fprintf(c, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n"+
				"Public: DESCRIBE, SETUP, PLAY, TEARDOWN\r\n\r\n", cseq)
			continue
		}
		_, _ = fmt.Fprintf(c, "RTSP/1.0 404 Not Found\r\nCSeq: %s\r\n\r\n", cseq)
		return
	}
}

// readRTSPRequest reads one request and returns its method and CSeq. Clients
// discard a response whose CSeq does not echo the request, so the header has to
// be parsed rather than hardcoded.
func readRTSPRequest(br *bufio.Reader) (string, string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	method, _, _ := strings.Cut(strings.TrimSpace(line), " ")

	var cseq string
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		header = strings.TrimSpace(header)
		if header == "" {
			return method, cseq, nil
		}
		if key, value, ok := strings.Cut(header, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(key), "CSeq") {
			cseq = strings.TrimSpace(value)
		}
	}
}

func (s *rtspProbeServer) url() string {
	return "rtsp://" + s.ln.Addr().String() + "/live0"
}

// maxGap returns the largest interval between consecutive connection attempts.
// This is the property that decides whether a wake window is caught: a camera
// that publishes its stream for two minutes is missed whenever the retry gap
// approaches that length, regardless of how many attempts were made overall.
func maxGap(offsets []time.Duration) time.Duration {
	var worst time.Duration
	for i := 1; i < len(offsets); i++ {
		if g := offsets[i] - offsets[i-1]; g > worst {
			worst = g
		}
	}
	return worst
}

// TestOnDemandSourceRetriesOnConstantInterval pins the property that makes
// on-demand cameras work: the gap between attempts stays short and flat, so a
// wake window opening at an arbitrary moment is caught. The default exponential
// backoff fails this, growing toward a 2 minute cap that matches the length of
// the window an eufy HomeBase offers, so it would catch windows only by luck.
//
// A default source runs against an identical server as a control. Asserting
// only the on-demand side would pass even if the policy silently became the
// default one, since both would then be measured against the same stub costs.
func TestOnDemandSourceRetriesOnConstantInterval(t *testing.T) {
	onDemandSrv := newRTSPProbeServer(t)
	defaultSrv := newRTSPProbeServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go newSource(onDemandSrv.url(), "tcp", true).Connect(ctx)
	go NewSourceWithTransport(defaultSrv.url(), "tcp").Connect(ctx)

	// Jitter halves a wait at most, so the on-demand gap stays within [1.5s, 3s]
	// while the default one grows through [2.5s, 5s], [3.75s, 7.5s], [5.6s,
	// 11.25s]. Observing 20s guarantees the default reaches that third gap.
	const observe = 20 * time.Second
	time.Sleep(observe)
	cancel()

	onDemandGaps := onDemandSrv.offsets()
	defaultGaps := defaultSrv.offsets()
	t.Logf("on-demand attempts at %v", onDemandGaps)
	t.Logf("default    attempts at %v", defaultGaps)

	// Ceiling sits above the on-demand worst case of 3s, with slack for a loaded
	// machine, and below the 5.6s floor of the default policy's third gap.
	const ceiling = 4500 * time.Millisecond

	if len(onDemandGaps) < 2 {
		t.Fatalf("on-demand source made %d attempts in %s; too few to measure a gap",
			len(onDemandGaps), observe)
	}
	if got := maxGap(onDemandGaps); got > ceiling {
		t.Errorf("on-demand retry gap grew to %s (max allowed %s); "+
			"a gap this long starts missing wake windows", got, ceiling)
	}
	if len(onDemandGaps) < 5 {
		t.Errorf("on-demand source made only %d attempts in %s; expected steady retry",
			len(onDemandGaps), observe)
	}

	// Control: the default policy must still back off, or the test above proves
	// nothing about on-demand being different.
	if got := maxGap(defaultGaps); got <= ceiling {
		t.Errorf("default source retry gap stayed at %s, never exceeding %s; "+
			"the control did not back off, so the on-demand assertion is vacuous",
			got, ceiling)
	}
}

// TestOnDemandSourceDoesNotCountSleepAsReconnect ensures a camera going back to
// sleep does not register as a flapping connection. Without this the reconnect
// metric tracks event volume and the camera reads as permanently unhealthy.
func TestOnDemandSourceDoesNotCountSleepAsReconnect(t *testing.T) {
	src := newSource("rtsp://192.0.2.10:554/live0", "tcp", true)

	sink := &atomic.Int64{}
	src.AddReconnectSink(sink)

	// Drive the real disconnect path as if an established stream had ended,
	// which for an on-demand camera is what happens at the end of every event.
	src.SimulateReconnectForTest()

	if n := src.Reconnects(); n != 0 {
		t.Errorf("on-demand source counted %d reconnects after a sleep cycle; want 0", n)
	}
	if n := sink.Load(); n != 0 {
		t.Errorf("on-demand sleep cycle incremented external sink to %d; want 0", n)
	}
}

// TestRegularSourceStillCountsReconnects is the negative control for the test
// above: the suppression must be scoped to on-demand sources, or it silently
// disables flap detection for every ordinary camera.
func TestRegularSourceStillCountsReconnects(t *testing.T) {
	src := NewSourceWithTransport("rtsp://192.0.2.10:554/stream", "tcp")

	sink := &atomic.Int64{}
	src.AddReconnectSink(sink)
	src.SimulateReconnectForTest()

	if n := src.Reconnects(); n != 1 {
		t.Errorf("regular source counted %d reconnects; want 1", n)
	}
	if n := sink.Load(); n != 1 {
		t.Errorf("regular source sink got %d; want 1", n)
	}
}

// TestHubAppliesOnDemandRegistration verifies the policy survives the Hub's
// shared-Source-per-URL model, where any subsystem may open the stream first.
func TestHubAppliesOnDemandRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := NewHub(ctx)
	defer h.Close()

	const onDemandURL = "rtsp://192.0.2.20:554/live0"
	const regularURL = "rtsp://192.0.2.21:554/stream"
	h.RegisterOnDemand(onDemandURL)

	if src := h.GetOrCreate(onDemandURL); !src.onDemand {
		t.Error("registered URL produced a Source without the on-demand policy")
	}
	if src := h.GetOrCreate(regularURL); src.onDemand {
		t.Error("unregistered URL produced an on-demand Source")
	}
}
