package stream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/rvben/vedetta/internal/config"
)

var (
	ErrTalkbackUnsupported = errors.New("camera has no supported ONVIF audio backchannel")
	ErrTalkbackBusy        = errors.New("another talkback session is active")
)

type TalkbackCapabilities struct {
	Supported bool   `json:"supported"`
	Codec     string `json:"codec,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type backchannel struct {
	client *gortsplib.Client
	media  *description.Media
	format *format.G711
	once   sync.Once
}

func (b *backchannel) Close() { b.once.Do(b.client.Close) }

func (b *backchannel) WriteRTP(pkt *rtp.Packet) error {
	clone := *pkt
	clone.PayloadType = b.format.PayloadType()
	return b.client.WritePacketRTP(b.media, &clone)
}

// TalkbackManager bridges a browser's microphone-only WebRTC peer to an
// ONVIF Profile T G.711 RTSP backchannel. Only one speaker can own a camera at
// a time, preventing overlapping household sessions from reaching the device.
type TalkbackManager struct {
	iceServers []webrtc.ICEServer
	mu         sync.Mutex
	active     map[string]struct{}
}

func NewTalkbackManager(iceServers []config.ICEServerConfig) *TalkbackManager {
	return &TalkbackManager{iceServers: iceServersFromConfig(iceServers), active: make(map[string]struct{})}
}

func (m *TalkbackManager) Capabilities(ctx context.Context, rtspURL string) TalkbackCapabilities {
	channel, err := openBackchannel(ctx, rtspURL)
	if err != nil {
		return TalkbackCapabilities{Reason: talkbackReason(err)}
	}
	defer channel.Close()
	return TalkbackCapabilities{Supported: true, Codec: codecName(channel.format)}
}

func talkbackReason(err error) string {
	if errors.Is(err, ErrTalkbackUnsupported) {
		return "Camera has no supported G.711 audio return channel"
	}
	return "Camera audio return channel is unavailable"
}

func codecName(f *format.G711) string {
	if f.MULaw {
		return "PCMU"
	}
	return "PCMA"
}

func openBackchannel(ctx context.Context, rawURL string) (*backchannel, error) {
	u, err := base.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse RTSP URL: %w", err)
	}
	protocol := gortsplib.ProtocolTCP
	dialer := &net.Dialer{}
	c := &gortsplib.Client{
		Scheme: u.Scheme, Host: u.Host, Protocol: &protocol,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		RequestBackChannels: true,
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}
	if err = c.Start(); err != nil {
		return nil, fmt.Errorf("connect to camera: %w", err)
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("describe backchannel: %w", err)
	}
	var selectedMedia *description.Media
	var selectedFormat *format.G711
	for _, media := range desc.Medias {
		if !media.IsBackChannel {
			continue
		}
		for _, candidate := range media.Formats {
			if g711, ok := candidate.(*format.G711); ok && g711.SampleRate == 8000 && g711.ChannelCount == 1 {
				selectedMedia, selectedFormat = media, g711
				break
			}
		}
	}
	if selectedMedia == nil {
		c.Close()
		return nil, ErrTalkbackUnsupported
	}
	if _, err = c.Setup(desc.BaseURL, selectedMedia, 0, 0); err != nil {
		c.Close()
		return nil, fmt.Errorf("setup backchannel: %w", err)
	}
	if _, err = c.Play(nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("start backchannel: %w", err)
	}
	return &backchannel{client: c, media: selectedMedia, format: selectedFormat}, nil
}

func (m *TalkbackManager) HandleOffer(ctx context.Context, cameraName, rtspURL string, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	m.mu.Lock()
	if _, exists := m.active[cameraName]; exists {
		m.mu.Unlock()
		return nil, ErrTalkbackBusy
	}
	m.active[cameraName] = struct{}{}
	m.mu.Unlock()
	release := func() {
		m.mu.Lock()
		delete(m.active, cameraName)
		m.mu.Unlock()
	}

	channel, err := openBackchannel(ctx, rtspURL)
	if err != nil {
		release()
		return nil, err
	}
	cleanupOnce := sync.Once{}
	var pc *webrtc.PeerConnection
	cleanup := func() {
		cleanupOnce.Do(func() {
			channel.Close()
			release()
			if pc != nil {
				// Close can synchronously emit ICE closed. Run it after the once
				// body returns so the callback cannot re-enter sync.Once.Do.
				go func(peer *webrtc.PeerConnection) { _ = peer.Close() }(pc)
			}
		})
	}

	mimeType := webrtc.MimeTypePCMA
	payloadType := webrtc.PayloadType(8)
	if channel.format.MULaw {
		mimeType = webrtc.MimeTypePCMU
		payloadType = 0
	}
	mediaEngine := &webrtc.MediaEngine{}
	err = mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mimeType, ClockRate: 8000, Channels: 1},
		PayloadType:        payloadType,
	}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		cleanup()
		return nil, err
	}
	registry := &interceptor.Registry{}
	if err = webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		cleanup()
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settings), webrtc.WithInterceptorRegistry(registry))
	pc, err = api.NewPeerConnection(webrtc.Configuration{ICEServers: m.iceServers})
	if err != nil {
		cleanup()
		return nil, err
	}
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		for {
			packet, _, readErr := track.ReadRTP()
			if readErr != nil || channel.WriteRTP(packet) != nil {
				cleanup()
				return
			}
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateClosed {
			cleanup()
		}
	})
	time.AfterFunc(30*time.Second, func() {
		state := pc.ICEConnectionState()
		if state != webrtc.ICEConnectionStateConnected && state != webrtc.ICEConnectionStateCompleted {
			cleanup()
		}
	})
	if err = pc.SetRemoteDescription(offer); err != nil {
		cleanup()
		return nil, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err = pc.SetLocalDescription(answer); err != nil {
		cleanup()
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	select {
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-gathered:
	}
	final := *pc.LocalDescription()
	return &final, nil
}
