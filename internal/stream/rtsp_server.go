package stream

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/auth"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/h264au"
	"github.com/rvben/vedetta/internal/rtsp"
)

// cameraStream holds the gortsplib ServerStream and consumer for one camera.
type cameraStream struct {
	mu       sync.RWMutex
	name     string
	rtspURL  string
	consumer *rtspServerConsumer
	stream   *gortsplib.ServerStream
}

// rtpStreamWriter is the part of gortsplib.ServerStream the consumer uses. It
// is an interface so a test can observe the packets that are actually published
// rather than reconstruct them.
type rtpStreamWriter interface {
	WritePacketRTP(*description.Media, *rtp.Packet) error
}

// rtspServerConsumer implements rtsp.Consumer and writes RTP into a gortsplib ServerStream.
// Video packets are depacketized and re-packetized to ensure proper RTP sizing,
// since some cameras (Tapo C200) send oversized RTP packets over TCP that exceed
// gortsplib's server-side MaxPacketSize (1472 bytes).
type rtspServerConsumer struct {
	stream     rtpStreamWriter
	videoMedia *description.Media
	audioMedia *description.Media
	videoPT    uint8 // expected payload type for video
	audioPT    uint8 // expected payload type for audio

	// H264 RTP decode/re-encode pipeline.
	h264Format *format.H264
	rtpDecoder *rtsp.H264AccessUnitDecoder
	rtpEncoder *rtph264.Encoder
}

func (c *rtspServerConsumer) writeRTP(media *description.Media, pkt *rtp.Packet, expectedPT uint8) {
	// Always write from a copy. gortsplib stamps its own SSRC into the packet it
	// is handed, and rtsp.Source hands every consumer one shared clone that the
	// recorder, the detector and the GOP cache read from their own goroutines,
	// so writing into it is both a data race and a corruption of the cached GOP.
	// The copy also carries the payload type this server declared in its SDP,
	// which can differ from what the camera sent.
	//
	// A shallow copy suffices: gortsplib marshals the packet into its own buffer
	// and mutates nothing but the header fields this copy already owns.
	clone := *pkt
	clone.PayloadType = expectedPT
	pkt = &clone

	defer func() {
		if r := recover(); r != nil {
			slog.Error("RTSP server: panic in WritePacketRTP", "recover", r)
		}
	}()

	if err := c.stream.WritePacketRTP(media, pkt); err != nil {
		slog.Debug("RTSP server: failed to write RTP", "error", err)
	}
}

func (c *rtspServerConsumer) OnVideoRTP(pkt *rtp.Packet) {
	if c.videoMedia == nil {
		return
	}

	// If no decoder is set up, forward raw (works for cameras with standard-sized packets).
	if c.rtpDecoder == nil {
		c.writeRTP(c.videoMedia, pkt, c.videoPT)
		return
	}

	// Depacketize H264 RTP into access units.
	au, _, err := c.rtpDecoder.Decode(pkt)
	if err != nil {
		return
	}

	// Strip SEI so a client of the republished stream gets the same normalized
	// bitstream as every other transport; see h264au.DropSEI.
	au = h264au.DropSEI(au)
	if len(au) == 0 {
		return
	}

	// Update SPS/PPS from in-band parameters.
	sps, pps, _ := h264au.TrackParameterSets(au, nil, nil)
	if sps != nil || pps != nil {
		curSPS, curPPS := c.h264Format.SafeParams()
		if sps == nil {
			sps = curSPS
		}
		if pps == nil {
			pps = curPPS
		}
		c.h264Format.SafeSetParams(sps, pps)
	}

	// Re-packetize into properly-sized RTP packets.
	pkts, err := c.rtpEncoder.Encode(au)
	if err != nil {
		return
	}

	for _, outPkt := range pkts {
		// Through writeRTP so a panic inside gortsplib is contained here. The
		// RTSP source recovers consumer panics too, but by detaching the
		// consumer, which would silence the republished stream permanently.
		c.writeRTP(c.videoMedia, outPkt, c.videoPT)
	}
}

