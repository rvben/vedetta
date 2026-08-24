package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/notify"
	"github.com/rvben/vedetta/internal/storage"
)

// requireInteractiveUser enforces that the caller is a real human browsing
// the UI, not a long-lived API token. Session cookies (direct Vedetta login)
// and proxy principals (authenticated upstream by Authelia/equivalent via a
// trusted Remote-User header) are both accepted. Bearer API tokens are
// rejected — push subscriptions are per-device and per-human; giving a
// service token the ability to register or manipulate them would let an
// automation silently route notifications to arbitrary endpoints.
func (s *Server) requireInteractiveUser(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p := principalFromContext(r.Context())
	if p == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authentication required"})
		return nil, false
	}
	if p.Kind != auth.AuthKindSession && p.Kind != auth.AuthKindProxy {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "interactive user authentication required (not API token)"})
		return nil, false
	}
	return p, true
}

// GetVAPIDPublicKey returns the server's VAPID public key so the browser's
// service worker can construct a PushSubscription.
func (s *Server) GetVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInteractiveUser(w, r); !ok {
		return
	}
	if s.notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notifications disabled"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": s.notifier.VAPIDPublicKey()})
}

type subscriptionBody struct {
	Endpoint  string `json:"endpoint"`
	UserAgent string `json:"userAgent"`
	Keys      struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// CreatePushSubscription registers a new browser push subscription for the
// authenticated user. Endpoint and keys are validated before persisting.
func (s *Server) CreatePushSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInteractiveUser(w, r)
	if !ok {
		return
	}
	var body subscriptionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := notify.ValidateSubscriptionEndpoint(body.Endpoint); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := notify.ValidateSubscriptionKeys(body.Keys.P256dh, body.Keys.Auth); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(body.UserAgent) > notify.MaxUserAgentLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "userAgent too long"})
		return
	}
	id, err := s.db.SavePushSubscription(storage.PushSubscription{
		Username:  p.Username,
		Endpoint:  body.Endpoint,
		P256dh:    body.Keys.P256dh,
		Auth:      body.Keys.Auth,
		UserAgent: body.UserAgent,
	})
	if errors.Is(err, storage.ErrSubscriptionOwnedByOther) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "subscription endpoint already registered"})
		return
	}
	if err != nil {
		slog.Error("push subscribe: save failed", "username", p.Username, "error", err)
		s.serverError(w, r, err)
		return
	}
	slog.Info("push subscribe: registered", "username", p.Username, "id", id, "user_agent", body.UserAgent)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// ListPushSubscriptions lists the caller's own push subscriptions. The
