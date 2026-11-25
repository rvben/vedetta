package api

import (
	"context"
	"net/http"
	"net/http/pprof"
	"strconv"
)

// maxProfileSeconds bounds ?seconds=, which every profile accepts: the CPU
// profile and execution trace record for that long, and a snapshot profile
// given it answers with the change over that interval instead of the totals.
// Either way the request stays open and the process holds profiling state for
// the whole length, so the length is the caller's to choose only up to a limit.
const maxProfileSeconds = 120

// pprofSnapshots are the point-in-time profiles served on request. block and
// mutex are absent because their sampling is off, which would make them answer
// with an empty profile that reads as "no contention".
var pprofSnapshots = map[string]bool{
	"heap":         true,
	"allocs":       true,
	"goroutine":    true,
	"threadcreate": true,
}

// handlePprof serves runtime profiles of the running process, so CPU and memory
// questions about a deployed server can be answered from that server. trace is
// an execution trace for `go tool trace`; the rest are in the format
// `go tool pprof` reads.
//
// Importing net/http/pprof also registers its handlers on
// http.DefaultServeMux. The API server serves its own mux, so those
// registrations are unreachable and this route is the only way in.
func (s *Server) handlePprof(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	timed := name == "profile" || name == "trace"
	if !timed && !pprofSnapshots[name] {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown profile"})
		return
	}
	// The profilers read their parameters with FormValue, which would parse a
	// request body. Handing them a request whose form is already the query
	// keeps the body unread and makes the query the only source of the length.
	query := r.URL.Query()
	r = r.Clone(r.Context())
	r.Form = query
	if query.Has("seconds") {
		seconds, err := strconv.Atoi(query.Get("seconds"))
		if err != nil || seconds < 1 || seconds > maxProfileSeconds {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "seconds must be a whole number from 1 to " + strconv.Itoa(maxProfileSeconds),
			})
			return
		}
	}

	// A capture waits on the request context. Server shutdown does not cancel
	// that, so the capture also ends when the application context does and a
	// restart is not held up by a profile in progress.
	if s.ctx != nil {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(s.ctx, cancel)()
		r = r.WithContext(ctx)
	}

	// A profile is a view of process memory behind the admin check, so no cache
	// may keep a copy to serve later.
	w.Header().Set("Cache-Control", "no-store")

	switch name {
	case "profile":
		pprof.Profile(w, r)
	case "trace":
		pprof.Trace(w, r)
	default:
		pprof.Handler(name).ServeHTTP(w, r)
	}
}
