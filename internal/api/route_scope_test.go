package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/auth"
)

// scopeClass is the authority a route demands from an API token.
type scopeClass int

const (
	classAdmin  scopeClass = iota // admin scope only; api:write is not enough
	classWrite                    // an ordinary api:write token may call it
	classTokens                   // token management, gated by tokens:write
	classPublic                   // no scope check: the auth middleware exempts it
)

func (c scopeClass) String() string {
	switch c {
	case classAdmin:
		return "admin"
	case classWrite:
		return "api:write"
	case classTokens:
		return "tokens:write"
	case classPublic:
		return "public"
	}
	return "unknown"
}

// routeScopes is the register of what every mutating route demands. Mutation is
// admin by default, so an entry of classWrite is a deliberate downgrade: the
// route neither rewrites configuration nor destroys recorded data.
//
// Keys are "METHOD pattern" exactly as the route is declared, in the OpenAPI
// spec or in a mux registration. A route missing from this table fails the test
// rather than shipping with whatever scope it happens to inherit.
var routeScopes = map[string]scopeClass{
	// Session handling. Login is reached before authentication runs.
	"POST /api/auth/login":  classPublic,
	"POST /api/auth/logout": classWrite,

	// Account credentials.
	"POST /api/auth/password": classAdmin,

	// Configuration: the camera list, the settings forms, codec installation.
	"POST /api/cameras/manage":                 classAdmin,
	"PUT /api/cameras/manage/{index}":          classAdmin,
	"DELETE /api/cameras/manage/{index}":       classAdmin,
	"PUT /api/settings/mqtt":                   classAdmin,
	"PUT /api/settings/recording":              classAdmin,
	"PUT /api/settings/detect":                 classAdmin,
	"POST /api/system/codecs/openh264/install": classAdmin,

	// Detection zones decide what the NVR records and alerts on, so editing
	// them is configuration even though the rows live in the database.
	"POST /api/cameras/{name}/zones":          classAdmin,
	"PUT /api/cameras/{name}/zones/{zone}":    classAdmin,
	"DELETE /api/cameras/{name}/zones/{zone}": classAdmin,

	// Starting and stopping a camera turns recording for that camera off. That
	// is an operator action, not a data write.
	"POST /api/cameras/{name}/start": classAdmin,
	"POST /api/cameras/{name}/stop":  classAdmin,

	// Destructive: these delete recordings, clips, people, or object history.
	"POST /api/storage/delete":            classAdmin,
	"POST /api/storage/cleanup":           classAdmin,
	"DELETE /api/objects/{id}":            classAdmin,
	"DELETE /api/objects/references/{id}": classAdmin,
	"DELETE /api/objects/sightings/{id}":  classAdmin,
	"DELETE /api/people/{id}":             classAdmin,
	"POST /api/people/merge":              classAdmin,

	// Whole-library reprocessing jobs: expensive, and they rewrite stored
	// derivatives for every event at once.
	"POST /api/faces/backfill":            classAdmin,
	"POST /api/system/recompress/trigger": classAdmin,

	// The setup wizard runs before authentication exists; once setup is done
	// these are configuration writes.
	"POST /api/setup":                         classAdmin,
	"POST /api/setup/complete":                classAdmin,
	"POST /api/setup/verify":                  classAdmin,
	"POST /api/setup/test-rtsp":               classAdmin,
	"POST /api/setup/codecs/openh264/install": classAdmin,
	"POST /api/discover/probe":                classAdmin,
	"POST /api/cameras":                       classAdmin,

	// Live media and camera control: no stored state changes.
	"POST /api/cameras/{name}/webrtc/offer":               classWrite,
	"POST /api/cameras/{name}/talkback/offer":             classWrite,
	"POST /api/cameras/{name}/ptz":                        classWrite,
	"POST /api/cameras/{name}/doorbell":                   classWrite,
	"POST /api/cameras/{name}/doorbell/{event_id}/answer": classWrite,

	// Curating what the NVR already recorded: labels, identities, thumbnails.
	"POST /api/activities/{id}/evidence/{eventId}/exclude": classWrite,
	"POST /api/activities/{id}/evidence/{eventId}/restore": classWrite,
	"POST /api/events/{id}/assign-person":                  classWrite,
	"POST /api/events/{id}/identify":                       classWrite,
	"POST /api/events/{id}/track-person":                   classWrite,
	"POST /api/events/{id}/clip":                           classWrite,
	"PUT /api/faces/{id}/assign":                           classWrite,
	"POST /api/faces/{id}/ignore":                          classWrite,
	"POST /api/objects":                                    classWrite,
	"PUT /api/objects/{id}":                                classWrite,
	"POST /api/objects/{id}/references":                    classWrite,
	"POST /api/objects/{id}/thumbnail":                     classWrite,
	"POST /api/cameras/{name}/objects":                     classWrite,
	"PUT /api/people/{id}":                                 classWrite,

	// Per-client self service and connectivity checks: nothing persisted that
	// another client can see.
	"POST /api/push/subscriptions":        classWrite,
	"DELETE /api/push/subscriptions/{id}": classWrite,
	"PUT /api/push/prefs":                 classWrite,
	"POST /api/push/test":                 classWrite,
	"POST /api/settings/mqtt/test":        classWrite,
	"POST /api/cameras/test-rtsp":         classWrite,
	"POST /api/updates/dismiss":           classWrite,

	// Token management has its own scope pair.
	"POST /api/tokens":        classTokens,
	"DELETE /api/tokens/{id}": classTokens,
}

