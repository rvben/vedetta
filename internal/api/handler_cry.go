package api

import (
	"encoding/json"
	"github.com/rvben/vedetta/internal/notify"
	"net/http"
	"time"
)

// SnoozeCry suppresses cry notifications for every provider while preserving
// recognition and evidence. It survives service restarts; zero resumes alerts.
func (s *Server) SnoozeCry(w http.ResponseWriter, r *http.Request, name string) {
	cam := s.cameras.GetCamera(name)
	if cam == nil || cam.Status().Cry == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Crying recognition is not enabled for this camera"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	defer r.Body.Close()
	var body struct {
		Minutes *int `json:"minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Minutes == nil || *body.Minutes < 0 || *body.Minutes > 1440 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Snooze must be between 0 and 1440 minutes"})
		return
	}
	var until time.Time
	value := ""
	if *body.Minutes > 0 {
		until = time.Now().UTC().Add(time.Duration(*body.Minutes) * time.Minute)
		value = until.Format(time.RFC3339)
	}
	if err := cam.PersistCrySnooze(until, func() error { return s.db.SetSetting("cry_snooze:"+name, value) }); err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cam.Status().Cry)
}

func (s *Server) SetCryDeliveryProvider(provider any, enabled bool) {
	s.cryDelivery.Store(func(name string) notify.DeliveryStatus {
		if !enabled {
			return notify.DeliveryStatus{State: "disabled"}
		}
		if reporter, ok := provider.(notify.DeliveryReporter); ok {
			return reporter.CryDeliveryStatus(name)
		}
		return notify.DeliveryStatus{State: "unavailable"}
	})
}

func (s *Server) cryDeliveryStatus(name string) notify.DeliveryStatus {
	if report := s.cryDelivery.Load(); report != nil {
		return report.(func(string) notify.DeliveryStatus)(name)
	}
	return notify.DeliveryStatus{State: "unavailable"}
}