func (c *rtspServerConsumer) OnAudioRTP(pkt *rtp.Packet) {
	if c.audioMedia == nil {
		return
	}
	c.writeRTP(c.audioMedia, pkt, c.audioPT)
}

func (c *rtspServerConsumer) OnDisconnect() {}

// RTSPServer re-publishes camera streams via RTSP.
type RTSPServer struct {
	hub     *rtsp.Hub
	server  *gortsplib.Server
	auth    *auth.Checker
	mu      sync.RWMutex
	cameras map[string]*cameraStream // camera name → stream
}

// NewRTSPServer creates a new RTSP re-publishing server.
func NewRTSPServer(hub *rtsp.Hub, cfg config.RTSPServerConfig, authChecker *auth.Checker, cameras []config.CameraConfig) *RTSPServer {
	rs := &RTSPServer{
		hub:     hub,
		auth:    authChecker,
		cameras: make(map[string]*cameraStream),
	}

	rs.server = &gortsplib.Server{
		Handler:        rs,
		RTSPAddress:    fmt.Sprintf(":%d", cfg.Port),
		UDPRTPAddress:  ":8000",
		UDPRTCPAddress: ":8001",
	}

	for _, cam := range cameras {
		if !cam.IsEnabled() {
			continue
		}
		// Main stream: use record_url (high-res) when available, else url.
		mainURL := cam.RecordURL
		if mainURL == "" {
			mainURL = cam.URL
		}
		rs.cameras[cam.Name] = &cameraStream{
			name:    cam.Name,
			rtspURL: mainURL,
		}
		// Sub stream: publish at /<name>_sub when a separate sub-stream URL exists.
		// Uses _sub suffix (not /sub path) to match go2rtc ecosystem convention.
		if cam.RecordURL != "" && cam.URL != cam.RecordURL {
			rs.cameras[cam.Name+"_sub"] = &cameraStream{
				name:    cam.Name + "_sub",
				rtspURL: cam.URL,
			}
		}
	}

	return rs
}

// Start starts the RTSP server and registers consumers on Hub sources.
func (rs *RTSPServer) Start() error {
	if err := rs.server.Start(); err != nil {
		return fmt.Errorf("RTSP server start: %w", err)
	}

	for name, cs := range rs.cameras {
		source := rs.hub.GetOrCreate(cs.rtspURL)

		desc, videoMedia, audioMedia := buildDescription(source)
		if desc == nil {
			slog.Warn("RTSP server: no tracks yet, stream will be available once camera connects",
				"camera", name)
			continue
		}

		initialized, err := rs.initCameraStream(cs, desc, videoMedia, audioMedia)
		if err != nil {
			slog.Error("RTSP server: failed to init stream", "camera", name, "error", err)
			continue
		}
		if !initialized {
			continue
		}

		source.AddConsumer(cs.consumer)
		slog.Info("RTSP server: publishing stream", "camera", name, "path", "/"+name)
	}

	return nil
}

// initCameraStream atomically initializes a camera's ServerStream and consumer.
// Returns false if the camera was already initialized (no-op).
func (rs *RTSPServer) initCameraStream(cs *cameraStream, desc *description.Session, videoMedia, audioMedia *description.Media) (bool, error) {
	serverStream := &gortsplib.ServerStream{
		Server: rs.server,
		Desc:   desc,
	}
	if err := serverStream.Initialize(); err != nil {
		return false, err
	}

	consumer := &rtspServerConsumer{
		stream:     serverStream,
		videoMedia: videoMedia,
		audioMedia: audioMedia,
	}
	if videoMedia != nil && len(videoMedia.Formats) > 0 {
		consumer.videoPT = videoMedia.Formats[0].PayloadType()

		if h264Fmt, ok := videoMedia.Formats[0].(*format.H264); ok {
			consumer.h264Format = h264Fmt
			dec, err := h264Fmt.CreateDecoder()
			if err == nil {
				enc, err := h264Fmt.CreateEncoder()
				if err == nil {
					consumer.rtpDecoder = rtsp.NewH264AccessUnitDecoder(dec)
					consumer.rtpEncoder = enc
				}
			}
		}
	}
	if audioMedia != nil && len(audioMedia.Formats) > 0 {
		consumer.audioPT = audioMedia.Formats[0].PayloadType()
	}

	cs.mu.Lock()
	if cs.stream != nil {
		// Another goroutine initialized while we were building the description.
		cs.mu.Unlock()
		serverStream.Close()
		return false, nil
	}
	cs.stream = serverStream
	cs.consumer = consumer
	cs.mu.Unlock()

	return true, nil
}