// raw endpoint and keys are never returned — only metadata the user needs
// to identify and manage their devices.
func (s *Server) ListPushSubscriptions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInteractiveUser(w, r)
	if !ok {
		return
	}
	list, err := s.db.ListPushSubscriptionsByUser(p.Username)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type subOut struct {
		ID        int64  `json:"id"`
		UserAgent string `json:"userAgent,omitempty"`
		CreatedAt string `json:"createdAt"`
	}
	out := make([]subOut, 0, len(list))
	for _, sub := range list {
		out = append(out, subOut{
			ID:        sub.ID,
			UserAgent: sub.UserAgent,
			CreatedAt: sub.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DeletePushSubscription removes a subscription owned by the caller.
// A mismatch (wrong user, wrong id, or nonexistent id) always returns
// 404 — the spec deliberately refuses to distinguish "not yours" from
// "not found" to avoid leaking existence to other users.
func (s *Server) DeletePushSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInteractiveUser(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.db.DeletePushSubscription(id, p.Username); err != nil {
		if errors.Is(err, storage.ErrPushSubscriptionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// prefsResponse is the shape of both GET and PUT /api/push/prefs payloads.
// Cameras is a nested map: camera name → object class → enabled.
type prefsResponse struct {
	Muted           bool                       `json:"muted"`
	CooldownSeconds int                        `json:"cooldown_seconds"`
	Cameras         map[string]map[string]bool `json:"cameras"`
}

// GetPushPrefs returns the caller's current notification preferences. The
// cameras map is seeded with all known (camera, class) pairs as enabled,
// then flipped false for any explicit disable rows from notification_prefs.
func (s *Server) GetPushPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInteractiveUser(w, r)
	if !ok {
		return
	}
	muted, _, _ := s.db.GetKV("notify:" + p.Username + ":muted")
	cdRaw, _, _ := s.db.GetKV("notify:" + p.Username + ":cooldown_seconds")
	cd, _ := strconv.Atoi(cdRaw)
	if cd == 0 {
		cd = 180
	}
	disabled, err := s.db.ListNotificationPrefs(p.Username)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	resp := prefsResponse{
		Muted:           muted == "1",
		CooldownSeconds: cd,
		Cameras:         map[string]map[string]bool{},
	}
	for _, cam := range s.cameraNames() {
		resp.Cameras[cam] = map[string]bool{}
		for _, class := range knownObjectClasses() {
			resp.Cameras[cam][class] = true
		}
	}
	for _, d := range disabled {
		if _, ok := resp.Cameras[d.Camera]; !ok {
			resp.Cameras[d.Camera] = map[string]bool{}
		}
		if d.ObjectClass == "*" {
			for class := range resp.Cameras[d.Camera] {
				resp.Cameras[d.Camera][class] = false
			}
		} else {
			resp.Cameras[d.Camera][d.ObjectClass] = false
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// PutPushPrefs updates the caller's notification preferences. Muted flag
// and cooldown seconds live in kv_store; per-(camera,class) rows live in
// notification_prefs.
func (s *Server) PutPushPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInteractiveUser(w, r)
	if !ok {
		return
	}
	var body prefsResponse
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	mutedVal := "0"
	if body.Muted {
		mutedVal = "1"
	}
	if err := s.db.SetKV("notify:"+p.Username+":muted", mutedVal); err != nil {
		s.serverError(w, r, err)
		return
	}
	if body.CooldownSeconds > 0 {
		if err := s.db.SetKV("notify:"+p.Username+":cooldown_seconds", strconv.Itoa(body.CooldownSeconds)); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	for cam, classes := range body.Cameras {
		for class, enabled := range classes {
			if err := s.db.SetNotificationPref(p.Username, cam, class, enabled); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// TestPush sends a delivery check to every subscription owned by the caller.
// The latest retained snapshot from the first configured camera is included
// when available; otherwise clients use the normal Vedetta icon fallback.
func (s *Server) TestPush(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInteractiveUser(w, r)
	if !ok {
		return
	}
	if s.notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notifications disabled"})
		return
	}
	cams := s.cameraNames()
	if len(cams) == 0 {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "no cameras configured"})
		return
	}
	snapshotEventID := ""
	latest, err := s.db.LatestSnapshotEvent(cams[0])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if latest != nil {
		snapshotEventID = latest.ID
	}
	s.notifier.EnqueueTest(p.Username, cams[0], snapshotEventID)
	w.WriteHeader(http.StatusAccepted)
}

// GetPushSnapshot serves an event snapshot bypassing session auth, gated
// only by an HMAC signature in the query string. Used by iOS push
// notification thumbnails, which iOS fetches without cookies in SW scope.
// The signature is minted by the dispatcher when building the push
// payload; see notify.SnapshotSigner.
func (s *Server) GetPushSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		http.NotFound(w, r)
		return
	}
	eventID := r.PathValue("id")
	exp := r.URL.Query().Get("e")
	sig := r.URL.Query().Get("s")
	if err := s.notifier.VerifySnapshot(eventID, exp, sig); err != nil {
		slog.Debug("push snapshot: bad signature", "event", eventID, "error", err)
		http.NotFound(w, r)
		return
	}
	ev, err := s.db.GetEventByID(eventID)
	if err != nil || ev == nil || !ev.SnapshotAvailable || ev.SnapshotPath == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, ev.SnapshotPath)
}

// cameraNames returns the list of configured camera names used by the prefs
// handler to seed the response. Populated by SetCameraNames at startup.
func (s *Server) cameraNames() []string {
	return s.cameraNamesCached
}

// knownObjectClasses returns the labels Vedetta currently notifies on. There
// is no canonical source: config.DetectConfig.Labels is user-configured and
// may be empty, and the YOLO detector accepts any COCO label. The short list
// here covers what the user will actually want to toggle in the settings UI.
//
// TODO: move to detect package when a canonical label registry exists.
func knownObjectClasses() []string {
	return []string{"person", "car", "bicycle", "motorcycle", "truck", "bus", "dog", "cat"}
}
