package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func pprofRequest(t *testing.T, handler http.Handler, target string, authorize func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authorize != nil {
		authorize(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// A pprof payload is a gzip-compressed protobuf. Decompressing it proves the
// handler wrote a real profile rather than an error page with a 200 status.
func requireGzipProfile(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading profile: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("profile is empty")
	}
}

func TestPprof_RequiresAuthentication(t *testing.T) {
	_, handler, _ := newTestServerWithAuth(t)

	rec := pprofRequest(t, handler, "/api/debug/pprof/heap", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPprof_TokenNeedsAdminScope(t *testing.T) {
	_, handler, checker := newTestServerWithAuth(t)

	bearer := func(scopes ...string) func(*http.Request) {
		_, token, err := checker.CreateToken("admin", "pprof-"+scopes[0], scopes, "192.0.2.10")
		if err != nil {
			t.Fatalf("CreateToken(%v): %v", scopes, err)
		}
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}

	rec := pprofRequest(t, handler, "/api/debug/pprof/heap", bearer("api:read"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("api:read status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	requireGzipProfile(t, pprofRequest(t, handler, "/api/debug/pprof/heap", bearer("admin")))
}

func TestPprof_ServesEachSnapshotProfile(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, name := range []string{"heap", "allocs", "goroutine", "threadcreate"} {
		t.Run(name, func(t *testing.T) {
			requireGzipProfile(t, pprofRequest(t, srv.mux, "/api/debug/pprof/"+name, nil))
		})
	}
}

func TestPprof_CPUProfileRunsForTheRequestedTime(t *testing.T) {
	srv, _ := newTestServer(t)

	start := time.Now()
	rec := pprofRequest(t, srv.mux, "/api/debug/pprof/profile?seconds=1", nil)
	elapsed := time.Since(start)

	requireGzipProfile(t, rec)
	if elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("profile took %v, want about 1s", elapsed)
	}
}

func TestPprof_TraceIsServed(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := pprofRequest(t, srv.mux, "/api/debug/pprof/trace?seconds=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("trace is empty")
	}
}

// The capture length is the caller's to choose, within a bound: an unbounded
// value would leave the profiler running, and the request open, for as long as
// a typo says.
func TestPprof_RejectsCaptureLengthOutsideBounds(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, kind := range []string{"profile", "trace", "heap", "allocs", "goroutine", "threadcreate"} {
		for _, seconds := range []string{"0", "-1", "121", "abc", "1.5", ""} {
			target := "/api/debug/pprof/" + kind + "?seconds=" + seconds
			t.Run(target, func(t *testing.T) {
				start := time.Now()
				rec := pprofRequest(t, srv.mux, target, nil)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
				}
				if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
					t.Fatalf("rejected request still ran for %v", elapsed)
				}
			})
		}
	}
}

// The length comes from the query alone. The profilers would otherwise read
// one from a request body, past the bound, so a body is left unparsed.
func TestPprof_IgnoresCaptureLengthInTheBody(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, kind := range []string{"heap", "trace"} {
		t.Run(kind, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			if err := form.WriteField("seconds", "600"); err != nil {
				t.Fatal(err)
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/debug/pprof/"+kind, &body)
			req.Header.Set("Content-Type", form.FormDataContentType())
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				srv.mux.ServeHTTP(rec, req)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("request is still running: the body's 600 seconds was honoured")
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if body.Len() == 0 {
				t.Fatal("request body was read")
			}
		})
	}
}

func TestPprof_ResponsesAreNotCacheable(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := pprofRequest(t, srv.mux, "/api/debug/pprof/heap", nil)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// A restart must not wait for a capture in progress: the capture ends with the
// application context even though the client is still connected.
func TestPprof_CaptureEndsWhenTheApplicationStops(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv.SetContext(ctx)

	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	pprofRequest(t, srv.mux, "/api/debug/pprof/profile?seconds=20", nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("capture ran for %v after the application context ended", elapsed)
	}
}

// A snapshot profile given ?seconds= answers with what changed over that
// interval, which is how allocation is attributed to a window of a long-lived
// process.
func TestPprof_SnapshotProfileCoversTheRequestedInterval(t *testing.T) {
	srv, _ := newTestServer(t)

	start := time.Now()
	rec := pprofRequest(t, srv.mux, "/api/debug/pprof/allocs?seconds=1", nil)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("interval profile returned after %v, want about 1s", elapsed)
	}
	if _, err := gzip.NewReader(rec.Body); err != nil {
		t.Fatalf("interval profile is not a gzip stream: %v", err)
	}
}

func TestPprof_UnknownProfileIsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	// cmdline and symbol exist in net/http/pprof and are deliberately not
	// offered; block and mutex are empty unless sampling is switched on.
	for _, name := range []string{"nonsense", "cmdline", "symbol", "block", "mutex"} {
		rec := pprofRequest(t, srv.mux, "/api/debug/pprof/"+name, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, http.StatusNotFound)
		}
	}
}
