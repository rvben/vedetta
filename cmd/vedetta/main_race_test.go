package main

import (
	"sync"
	"testing"

	"github.com/rvben/vedetta/internal/mqtt"
)

// TestSubsystemsMQTTClientRaceFree exercises the production access pattern for
// subsystems.mqtt: the MQTT reconnect goroutine installs a client while
// the event loop and the disk/camera-status ticker goroutines read it. Run
// under -race. A plain *mqtt.Client field reports a data race here; a
// synchronized field does not.
func TestSubsystemsMQTTClientRaceFree(t *testing.T) {
	var sub subsystems
	const iters = 2000
	var wg sync.WaitGroup

	// Writer: mimics the reconnect goroutine (main.go) installing a client.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			sub.mqtt.Store(&mqtt.Client{})
		}
	}()

	// Readers: mimic the event loop and ticker goroutines nil-checking the
	// client before publishing.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if c := sub.mqtt.Load(); c != nil {
					_ = c
				}
			}
		}()
	}
	wg.Wait()
}
