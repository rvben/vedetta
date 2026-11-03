package notify

import "time"

// DeliveryStatus describes provider acceptance, never receipt on a phone.
// It contains no credentials, provider response bodies or event identifiers.
type DeliveryStatus struct {
	State        string    `json:"state"`
	LastAccepted time.Time `json:"last_accepted,omitempty"`
	LastFailure  time.Time `json:"last_failure,omitempty"`
	Queued       int       `json:"queued"`
}

type DeliveryReporter interface{ CryDeliveryStatus(string) DeliveryStatus }

func (p *Pushover) CryDeliveryStatus(camera string) DeliveryStatus {
	if !p.accepts(camera, "cry") {
		return DeliveryStatus{State: "excluded"}
	}
	p.deliveryMu.RLock()
	defer p.deliveryMu.RUnlock()
	status := p.delivery
	status.Queued = len(p.jobs)
	status.State = "unverified"
	if !status.LastAccepted.IsZero() {
		status.State = "accepted"
	}
	if status.LastFailure.After(status.LastAccepted) {
		status.State = "failed"
	}
	return status
}

func (p *Pushover) recordDelivery(accepted bool) {
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	if accepted {
		p.delivery.LastAccepted = time.Now().UTC()
	} else {
		p.delivery.LastFailure = time.Now().UTC()
	}
}

func (f *Fanout) CryDeliveryStatus(camera string) DeliveryStatus {
	for _, provider := range f.providers {
		if status, ok := provider.(DeliveryReporter); ok {
			return status.CryDeliveryStatus(camera)
		}
	}
	return DeliveryStatus{State: "disabled"}
}
