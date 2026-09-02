package mqtt

import (
	"sync"
	"sync/atomic"

	"github.com/rvben/vedetta/internal/config"
)

// Holder is the single owner of the live MQTT client.
//
// Several components publish through the same connection: the event loop, the
// presence and camera-status tickers, the disk-status publisher and the API
// server's connectivity readout. When the operator saves new broker settings
// the client is replaced, and every one of those readers has to end up on the
// new one. A component that kept its own copy would go on publishing into a
// client this swap already closed, which looks exactly like a working setup and
// delivers nothing.
//
// Load is safe from any goroutine and returns nil while MQTT is off or the
// broker is unreachable; callers check for nil on every use rather than caching
// the result.
type Holder struct {
	ptr atomic.Pointer[Client]

	// mu serializes Replace and Close against each other so two concurrent
	// settings saves cannot both close the client one of them just installed.
	mu     sync.Mutex
	onSwap func(*Client)

	// gen counts installs. The startup reconnect loop dials the broker named in
	// the config at boot and can still be dialling minutes later; an operator
	// who saves different settings, or turns MQTT off, in that window must not
	// have the old broker's client land on top of the decision. The loop
	// captures gen before it starts and offers its client against that value.
	gen uint64
}

// NewHolder returns an empty holder. MQTT is off until a client is installed.
func NewHolder() *Holder {
	return &Holder{}
}

// Load returns the current client, or nil when none is installed.
func (h *Holder) Load() *Client {
	if h == nil {
		return nil
	}
	return h.ptr.Load()
}

// Connected reports whether a client is installed. It is what a status readout
// wants: "enabled in the config" and "actually connected" are different facts,
// and only this one changes when the broker goes away at boot.
func (h *Holder) Connected() bool {
	return h.Load() != nil
}

// Store installs a client without closing whatever was there. It exists for the
// startup and background-reconnect paths, which install into an empty holder.
// Use Replace to change brokers.
func (h *Holder) Store(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.install(c)
}

// Generation reports how many clients have been installed. A background
// reconnect captures it before it starts dialling and passes it back to
// StoreIfGeneration, which is how "nothing has happened since I started" is
// told apart from "the operator has since replaced what I am retrying".
func (h *Holder) Generation() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen
}

// StoreIfGeneration installs c only while the holder is still at gen, and
// reports whether it did. A refused install leaves c untouched and connected,
// so the caller closes it.
func (h *Holder) StoreIfGeneration(gen uint64, c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != gen {
		return false
	}
	h.install(c)
	return true
}

// SetOnSwap registers a callback run with each newly installed client, and with
// nil when MQTT is turned off. It is how Home Assistant discovery is announced
// on a connection that appears after startup: a new broker has none of the
// retained discovery state the old one held.
func (h *Holder) SetOnSwap(fn func(*Client)) {
	h.mu.Lock()
	h.onSwap = fn
	current := h.ptr.Load()
	h.mu.Unlock()
	if fn != nil && current != nil {
		fn(current)
	}
}

// Replace closes the installed client and connects with cfg. A cfg that is not
// enabled closes without reconnecting. The old client is closed before the new
// one is built, so a broker that refuses the new settings leaves MQTT off
// rather than leaving the previous connection installed under settings the
// operator has already replaced.
func (h *Holder) Replace(cfg config.MQTTConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if old := h.ptr.Load(); old != nil {
		old.Close()
	}
	if !cfg.Enabled {
		h.install(nil)
		return nil
	}

	client, err := New(cfg)
	if err != nil {
		h.install(nil)
		return err
	}
	h.install(client)
	return nil
}

// Close closes and uninstalls the client. Safe to call when none is installed.
func (h *Holder) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.ptr.Load(); c != nil {
		c.Close()
	}
	h.ptr.Store(nil)
	// A shutdown counts as a change too: a reconnect that succeeds during it
	// must not install into a holder nothing will ever close again.
	h.gen++
}

// install publishes the client and notifies the swap hook. Called with h.mu
// held so the notification order matches the installation order.
func (h *Holder) install(c *Client) {
	h.ptr.Store(c)
	h.gen++
	if h.onSwap != nil {
		h.onSwap(c)
	}
}
