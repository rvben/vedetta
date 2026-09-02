package mqtt

import (
	"sync"
	"testing"
	"time"
)

// The connection callback is what republishes Home Assistant discovery after a
// broker restart, so it has to run on every connect, not only the first.
func TestOnConnectCallbackRunsOnEveryConnect(t *testing.T) {
	var c Client
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{}, 3)

	c.SetOnConnect(func() {
		mu.Lock()
		calls++
		mu.Unlock()
		done <- struct{}{}
	})

	for range 3 {
		c.runOnConnect()
	}
	for range 3 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("connection callback did not run")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("callback calls = %d, want 3", calls)
	}
}

// A client with no callback registered is the normal case for a process that
// does not publish discovery. Connecting must not panic.
func TestOnConnectWithoutACallbackIsSafe(t *testing.T) {
	var c Client
	c.runOnConnect()
	c.SetOnConnect(func() {})
	c.SetOnConnect(nil)
	c.runOnConnect()
}
