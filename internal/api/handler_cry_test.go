package api

import (
	"bytes"
	"encoding/json"
	"github.com/rvben/vedetta/internal/notify"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

func TestCrySnoozePersistsAndResumes(t *testing.T) {
	s, db := newTestServer(t)
	s.cameras.AddCamera(config.CameraConfig{Name: "nursery", URL: "rtsp://example.invalid", Cry: config.CryCameraConfig{Enabled: true, Alerts: true}})
	s.cameras.ConfigureCry(nil, "no model", config.CryConfig{Threshold: .2, CooldownSeconds: 60})
	for _, minutes := range []string{"30", "0"} {
		r := httptest.NewRequest(http.MethodPost, "/api/cameras/nursery/cry/snooze", bytes.NewBufferString(`{"minutes":`+minutes+`}`))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		value, err := db.GetSetting("cry_snooze:nursery")
		if err != nil {
			t.Fatal(err)
		}
		status := s.cameras.GetCamera("nursery").Status().Cry
		if minutes == "30" {
			if !status.SnoozedUntil.After(time.Now()) || value == "" {
				t.Fatal("snooze not persisted")
			}
		} else if !status.SnoozedUntil.IsZero() || value != "" {
			t.Fatal("resume not persisted")
		}
	}
	for _, body := range []string{`{}`, `{"minutes":-1}`, `{"minutes":1441}`, `{"minutes":1.5}`, `bad`} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/cameras/nursery/cry/snooze", bytes.NewBufferString(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s", body)
		}
	}
}

type cryTestDelivery struct{ state string }

func (d cryTestDelivery) CryDeliveryStatus(string) notify.DeliveryStatus {
	return notify.DeliveryStatus{State: d.state}
}

func TestCameraReportsProviderHealthSeparatelyFromCryOptIn(t *testing.T) {
	s, _ := newTestServer(t)
	s.cameras.AddCamera(config.CameraConfig{Name: "nursery", URL: "rtsp://example.invalid", Cry: config.CryCameraConfig{Enabled: true, Alerts: true}})
	s.cameras.ConfigureCry(nil, "no model", config.CryConfig{Threshold: .2, CooldownSeconds: 60})
	for _, state := range []string{"unverified", "accepted", "failed", "excluded"} {
		s.SetCryDeliveryProvider(cryTestDelivery{state: state}, true)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cameras/nursery", nil))
		var body struct {
			Cry struct {
				Alerts bool `json:"alerts"`
			} `json:"cry"`
			Delivery notify.DeliveryStatus `json:"cry_delivery"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Cry.Alerts || body.Delivery.State != state {
			t.Fatalf("mixed opt-in with delivery health: %s", w.Body.String())
		}
	}
	s.SetCryDeliveryProvider(nil, true)
	if s.cryDeliveryStatus("nursery").State != "unavailable" {
		t.Fatal("missing provider reported healthy")
	}
	s.SetCryDeliveryProvider(nil, false)
	if s.cryDeliveryStatus("nursery").State != "disabled" {
		t.Fatal("disabled provider reported healthy")
	}
}
