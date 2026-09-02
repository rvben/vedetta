package auth

import (
	"net/http"
	"testing"
)

// Mutation is admin by default. A route nobody has classified must be closed to
// api:write tokens, so that adding an endpoint cannot silently hand every write
// token a new capability. These paths stand in for the route added tomorrow.
func TestUnclassifiedMutatingRouteRequiresAdmin(t *testing.T) {
	apiWrite := &Principal{Username: "t", Kind: AuthKindToken, Scopes: []string{"api:write"}}
	apiStar := &Principal{Username: "t", Kind: AuthKindToken, Scopes: []string{"api:*"}}

	for _, tc := range [][2]string{
		{http.MethodPost, "/api/storage/delete"},
		{http.MethodPost, "/api/storage/cleanup"},
		{http.MethodPost, "/api/some-route-added-tomorrow"},
		{http.MethodDelete, "/api/people/7"},
		{http.MethodPatch, "/api/objects/7"},
	} {
		if apiWrite.Allows(tc[0], tc[1]) {
			t.Errorf("api:write is allowed on the unclassified route %s %s", tc[0], tc[1])
		}
		if apiStar.Allows(tc[0], tc[1]) {
			t.Errorf("api:* is allowed on the unclassified route %s %s", tc[0], tc[1])
		}
	}
}

// The allowlist matches whole segments. A pattern must not admit a longer path
// that merely starts the same way, which is how a prefix check leaks a
// sub-resource.
func TestNonAdminAllowlistMatchesWholeSegments(t *testing.T) {
	apiWrite := &Principal{Username: "t", Kind: AuthKindToken, Scopes: []string{"api:write"}}

	if !apiWrite.Allows(http.MethodPut, "/api/objects/7") {
		t.Fatal("api:write must still update an object")
	}
	for _, tc := range [][2]string{
		{http.MethodPut, "/api/objects/7/extra"},
		{http.MethodPut, "/api/objects"},
		{http.MethodPost, "/api/push/subscriptions/7/extra"},
		{http.MethodPost, "/api/cameras/front/ptz/extra"},
	} {
		if apiWrite.Allows(tc[0], tc[1]) {
			t.Errorf("allowlist pattern leaked onto %s %s", tc[0], tc[1])
		}
	}
}