// Close shuts down the RTSP server and removes all consumers.
func (rs *RTSPServer) Close() {
	for name, cs := range rs.cameras {
		cs.mu.RLock()
		consumer := cs.consumer
		stream := cs.stream
		cs.mu.RUnlock()

		if consumer != nil {
			if source := rs.hub.Get(cs.rtspURL); source != nil {
				source.RemoveConsumer(consumer)
			}
		}
		if stream != nil {
			stream.Close()
		}
		slog.Debug("RTSP server: unpublished stream", "camera", name)
	}

	rs.server.Close()
	slog.Info("RTSP server closed")
}

// initLateCamera initializes a camera stream that wasn't ready at startup.
func (rs *RTSPServer) initLateCamera(name string, cs *cameraStream) {
	source := rs.hub.Get(cs.rtspURL)
	if source == nil {
		return
	}

	desc, videoMedia, audioMedia := buildDescription(source)
	if desc == nil {
		return
	}

	initialized, err := rs.initCameraStream(cs, desc, videoMedia, audioMedia)
	if err != nil {
		slog.Error("RTSP server: failed to late-init stream", "camera", name, "error", err)
		return
	}
	if !initialized {
		return
	}

	source.AddConsumer(cs.consumer)
	slog.Info("RTSP server: publishing stream (late init)", "camera", name, "path", "/"+name)
}

// buildDescription constructs an SDP description from a Source's track info.
func buildDescription(source *rtsp.Source) (*description.Session, *description.Media, *description.Media) {
	vt := source.VideoTrack()
	if vt == nil {
		return nil, nil, nil
	}

	var medias []*description.Media
	var videoMedia, audioMedia *description.Media

	switch vt.Codec {
	case "H264":
		videoPT := vt.PayloadType
		if videoPT == 0 {
			videoPT = 96
		}
		h264Format := &format.H264{
			PayloadTyp:        videoPT,
			PacketizationMode: 1,
		}
		if len(vt.SPS) > 0 {
			h264Format.SPS = vt.SPS
		}
		if len(vt.PPS) > 0 {
			h264Format.PPS = vt.PPS
		}
		videoMedia = &description.Media{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{h264Format},
		}
		medias = append(medias, videoMedia)
	}

	at := source.AudioTrack()
	if at != nil {
		switch at.Codec {
		case "AAC":
			audioPT := at.PayloadType
			if audioPT == 0 {
				audioPT = 97
			}
			aacFormat := &format.MPEG4Audio{
				PayloadTyp: audioPT,
				Config: &mpeg4audio.AudioSpecificConfig{
					Type:          mpeg4audio.ObjectTypeAACLC,
					SampleRate:    at.ClockRate,
					ChannelConfig: uint8(at.ChannelCount),
				},
				SizeLength:       13,
				IndexLength:      3,
				IndexDeltaLength: 3,
			}
			audioMedia = &description.Media{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{aacFormat},
			}
			medias = append(medias, audioMedia)

		case "PCMU", "PCMA":
			g711Format := &format.G711{
				PayloadTyp:   at.PayloadType,
				MULaw:        at.Codec == "PCMU",
				SampleRate:   at.ClockRate,
				ChannelCount: at.ChannelCount,
			}
			audioMedia = &description.Media{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{g711Format},
			}
			medias = append(medias, audioMedia)
		}
	}

	if len(medias) == 0 {
		return nil, nil, nil
	}

	return &description.Session{Medias: medias}, videoMedia, audioMedia
}

