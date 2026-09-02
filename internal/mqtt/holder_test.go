package mqtt

import (
	"sync"
	"testing"

	"github.com/rvben/vedetta/internal/config"
)

func TestHolderEmptyHolderIsNotConnected(t *testing.T) {
	var h Holder
	if h.Load() != nil {
		t.Fatal("expected no client in a fresh holder")
	}
	if h.Connected() {
		t.Fatal("expected a fresh holder to report disconnected")
	}
}

func TestHolderNilReceiverLoadsNil(t *testing.T) {
	var h *Holder
	if h.Load() != nil {
		t.Fatal("expected a nil holder to load nil rather than panic")
	}
	if h.Connected() {
		t.Fatal("expected a nil holder to report disconnected")
	}
}

func TestHolderStoreInstallsTheClient(t *testing.T) {
	var h Holder
	c := &Client{}
	h.Store(c)
	if h.Load() != c {
		t.Fatal("expected the stored client to be readable")
	}
	if !h.Connected() {
		t.Fatal("expected a holder with a client to report connected")
	}
}

// Discovery is retained broker state, so every client that gets installed has
// to be announced to. A hook registered before the client sees it on install.
func TestHolderSwapHookRunsOnInstall(t *testing.T) {
	var h Holder
	var got []*Client
	h.SetOnSwap(func(c *Client) { got = append(got, c) })

	if len(got) != 0 {
		t.Fatalf("expected no notification for an empty holder, got %d", len(got))
	}

	c := &Client{}
	h.Store(c)

	if len(got) != 1 || got[0] != c {
		t.Fatalf("expected one notification for the installed client, got %v", got)
	}
}

// The reverse order matters just as much: the background retry can install a
// client before the announcement is wired up, and that client still needs the
// entities published to it.
func TestHolderSwapHookRunsForAClientInstalledEarlier(t *testing.T) {
	var h Holder
	c := &Client{}
	h.Store(c)

	var got []*Client
	h.SetOnSwap(func(cl *Client) { got = append(got, cl) })

	if len(got) != 1 || got[0] != c {
		t.Fatalf("expected the already-installed client to be announced, got %v", got)
	}
}

// Replace with MQTT turned off uninstalls the client instead of leaving the
// previous broker connected under settings the operator has replaced.
func TestHolderReplaceDisabledUninstalls(t *testing.T) {
	var h Holder
	h.Store(&Client{})

	var swaps []*Client
	h.SetOnSwap(func(c *Client) { swaps = append(swaps, c) })
	swaps = swaps[:0]

	if err := h.Replace(config.MQTTConfig{Enabled: false}); err != nil {
		t.Fatalf("disabling should not error: %v", err)
	}
	if h.Load() != nil {
		t.Fatal("expected no client after MQTT is disabled")
	}
	if h.Connected() {
		t.Fatal("expected disconnected after MQTT is disabled")
	}
	if len(swaps) != 1 || swaps[0] != nil {
		t.Fatalf("expected one nil notification for the uninstall, got %v", swaps)
	}
}

// A broker that refuses the new settings must leave MQTT off. Keeping the
// previous client would publish to a broker the operator has just replaced.
func TestHolderReplaceFailureLeavesNoClient(t *testing.T) {
	var h Holder
	h.Store(&Client{})

	// Port 0 is not connectable, so New fails without reaching a network.
	err := h.Replace(config.MQTTConfig{Enabled: true, Host: "192.0.2.1", Port: 0})
	if err == nil {
		t.Fatal("expected an unreachable broker to error")
	}
	if h.Load() != nil {
		t.Fatal("expected no client after a failed replace")
	}
	if h.Connected() {
		t.Fatal("expected disconnected after a failed replace")
	}
}

func TestHolderCloseUninstalls(t *testing.T) {
	var h Holder
	h.Store(&Client{})
	h.Close()
	if h.Load() != nil {
		t.Fatal("expected no client after Close")
	}
	// Closing an empty holder is a no-op, not a panic: shutdown runs it
	// whether or not MQTT ever connected.
	h.Close()
}

// Production access pattern: the reconnect goroutine installs while the event
// loop and the status tickers read. Run under -race.
func TestHolderConcurrentStoreAndLoadIsRaceFree(t *testing.T) {
	var h Holder
	const iters = 2000
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			h.Store(&Client{})
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if c := h.Load(); c != nil {
					_ = c
				}
				_ = h.Connected()
			}
		}()
	}
	wg.Wait()
}

// The startup reconnect loop dials the broker named in the config at boot. An
// operator who turns MQTT off while it is still dialling has decided; a client
// that connects afterwards must not undo that decision.
func TestHolderStoreIfGenerationRefusesAfterMQTTIsDisabled(t *testing.T) {
	var h Holder
	startGen := h.Generation()

	if err := h.Replace(config.MQTTConfig{Enabled: false}); err != nil {
		t.Fatalf("disabling should not error: %v", err)
	}

	late := &Client{}
	if h.StoreIfGeneration(startGen, late) {
		t.Fatal("a startup reconnect installed a client after MQTT was disabled")
	}
	if h.Load() != nil {
		t.Fatalf("expected MQTT to stay off, got %p", h.Load())
	}
}

// The same loop must not install the old broker's client over the one the
// operator's new settings installed. Overwriting drops the new client without
// closing it, so it stays connected and subscribed while nothing publishes
// through it, which reads as a working setup that delivers nothing.
func TestHolderStoreIfGenerationRefusesAfterAnotherInstall(t *testing.T) {
	var h Holder
	startGen := h.Generation()

	current := &Client{}
	h.Store(current)

	if h.StoreIfGeneration(startGen, &Client{}) {
		t.Fatal("a startup reconnect installed over a client from newer settings")
	}
	if h.Load() != current {
		t.Fatal("the client installed from the newer settings was replaced")
	}
}

// The accepting bound: while nothing has changed, the reconnect installs.
func TestHolderStoreIfGenerationInstallsWhenNothingChanged(t *testing.T) {
	var h Holder
	startGen := h.Generation()

	c := &Client{}
	if !h.StoreIfGeneration(startGen, c) {
		t.Fatal("a startup reconnect was refused with no competing change")
	}
	if h.Load() != c {
		t.Fatal("the reconnected client was not installed")
	}
}

// Shutdown counts as a change: closeSubsystems has already run, so a client
// installed after it would stay connected for the life of the process.
func TestHolderStoreIfGenerationRefusesAfterClose(t *testing.T) {
	var h Holder
	startGen := h.Generation()

	h.Close()

	if h.StoreIfGeneration(startGen, &Client{}) {
		t.Fatal("a startup reconnect installed a client after the holder was closed")
	}
}
