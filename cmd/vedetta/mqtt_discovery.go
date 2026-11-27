package main

import (
	"log/slog"
	"sync"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/mqtt"
	"github.com/rvben/vedetta/internal/storage"
)

// publishHomeAssistantDiscovery announces every Vedetta entity to Home
// Assistant. Discovery messages are retained broker state, so a broker restart
// or a wiped retained tree leaves Home Assistant with no entities at all until
// they are published again. It therefore runs at startup and again on every
// broker reconnect, and reads the current camera, object and zone sets each
// time so a late announcement reflects the running configuration.
func publishHomeAssistantDiscovery(cfg *config.Config, db *storage.DB, mc *mqtt.Client) {
	if mc == nil {
		return
	}

	var cameraNames []string
	for _, cam := range cfg.Cameras {
		if cam.IsEnabled() {
			cameraNames = append(cameraNames, cam.Name)
		}
	}
	mc.PublishDiscovery(cameraNames)

	if knownObjects, err := db.ListKnownObjects(); err == nil {
		objInfos := make([]mqtt.ObjectInfo, 0, len(knownObjects))
		for _, obj := range knownObjects {
			objInfos = append(objInfos, mqtt.ObjectInfo{Name: obj.Name, Label: obj.Label})
		}
		mc.PublishObjectDiscovery(objInfos)
	} else {
		slog.Warn("known objects unavailable, skipping object discovery", "error", err)
	}

	// Publish doorbell discovery for enabled-doorbell cameras; clear it for
	// others so Home Assistant drops the entity when doorbell is turned off.
	var doorbellCams, nonDoorbellCams []string
	for _, cam := range cfg.Cameras {
		if !cam.IsEnabled() {
			continue
		}
		if cam.Doorbell.Enabled {
			doorbellCams = append(doorbellCams, cam.Name)
		} else {
			nonDoorbellCams = append(nonDoorbellCams, cam.Name)
		}
	}
	mc.PublishDoorbellDiscovery(doorbellCams)
	mc.ClearDoorbellDiscovery(nonDoorbellCams)

	listening, notListening := cryRecognitionCameras(cfg.Cameras)
	mc.PublishCryRecognitionDiscovery(listening)
	mc.ClearCryRecognitionDiscovery(notListening)

	if zoneInfos := presenceZoneInfos(cfg, db); len(zoneInfos) > 0 {
		mc.PublishPresenceDiscovery(zoneInfos)
	}

	mc.PublishDiskDiscovery()
}

// cryRecognitionCameras splits the cameras into those that run crying
// recognition and those that do not. The second group includes disabled
// cameras, so turning a camera off also drops its sensor from Home Assistant.
func cryRecognitionCameras(cameras []config.CameraConfig) (listening, notListening []string) {
	for _, cam := range cameras {
		if cam.IsEnabled() && cam.Cry.Enabled {
			listening = append(listening, cam.Name)
		} else {
			notListening = append(notListening, cam.Name)
		}
	}
	return listening, notListening
}

// cryRecognitionState is the recognition state published for a camera. A
// camera its operator stopped reports "stopped" whatever its monitor last
// said, and a camera without recognition reports nothing.
func cryRecognitionState(st camera.CameraStatus) (string, bool) {
	switch {
	case st.Cry == nil:
		return "", false
	case st.Stopped:
		return "stopped", true
	}
	return st.Cry.State, true
}

// presenceZoneInfos collects the zone/label pairs that track presence.
func presenceZoneInfos(cfg *config.Config, db *storage.DB) []mqtt.ZoneInfo {
	var zoneInfos []mqtt.ZoneInfo
	for _, camCfg := range cfg.Cameras {
		if !camCfg.IsEnabled() {
			continue
		}
		zones, err := db.ListZones(camCfg.Name)
		if err != nil {
			slog.Warn("zones unavailable, skipping presence discovery",
				"camera", camCfg.Name, "error", err)
			continue
		}
		for _, z := range zones {
			if !z.TrackPresence || !z.Enabled {
				continue
			}
			for _, label := range z.Labels {
				zoneInfos = append(zoneInfos, mqtt.ZoneInfo{ZoneName: z.Name, Label: label})
			}
		}
	}
	return zoneInfos
}

// connectHook is the announcer's view of an MQTT client: something that can
// tell it when a broker connection has been established.
type connectHook interface {
	SetOnConnect(func())
}

// mqttAnnouncer publishes Home Assistant discovery whenever a broker
// connection is established. The two halves arrive in either order: the broker
// can be down at boot and a client appear from the background reconnect much
// later, or the client can exist before the announcement is configured. Each
// half records itself and the announcement runs exactly once, as soon as both
// are present.
type mqttAnnouncer struct {
	mu       sync.Mutex
	announce func()
	client   connectHook
}

// setAnnounce installs the announcement and runs it against an already
// attached client.
func (a *mqttAnnouncer) setAnnounce(fn func()) {
	a.mu.Lock()
	a.announce = fn
	attached := a.client != nil
	a.mu.Unlock()
	if fn != nil && attached {
		fn()
	}
}

// attach makes client the current connection, announces to it, and
// re-announces on every subsequent reconnect.
func (a *mqttAnnouncer) attach(client connectHook) {
	if client == nil {
		return
	}
	a.mu.Lock()
	a.client = client
	announce := a.announce
	a.mu.Unlock()

	client.SetOnConnect(a.run)
	if announce != nil {
		announce()
	}
}

// run announces using the currently installed announcement.
func (a *mqttAnnouncer) run() {
	a.mu.Lock()
	announce := a.announce
	a.mu.Unlock()
	if announce != nil {
		announce()
	}
}
