package api

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// First-run setup listens on every interface with no credentials configured
// yet, so whoever reaches the port first can create the admin account. The
// one-time setup code closes that window: it is generated per start, printed
// once to the log, and demanded by every setup endpoint that changes something
// or reveals the network.
const (
	setupCodeHeader = "X-Setup-Code"
	setupCodeQuery  = "setup_code"
	setupCodeCookie = "vedetta_setup_code"

	// setupCodeLength is 10 characters from a 32-symbol alphabet, i.e. 50 bits.
	setupCodeLength = 10

	// setupCodeAlphabet omits I, O, 0 and 1 so the code can be read off a
	// console and typed without ambiguity. Its length is a power of two, so
	// masking a random byte selects a symbol uniformly.
	setupCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// enableSetupCode generates the code for this start and logs it exactly once.
// Only the setup-mode server calls it; the full server has real authentication.
func (h *SetupHandler) enableSetupCode() {
	code, err := newSetupCode()
	if err != nil {
		// Without a code every setup endpoint refuses service, which is the
		// safe direction: the operator restarts rather than getting an open
		// setup wizard.
		slog.Error("failed to generate setup code, setup endpoints will refuse every request", "error", err)
		return
	}

	h.mu.Lock()
	h.code = code
	h.mu.Unlock()

	// The only place the code is ever written to the log. Failed attempts must
	// not repeat it, or the log becomes the leak.
	slog.Warn("first-run setup requires a one-time code, shown once below",
		"setup_code", code,
		"usage", "enter it in the setup wizard, or send it as the X-Setup-Code header or a setup_code query parameter")
}

func (h *SetupHandler) setupCode() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.code
}

func newSetupCode() (string, error) {
	buf := make([]byte, setupCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = setupCodeAlphabet[int(b)%len(setupCodeAlphabet)]
	}
	return string(buf), nil
}

// requireSetupCode wraps a setup handler so it only runs for a caller that
// presents the code. A missing or wrong code is a 403, and the response never
// echoes the expected value.
func (h *SetupHandler) requireSetupCode(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		supplied := suppliedSetupCode(r)

		if !h.setupCodeMatches(supplied) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "setup code required: see the one-time code printed in the server log at startup",
			})
			return
		}

		// The wizard makes many calls; carrying the verified code in a cookie
		// means the operator types it once. Set it on every accepted request,
		// whichever way the code arrived: the wizard sends it in the body of
		// the first call, and the discovery thumbnails that follow are <img>
		// loads that can carry neither a header nor a body.
		http.SetCookie(w, &http.Cookie{
			Name:     setupCodeCookie,
			Value:    supplied,
			Path:     "/",
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteStrictMode,
		})

		next(w, r)
	}
}

// setupCodeMatches reports whether supplied is the code this start issued. An
// unset code matches nothing, including the empty string a caller that sent no
// code at all produces.
func (h *SetupHandler) setupCodeMatches(supplied string) bool {
	want := h.setupCode()
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(want)) == 1
}

// setupCodeValid reports whether a request already holds the current code.
//
// The wizard asks this before choosing a screen. A restart part-way through
// setup issues a new code while the admin account stays configured, so a page
// that assumes the code its cookie holds is still good jumps to the camera
// screen, where every call is refused and no field exists to type the new code
// into. Answering the question here is what lets the page offer that field.
func (h *SetupHandler) setupCodeValid(r *http.Request) bool {
	return h.setupCodeMatches(suppliedSetupCode(r))
}

// suppliedSetupCode reads the code a request carries. A header suits API
// clients, a query parameter suits URLs a browser loads directly (thumbnails),
// a JSON body field suits the wizard form, and the cookie carries a code that
// was already accepted once.
func suppliedSetupCode(r *http.Request) string {
	if code := r.Header.Get(setupCodeHeader); code != "" {
		return code
	}
	if code := r.URL.Query().Get(setupCodeQuery); code != "" {
		return code
	}
	if code := setupCodeFromBody(r); code != "" {
		return code
	}
	if c, err := r.Cookie(setupCodeCookie); err == nil {
		return c.Value
	}
	return ""
}

// setupCodeBodyLimit bounds how much of a request body this function buffers
// while looking for the field. It is not an admission limit: handlers impose
// their own with http.MaxBytesReader, and a larger body is passed on whole so
// that limit still sees its true size.
const setupCodeBodyLimit = 1 << 20

// bodyReadCloser rejoins the bytes already buffered with the rest of the
// stream. A request body cannot be rewound, so this is how a body too large to
// buffer is handed on unchanged.
type bodyReadCloser struct {
	io.Reader
	io.Closer
}

// setupCodeFromBody reads a setup_code field out of a JSON body and puts the
// body back for the handler.
//
// The body always reaches the handler exactly as it arrived. Buffering a prefix
// and passing that on instead would turn an oversized request into a malformed
// one: the handler's own size limit would see a body that fits and answer
// "invalid JSON" for a request that is simply too large. Past the cap the field
// is not looked for, and a request that carries its code only there is refused
// like any request with no code, which is the safe direction for a guard.
func setupCodeFromBody(r *http.Request) string {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return ""
	}
	// One byte past the cap is what distinguishes a whole body from a body
	// larger than this function will hold.
	raw, err := io.ReadAll(io.LimitReader(r.Body, setupCodeBodyLimit+1))
	if err != nil {
		return ""
	}
	if len(raw) > setupCodeBodyLimit {
		rest := r.Body
		r.Body = bodyReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), rest), Closer: rest}
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))

	var body struct {
		SetupCode string `json:"setup_code"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return body.SetupCode
}
