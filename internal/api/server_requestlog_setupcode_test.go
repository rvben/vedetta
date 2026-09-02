package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The setup code is a credential: it authorizes every setup endpoint, and
// suppliedSetupCode accepts it as a query parameter so an operator can follow a
// link that carries it. The access log is retained for weeks, shipped to
// whatever sink is configured, and readable by anyone who can read logs, so a
// request line logged verbatim hands that credential to all of them.
func TestRequestLogRedactsTheSetupCodeFromTheQuery(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	const code = "K7QP2MRX9T"
	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/setup/status?"+setupCodeQuery+"="+code+"&step=cameras", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := buf.String(); strings.Contains(got, code) {
		t.Fatalf("setup code %q reached the log:\n%s", code, got)
	}

	rec := findRequestLog(t, buf)
	uri, _ := rec["uri"].(string)
	// The line still has to identify the request, or redacting it has traded a
	// leak for a blind spot: the path, the other parameters, and the fact that a
	// setup code was supplied all remain.
	if !strings.HasPrefix(uri, "/api/setup/status?") {
		t.Errorf("logged uri lost the path: %q", uri)
	}
	if !strings.Contains(uri, setupCodeQuery+"=redacted") {
		t.Errorf("logged uri does not record that a setup code was supplied: %q", uri)
	}
	if !strings.Contains(uri, "step=cameras") {
		t.Errorf("logged uri dropped an unrelated parameter: %q", uri)
	}
}

// A wrong code is the case an operator actually goes looking for in the log, and
// it is graded Warn rather than Info, which takes a different branch through the
// middleware. Redaction has to hold on that branch too.
func TestRequestLogRedactsTheSetupCodeOnARejectedRequest(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	const code = "WRONGCODE1"
	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/setup/config?"+setupCodeQuery+"="+code, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := buf.String(); strings.Contains(got, code) {
		t.Fatalf("setup code %q reached the log on a 401:\n%s", code, got)
	}
}

// The accepting bound. Redaction rewrites the query it touches, and re-encoding
// normalizes parameter order and escaping, so an ordinary request line would
// stop being a faithful record of what the client asked for. Every request
// without a setup code must be logged byte for byte.
func TestRequestLogKeepsAnOrdinaryQueryVerbatim(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Reverse alphabetical order with a literal space escaped as '+': both
	// survive verbatim, and neither survives a round trip through Encode.
	const target = "/api/events?zone=front+yard&label=person&camera=garage"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))

	rec := findRequestLog(t, buf)
	if uri, _ := rec["uri"].(string); uri != target {
		t.Errorf("logged uri = %q, want the request line unchanged: %q", uri, target)
	}
}

// A parameter whose name merely ends in the redacted one is not the credential,
// and must not be mistaken for it by the substring prefilter.
func TestRequestLogKeepsAParameterThatOnlyLooksLikeTheSetupCode(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	const target = "/api/events?not_" + setupCodeQuery + "=visible"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))

	rec := findRequestLog(t, buf)
	if uri, _ := rec["uri"].(string); uri != target {
		t.Errorf("logged uri = %q, want %q", uri, target)
	}
}

// A query parameter name can be percent-encoded, and Go decodes the name before
// matching it: "setup%5Fcode" parses to the key "setup_code" and is accepted as
// the credential. Deciding what to redact from the raw request line therefore
// misses it, and the working code is logged in the clear. The test asserts the
// credential is real before asserting it is redacted, so it cannot pass by the
// parameter simply being ignored.
func TestRequestLogRedactsAPercentEncodedSetupCodeName(t *testing.T) {
	buf := captureLogsAt(t, slog.LevelDebug)

	const code = "M4XV7BQZ2K"
	const target = "/api/setup/status?setup%5Fcode=" + code

	if got := suppliedSetupCode(httptest.NewRequest(http.MethodGet, target, nil)); got != code {
		t.Fatalf("suppliedSetupCode = %q, want %q: the encoded name is not a credential, so there is nothing to redact", got, code)
	}

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))

	if got := buf.String(); strings.Contains(got, code) {
		t.Fatalf("setup code %q reached the log through a percent-encoded parameter name:\n%s", code, got)
	}

	rec := findRequestLog(t, buf)
	uri, _ := rec["uri"].(string)
	if !strings.Contains(uri, setupCodeQuery+"=redacted") {
		t.Errorf("logged uri does not record that a setup code was supplied: %q", uri)
	}
}
