package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// AnswerDoorbell acknowledges a doorbell ring. Idempotent: UpdateEventAnswered
// only sets the columns while answered_at IS NULL in the DB, so the first
// answerer wins. The response always reflects the persisted answer state.
func (s *Server) AnswerDoorbell(w http.ResponseWriter, r *http.Request, name string, eventID string) {
	ev, err := s.db.GetEventByID(eventID)
	if err != nil || ev == nil || ev.ID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "ring not found"})
		return
	}
	if ev.Kind != camera.EventKindDoorbell || ev.CameraName != name {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "ring not found"})
		return
	}

	if ev.AnsweredAt.IsZero() {
		answeredBy := ""
		if p := principalFromContext(r.Context()); p != nil {
			answeredBy = p.Username
		}
		now := time.Now()
		if err := s.db.UpdateEventAnswered(eventID, now, answeredBy); err != nil {
			s.serverError(w, r, err)
			return
		}
		// The write succeeded; reflect it on the already-loaded ev so the response
		// and SSE never depend on the re-read. Prefer the persisted row (first
		// answerer under a race) when the re-read succeeds.
		ev.AnsweredAt = now
		ev.AnsweredBy = answeredBy
		if updated, rerr := s.db.GetEventByID(eventID); rerr == nil && updated != nil {
			ev = updated
		}

		s.broadcastSSE("doorbell_answered", map[string]string{
			"event_id":    eventID,
			"camera":      name,
			"answered_by": ev.AnsweredBy,
		})
		if s.doorbellMetrics != nil {
			s.doorbellMetrics.recordAnswer(name)
		}
		slog.Info("doorbell answered", "camera", name, "event", eventID, "by", ev.AnsweredBy)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"event_id":    eventID,
		"answered_at": ev.AnsweredAt.UTC().Format(time.RFC3339),
		"answered_by": ev.AnsweredBy,
	})
}