// parseStreamKey extracts the stream key from an RTSP path.
// Returns keys like "front_door" or "front_door_sub".
// Uses _sub suffix convention (matching go2rtc ecosystem) instead of /sub path segments.
// Handles DESCRIBE paths ("/front_door", "/front_door_sub") and
// SETUP paths ("/front_door/trackID=0", "/front_door_sub/trackID=0").
func parseStreamKey(path string) string {
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")

	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	return parts[0]
}

// --- gortsplib ServerHandler interface ---

func (rs *RTSPServer) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	slog.Debug("RTSP server: client connected", "remote", ctx.Conn.NetConn().RemoteAddr())
}

func (rs *RTSPServer) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	slog.Debug("RTSP server: client disconnected", "remote", ctx.Conn.NetConn().RemoteAddr(), "error", ctx.Error)
}

func (rs *RTSPServer) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {
	slog.Debug("RTSP server: session opened")
}

func (rs *RTSPServer) OnSessionClose(_ *gortsplib.ServerHandlerOnSessionCloseCtx) {
	slog.Debug("RTSP server: session closed")
}

func (rs *RTSPServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if resp := rs.checkRTSPAuth(ctx.Request, ctx.Conn); resp != nil {
		return resp, nil, nil
	}

	name := parseStreamKey(ctx.Path)

	rs.mu.RLock()
	cs, ok := rs.cameras[name]
	rs.mu.RUnlock()

	if !ok {
		slog.Debug("RTSP server: DESCRIBE for unknown camera", "path", ctx.Path)
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	cs.mu.RLock()
	stream := cs.stream
	cs.mu.RUnlock()

	if stream == nil {
		rs.initLateCamera(name, cs)
		cs.mu.RLock()
		stream = cs.stream
		cs.mu.RUnlock()
	}

	if stream == nil {
		slog.Debug("RTSP server: camera not ready yet", "camera", name)
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

func (rs *RTSPServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if resp := rs.checkRTSPAuth(ctx.Request, ctx.Conn); resp != nil {
		return resp, nil, nil
	}

	name := parseStreamKey(ctx.Path)

	rs.mu.RLock()
	cs, ok := rs.cameras[name]
	rs.mu.RUnlock()

	if !ok {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	cs.mu.RLock()
	stream := cs.stream
	cs.mu.RUnlock()

	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}

	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

func (rs *RTSPServer) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// checkRTSPAuth validates RTSP Basic Auth from the request Authorization header.
// Returns nil response if auth succeeds or is not configured; returns 401 response on failure.
func (rs *RTSPServer) checkRTSPAuth(req *base.Request, conn *gortsplib.ServerConn) *base.Response {
	if rs.auth == nil {
		return nil
	}

	remoteAddr := conn.NetConn().RemoteAddr().String()

	user, pass, ok := parseRTSPBasicAuth(req.Header)
	if !ok {
		return rtspUnauthorized()
	}

	remoteIP, _, _ := net.SplitHostPort(remoteAddr)
	if !rs.auth.Check(user, pass, remoteIP) {
		return rtspUnauthorized()
	}

	return nil
}

func rtspUnauthorized() *base.Response {
	return &base.Response{
		StatusCode: base.StatusUnauthorized,
		Header: base.Header{
			"WWW-Authenticate": base.HeaderValue{`Basic realm="vedetta"`},
		},
	}
}

// parseRTSPBasicAuth extracts username and password from an RTSP Authorization header.
func parseRTSPBasicAuth(header base.Header) (user, pass string, ok bool) {
	authHeader := header["Authorization"]
	if len(authHeader) == 0 {
		return "", "", false
	}

	val := authHeader[0]
	if !strings.HasPrefix(val, "Basic ") {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(val, "Basic "))
	if err != nil {
		return "", "", false
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	return parts[0], parts[1], true
}
