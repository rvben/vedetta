package logging

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func baseHandler(buf *bytes.Buffer) slog.Handler {
	return slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
}

func TestInitDisabledReturnsBaseHandler(t *testing.T) {
	var buf bytes.Buffer
	base := baseHandler(&buf)
	p, err := Init(context.Background(), Config{Enabled: false}, "test", base)
	if err != nil {
		t.Fatalf("Init returned %v", err)
	}
	if p.Handler() != base {
		t.Error("disabled Init must return the exact base handler")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("disabled Shutdown must be a no-op, got %v", err)
	}
}

func TestInitDegradesToBaseWhenExporterConstructionFails(t *testing.T) {
	// Exporter construction rarely fails synchronously in the real OTLP
	// exporters, so substitute a deterministic failure to prove the
	// degrade-to-base path: Init must return the EXACT base handler, no error.
	orig := exporterFactory
	t.Cleanup(func() { exporterFactory = orig })
	exporterFactory = func(context.Context, Config, func(string) string) (log.Exporter, error) {
		return nil, errors.New("boom")
	}

	var buf bytes.Buffer
	base := baseHandler(&buf)
	p, err := Init(context.Background(), Config{Enabled: true}, "test", base)
	if err != nil {
		t.Fatalf("Init must never return a fatal error, got %v", err)
	}
	if p.Handler() != base {
		t.Error("construction failure must degrade to the exact base handler")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("degraded Shutdown must be a no-op, got %v", err)
	}
}

func TestInitExportsToOTLPReceiverWithResourceAndCorrelation(t *testing.T) {
	type captured struct {
		req  *collogspb.ExportLogsServiceRequest
		path string
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, _ := gzip.NewReader(bytes.NewReader(body))
			body, _ = io.ReadAll(zr)
		}
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal export request: %v", err)
		}
		select {
		case got <- captured{req: &req, path: r.URL.Path}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	base := baseHandler(&buf)
	// httptest serves plain HTTP at the server root; use the URL form so the http
	// exporter targets it. ensureLogsPath must append /v1/logs.
	p, err := Init(context.Background(), Config{
		Enabled:     true,
		Endpoint:    srv.URL,
		Protocol:    "http/protobuf",
		ServiceName: "vedetta-test",
	}, "v9.9.9", base)
	if err != nil {
		t.Fatalf("Init returned %v", err)
	}

	// Emit under a known span context so we can assert trace-log correlation.
	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	lctx := trace.ContextWithSpanContext(context.Background(), sc)
	slog.New(p.Handler()).InfoContext(lctx, "hello-loki", "camera", "front")

	// Flush.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown returned %v", err)
	}

	select {
	case c := <-got:
		if c.path != "/v1/logs" {
			t.Errorf("OTLP/HTTP logs must POST to /v1/logs, got %q", c.path)
		}
		assertHasLog(t, c.req, "hello-loki", "vedetta-test", "v9.9.9", tid[:], sid[:])
		assertHasAttr(t, c.req, "camera", "front")
	case <-time.After(5 * time.Second):
		t.Fatal("no OTLP export received")
	}
}