type route struct {
	method  string
	pattern string
	source  string
}

func (r route) key() string { return r.method + " " + r.pattern }

// concretePath substitutes a literal for every {placeholder} segment so the
// pattern can be handed to Principal.Allows, which matches concrete paths.
func (r route) concretePath() string {
	segments := strings.Split(r.pattern, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segments[i] = "x"
		}
	}
	return strings.Join(segments, "/")
}

var muxRoutePattern = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)"`)

// allRoutes collects every route the server serves: the ones generated from the
// OpenAPI spec, and the ones registered by hand on the mux. The hand-registered
// set is read out of the package source, so a route added tomorrow shows up here
// without anybody remembering to update a list.
func allRoutes(t *testing.T) []route {
	t.Helper()

	var routes []route

	swagger, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for path, item := range swagger.Paths.Map() {
		for method := range item.Operations() {
			routes = append(routes, route{method: method, pattern: path, source: "openapi.yaml"})
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, m := range muxRoutePattern.FindAllStringSubmatch(string(src), -1) {
			method, pattern := m[1], m[2]
			// Trailing-slash registrations are prefix gates (the setup-mode
			// blocker), not endpoints of their own.
			if strings.HasSuffix(pattern, "/") {
				continue
			}
			routes = append(routes, route{method: method, pattern: pattern, source: file})
		}
	}

	if len(routes) < 100 {
		t.Fatalf("collected only %d routes, so the collector is broken, not the policy", len(routes))
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].key() < routes[j].key() })

	// A path may be registered twice (setup mode and full mode register the
	// discovery endpoints separately); one policy question per route is enough.
	unique := routes[:0]
	for i, r := range routes {
		if i > 0 && r.key() == routes[i-1].key() {
			continue
		}
		unique = append(unique, r)
	}
	return unique
}

func isSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// Every mutating route must demand the scope routeScopes records for it. The
// point of walking the route list rather than listing a few paths is that a new
// POST/PUT/PATCH/DELETE route fails this test until somebody decides whether an
// api:write token may call it.
func TestEveryMutatingRouteRequiresItsDeclaredScope(t *testing.T) {
	apiWrite := &auth.Principal{Username: "t", Kind: auth.AuthKindToken, Scopes: []string{"api:write"}}
	apiStar := &auth.Principal{Username: "t", Kind: auth.AuthKindToken, Scopes: []string{"api:*"}}
	apiRead := &auth.Principal{Username: "t", Kind: auth.AuthKindToken, Scopes: []string{"api:read"}}
	adminTok := &auth.Principal{Username: "t", Kind: auth.AuthKindToken, Scopes: []string{"admin"}}
	tokensTok := &auth.Principal{Username: "t", Kind: auth.AuthKindToken, Scopes: []string{"tokens:write"}}
	session := &auth.Principal{Username: "t", Kind: auth.AuthKindSession}

	routes := allRoutes(t)
	seen := map[string]bool{}
	for _, r := range routes {
		if isSafe(r.method) {
			continue
		}
		seen[r.key()] = true

		class, ok := routeScopes[r.key()]
		if !ok {
			t.Errorf("route %s (%s) is not classified in routeScopes: decide whether an api:write "+
				"token may call it and add it as classAdmin or classWrite", r.key(), r.source)
			continue
		}

		path := r.concretePath()
		switch class {
		case classAdmin:
			if apiWrite.Allows(r.method, path) {
				t.Errorf("%s is classAdmin but an api:write token may call it", r.key())
			}
			if apiStar.Allows(r.method, path) {
				t.Errorf("%s is classAdmin but an api:* token may call it", r.key())
			}
			if !adminTok.Allows(r.method, path) {
				t.Errorf("%s is classAdmin but the admin scope is refused", r.key())
			}
		case classWrite:
			if !apiWrite.Allows(r.method, path) {
				t.Errorf("%s is classWrite but an api:write token is refused", r.key())
			}
			if apiRead.Allows(r.method, path) {
				t.Errorf("%s is classWrite but a read-only token may call it", r.key())
			}
		case classTokens:
			if !tokensTok.Allows(r.method, path) {
				t.Errorf("%s is classTokens but tokens:write is refused", r.key())
			}
			if apiWrite.Allows(r.method, path) {
				t.Errorf("%s is classTokens but a plain api:write token may call it", r.key())
			}
		case classPublic:
			// Reached before the scope check; nothing to assert here beyond
			// the middleware exemption, which isPublicPath covers.
		}

		if class != classPublic && !session.Allows(r.method, path) {
			t.Errorf("%s refuses a logged-in session, which must keep full access", r.key())
		}
	}

	for key := range routeScopes {
		if !seen[key] {
			t.Errorf("routeScopes lists %q, which is not a route any more: remove it so the table "+
				"keeps meaning what it says", key)
		}
	}

	var admin, write int
	for _, class := range routeScopes {
		switch class {
		case classAdmin:
			admin++
		case classWrite:
			write++
		}
	}
	t.Logf("%d routes walked, %d mutating: %d admin-only, %d downgraded to api:write",
		len(routes), len(seen), admin, write)
}

// The read side needs no per-route table: every API read is api:read, and
// anything outside /api/ is session-only. Walking the routes still catches a new
// GET that lands somewhere a read token cannot reach.
func TestReadRoutesAcceptTheReadScope(t *testing.T) {
	apiRead := &auth.Principal{Username: "t", Kind: auth.AuthKindToken, Scopes: []string{"api:read"}}

	for _, r := range allRoutes(t) {
		if !isSafe(r.method) {
			continue
		}
		path := r.concretePath()
		switch {
		// Token management and /metrics carry their own scopes, covered by
		// dedicated tests in internal/auth.
		case path == "/metrics" || strings.HasPrefix(path, "/api/tokens"):
			continue
		case strings.HasPrefix(path, "/api/"):
			if !apiRead.Allows(r.method, path) {
				t.Errorf("%s refuses an api:read token", r.key())
			}
		default:
			if apiRead.Allows(r.method, path) {
				t.Errorf("%s is outside /api/ but an API token may call it", r.key())
			}
		}
	}
}
