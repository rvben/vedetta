package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// exportPath exports one span through the production exporter builder and
// returns the path the collector was asked for.
func exportPath(t *testing.T, endpoint func(serverURL string) string) string {
	t.Helper()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exporter, err := buildExporter(ctx, Config{Endpoint: endpoint(srv.URL), Protocol: "http"}, env(nil))
	if err != nil {
		t.Fatalf("buildExporter: %v", err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	_, span := provider.Tracer("test").Start(ctx, "probe")
	span.End()
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case path := <-got:
		return path
	default:
		t.Fatal("the collector received no export request")
		return ""
	}
}

func TestHTTPExporterPostsToTheTracesPath(t *testing.T) {
	cases := []struct {
		name     string
		endpoint func(string) string
		want     string
	}{
		{"URL without a path", func(u string) string { return u }, "/v1/traces"},
		{"URL with a root path", func(u string) string { return u + "/" }, "/v1/traces"},
		{"URL that names the path", func(u string) string { return u + "/v1/traces" }, "/v1/traces"},
		{"URL with a custom path", func(u string) string { return u + "/otlp/traces" }, "/otlp/traces"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exportPath(t, tc.endpoint); got != tc.want {
				t.Errorf("exported to %q, want %q", got, tc.want)
			}
		})
	}
}