func TestInitSendsConfiguredHeaders(t *testing.T) {
	// Multi-tenant log backends (e.g. Loki) require a tenant header on every
	// push. Configured headers must reach the OTLP receiver on the export request.
	gotHeader := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotHeader <- r.Header.Get("X-Scope-OrgID"):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	base := baseHandler(&buf)
	p, err := Init(context.Background(), Config{
		Enabled:     true,
		Endpoint:    srv.URL,
		Protocol:    "http/protobuf",
		ServiceName: "vedetta-test",
		Headers:     map[string]string{"X-Scope-OrgID": "vedetta"},
	}, "v9.9.9", base)
	if err != nil {
		t.Fatalf("Init returned %v", err)
	}

	slog.New(p.Handler()).Info("hello-loki")

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown returned %v", err)
	}

	select {
	case h := <-gotHeader:
		if h != "vedetta" {
			t.Errorf("X-Scope-OrgID header = %q, want \"vedetta\"", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OTLP export received")
	}
}

// noopExporter is a log.Exporter that discards all records and succeeds on all
// lifecycle calls. It is used in tests that need Init to reach the
// provider-creation path without a real OTLP collector.
type noopExporter struct{}

func (noopExporter) Export(context.Context, []log.Record) error { return nil }
func (noopExporter) ForceFlush(context.Context) error           { return nil }
func (noopExporter) Shutdown(context.Context) error             { return nil }

// TestInitEnabledInstallsRateLimitedErrorHandler proves that a successful Init
// (with OTLP enabled) replaces the default OTel error handler with the
// rate-limited one from otelexport. Without the InstallRateLimitedErrorHandler
// call in Init, a down collector would spam stderr via the default handler.
func TestInitEnabledInstallsRateLimitedErrorHandler(t *testing.T) {
	orig := exporterFactory
	t.Cleanup(func() { exporterFactory = orig })
	exporterFactory = func(_ context.Context, _ Config, _ func(string) string) (log.Exporter, error) {
		return noopExporter{}, nil
	}

	var buf bytes.Buffer
	base := baseHandler(&buf)
	p, err := Init(context.Background(), Config{Enabled: true, ServiceName: "test"}, "v0.0.0", base)
	if err != nil {
		t.Fatalf("Init returned %v", err)
	}
	defer p.Shutdown(context.Background()) //nolint:errcheck

	got := otel.GetErrorHandler()
	if strings.Contains(fmt.Sprintf("%T", got), "ErrDelegator") {
		t.Fatalf("Init must install a rate-limited error handler, got default %T", got)
	}
}

func assertHasLog(t *testing.T, req *collogspb.ExportLogsServiceRequest, body, svc, ver string, traceID, spanID []byte) {
	t.Helper()
	var sawBody, sawService, sawVersion, sawTrace, sawSpan bool
	for _, rl := range req.GetResourceLogs() {
		for _, kv := range rl.GetResource().GetAttributes() {
			if kv.GetKey() == "service.name" && kv.GetValue().GetStringValue() == svc {
				sawService = true
			}
			if kv.GetKey() == "service.version" && kv.GetValue().GetStringValue() == ver {
				sawVersion = true
			}
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				if strings.Contains(lr.GetBody().GetStringValue(), body) {
					sawBody = true
				}
				if bytes.Equal(lr.GetTraceId(), traceID) {
					sawTrace = true
				}
				if bytes.Equal(lr.GetSpanId(), spanID) {
					sawSpan = true
				}
			}
		}
	}
	if !sawBody {
		t.Error("exported logs missing the record body")
	}
	if !sawService {
		t.Errorf("exported logs missing service.name=%q", svc)
	}
	if !sawVersion {
		t.Errorf("exported logs missing service.version=%q", ver)
	}
	if !sawTrace {
		t.Error("exported logs missing the correlated trace_id")
	}
	if !sawSpan {
		t.Error("exported logs missing the correlated span_id")
	}
}

// assertHasAttr proves an slog key-value attribute passed at the call site
// (e.g. slog.InfoContext(ctx, msg, key, value)) survives the otelslog ->
// otel/sdk/log -> OTLP conversion pipeline and reaches the exported log
// record's own Attributes (not the resource's). This is what an otelslog or
// otel/log version bump could silently break if the two ever drift apart on
// their key/value representation.
func assertHasAttr(t *testing.T, req *collogspb.ExportLogsServiceRequest, key, value string) {
	t.Helper()
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				for _, kv := range lr.GetAttributes() {
					if kv.GetKey() == key && kv.GetValue().GetStringValue() == value {
						return
					}
				}
			}
		}
	}
	t.Errorf("exported log record missing attribute %s=%q", key, value)
}

func TestEnsureLogsPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no path gets /v1/logs", "http://host:4318", "http://host:4318/v1/logs"},
		{"root path gets /v1/logs", "http://host:4318/", "http://host:4318/v1/logs"},
		{"correct path preserved", "http://host:4318/v1/logs", "http://host:4318/v1/logs"},
		{"custom path preserved", "http://host:4318/custom/path", "http://host:4318/custom/path"},
		{"https no path gets /v1/logs", "https://otel.example.com", "https://otel.example.com/v1/logs"},
		{"unparseable returned unchanged", "://not a url", "://not a url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ensureLogsPath(tc.in); got != tc.want {
				t.Errorf("ensureLogsPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
