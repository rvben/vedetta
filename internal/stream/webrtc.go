package stream

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/rtsp"
)

// StreamManager manages per-camera WebRTC sessions with direct RTP forwarding.
type StreamManager struct {
	hub *rtsp.Hub
	mu  sync.Mutex
	// One webrtcConsumer per camera URL, shared across all peers watching that camera.
	consumers map[string]*webrtcConsumer
	// iceServers is the resolved set of STUN/TURN servers offered to peers.
	// Empty by default (no external ICE), populated only from config.
	iceServers []webrtc.ICEServer
}

// rtpWriter is the minimal interface needed to forward a packet onto a peer's
// outbound track. Pion's *webrtc.TrackLocalStaticRTP satisfies it; tests use a
// recording fake.
type rtpWriter interface {
	WriteRTP(pkt *rtp.Packet) error
}

// trackState handles per-track sequence/timestamp rewriting for mid-stream
// joins. The outbound sequence number is allocated monotonically by the track,
// independent of the source RTP sequence, so a single inbound packet may
// produce multiple outbound packets (e.g. FU-A fragments) without collisions.
type trackState struct {
	track rtpWriter

	mu       sync.Mutex
	outSeq   uint16
	tsOffset uint32
	started  bool
}

// write forwards pkt with a freshly assigned outbound sequence number and the
// timestamp rebased to start near zero. The caller's pkt is not mutated.
func (t *trackState) write(pkt *rtp.Packet) error {
	t.mu.Lock()
	if !t.started {
		t.tsOffset = -pkt.Timestamp
		t.started = true
	}
	seq := t.outSeq
	t.outSeq++
	ts := pkt.Timestamp + t.tsOffset
	t.mu.Unlock()

	clone := *pkt
	clone.SequenceNumber = seq
	clone.Timestamp = ts
	return t.track.WriteRTP(&clone)
}

type peerState struct {
	pc         *webrtc.PeerConnection
	cameraName string
	video      *trackState
	audio      *trackState // nil if camera has no supported audio

	// Parameter sets cached from the RTSP SDP. When the first forwarded
	// keyframe is a bare IDR (NAL type 5) — common for cameras that only
	// advertise SPS/PPS out-of-band — we inject these so the browser
	// decoder can bootstrap. Without them, the decoder waits forever and
	// the <video> element stays at readyState=0 with framesReceived=0.
	sps []byte
	pps []byte

	mu           sync.Mutex
	keyframeSeen bool

	// deliveryMu makes the cached-GOP replay and the transition to live RTP a
	// single ordered operation for this viewer. Packets received before ICE is
	// ready are intentionally skipped; a packet arriving during activation
	// waits and is delivered immediately after the cached GOP, so there is no
	// decode-corrupting hole at the handoff boundary.
	deliveryMu     sync.Mutex
	transportReady bool
	// pendingVideo holds one packet so the next packet can prove whether it is
	// really the end of its access unit. A few cameras set RTP's marker bit on
	// both the IDR slice and trailing SEI metadata at the same timestamp. Safari
	// treats those duplicate frame boundaries as two samples with one timestamp
	// and can freeze after the first decoded image. One-packet lookahead lets us
	// emit exactly one marker on the final packet of every timestamp while adding
	// less than one frame of latency.
	pendingVideo *rtp.Packet

	// Forwarding diagnostics. WriteRTP can fail silently deep in the pion
	// stack (e.g. a buffer pool with a fixed-size slot rejects payloads
	// larger than its cap with io.ErrShortBuffer). Without counters we
	// can't tell a healthy stream from a stream where 95% of packets are
	// being dropped — the symptoms are identical to upstream packet loss.
	videoCalls    atomic.Uint64
	videoOK       atomic.Uint64
	videoErr      atomic.Uint64
	videoBytes    atomic.Uint64
	videoMaxSize  atomic.Uint64
	videoOver1200 atomic.Uint64
	videoOver1400 atomic.Uint64
	// Inbound NAL type classification — reveals whether the camera is
	// sending huge single-NAL packets (fragmentable) or huge STAP-A
	// aggregates (not currently fragmented and likely the reason
	// keyframes don't assemble in the browser).
	inSingleIDR atomic.Uint64
	inSingleP   atomic.Uint64
	inSingleSPS atomic.Uint64
	inSinglePPS atomic.Uint64
	inSTAPA     atomic.Uint64
	inFUA       atomic.Uint64
	inOther     atomic.Uint64
	// Outbound packet counter — every WriteRTP call to the track, after
	// SPS/PPS injection and FU-A fragmentation expand each inbound packet
	// into one or more outbound packets.
	outPkts       atomic.Uint64
	loggedFirstKF atomic.Bool
	audioCalls    atomic.Uint64
	audioOK       atomic.Uint64
	audioErr      atomic.Uint64
	loggedErr     atomic.Bool
	statsCancel   context.CancelFunc
}

func (p *peerState) deliverVideo(pkt *rtp.Packet) error {
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	if !p.transportReady {
		return nil
	}
	return p.queueVideo(pkt)
}

func (p *peerState) deliverAudio(pkt *rtp.Packet) error {
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	if !p.transportReady {
		return nil
	}
	return p.writeAudio(pkt)
}

// activateTransport primes a newly connected viewer with the source's latest
// independently decodable GOP, then opens the live packet path without a race.
// The source cache is read while deliveryMu is held: any packet skipped before
// this lock is already in the cache, and any packet arriving afterward waits
// behind the replay.
func (p *peerState) activateTransport(source *rtsp.Source) {
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	if p.transportReady {
		return
	}

	primed := 0
	for _, pkt := range source.LatestVideoGOP() {
		if err := p.queueVideo(pkt); err != nil {
			// A fresh keyframe on the live path can recover from a failed replay.
			p.mu.Lock()
			p.keyframeSeen = false
			p.mu.Unlock()
			slog.Warn("failed to prime WebRTC peer from cached GOP", "camera", p.cameraName, "error", err)
			break
		}
		primed++
	}
	p.transportReady = true
	slog.Info("WebRTC transport ready", "camera", p.cameraName, "primedPackets", primed)
}

// queueVideo normalizes RTP marker bits with one-packet lookahead before the
// packet reaches Pion. RFC 6184 defines Marker as the last packet of an access
// unit, but cameras in the field can mark several NAL units sharing the same
// timestamp (for example IDR and trailing SEI). Browser H.264 depacketizers can
// then construct duplicate samples for one timestamp and stop advancing.
//
// deliveryMu serializes this method, including the cached-GOP-to-live handoff,
// so pendingVideo always observes the source packet order. The stored packet is
// a shallow copy because only its header is changed; RTP payloads remain
// immutable throughout the fan-out pipeline.
func (p *peerState) queueVideo(pkt *rtp.Packet) error {
	if pkt == nil {
		return nil
	}
	next := *pkt
	if p.pendingVideo == nil {
		p.pendingVideo = &next
		return nil
	}

	pending := *p.pendingVideo
	// A packet followed by another packet at the same timestamp cannot be the
	// access-unit boundary. Conversely, the final packet before a timestamp
	// change is the boundary even when a camera forgot to mark it.
	pending.Marker = pending.Timestamp != next.Timestamp
	p.pendingVideo = &next
	return p.writeVideo(&pending)
}

// classifyInbound bumps the NAL-type counter for pkt. The classification
// matches isKeyframe's NAL-type cases but covers every type so the periodic
// stats line shows the full inbound composition.
func (p *peerState) classifyInbound(pkt *rtp.Packet) {
	if len(pkt.Payload) < 1 {
		p.inOther.Add(1)
		return
	}
	switch pkt.Payload[0] & 0x1f {
	case 5:
		p.inSingleIDR.Add(1)
	case 1:
		p.inSingleP.Add(1)
	case 7:
		p.inSingleSPS.Add(1)
	case 8:
		p.inSinglePPS.Add(1)
	case 24:
		p.inSTAPA.Add(1)
	case 28:
		p.inFUA.Add(1)
	default:
		p.inOther.Add(1)
	}
}

// clearParamSetMarker returns a packet whose RTP marker bit is cleared when pkt
// is a parameter-set NAL (SPS type 7 / PPS type 8) that arrived with marker=1.
//
// Some cameras (notably Tapo) set marker=1 on every packet of an access unit,
// including the SPS/PPS NALs that precede the IDR slice. Chrome's H.264
// depacketizer treats marker=1 as "end of access unit" and assembles each into
// a separate frame; the SPS/PPS frames carry no slice data so the decoder
// silently rejects every keyframe and framesDecoded stays at 0. RFC 6184 §5.1
// specifies the marker bit MUST be set only on the final RTP packet of an
// access unit, and parameter-set NALs are never that. Clearing their marker bit
// makes Chrome assemble SPS+PPS+IDR into a single frame.
//
// pkt is the single clone shared by all fan-out consumers, so this never
// mutates it: when an adjustment is needed it returns a shallow copy (the
// payload backing array is shared read-only), otherwise it returns pkt as-is.
func clearParamSetMarker(pkt *rtp.Packet) *rtp.Packet {
	if !pkt.Marker || len(pkt.Payload) == 0 {
		return pkt
	}
	if nalType := pkt.Payload[0] & 0x1f; nalType != 7 && nalType != 8 {
		return pkt
	}
	cp := *pkt
	cp.Marker = false
	return &cp
}

func (p *peerState) writeVideo(pkt *rtp.Packet) error {
	p.mu.Lock()
	needsParams := false
	if !p.keyframeSeen {
		if !isKeyframe(pkt) {
			p.mu.Unlock()
			return nil
		}
		p.keyframeSeen = true
		needsParams = len(p.sps) > 0 && len(p.pps) > 0 && !containsParameterSets(pkt)
	}
	p.mu.Unlock()

	p.videoCalls.Add(1)
	p.classifyInbound(pkt)
	sz := uint64(len(pkt.Payload))
	for {
		cur := p.videoMaxSize.Load()
		if sz <= cur || p.videoMaxSize.CompareAndSwap(cur, sz) {
			break
		}
	}
	if sz > 1400 {
		p.videoOver1400.Add(1)
	}
	if sz > 1200 {
		p.videoOver1200.Add(1)
	}

	var nalType byte
	if len(pkt.Payload) > 0 {
		nalType = pkt.Payload[0] & 0x1f
	}

	// Clear the marker bit on parameter-set NALs for this peer's output. This
	// must not mutate the shared fan-out clone, so it may return a copy.
	pkt = clearParamSetMarker(pkt)

	if needsParams {
		if err := p.video.write(buildNALPacket(pkt, p.sps)); err != nil {
			p.recordWriteErr("video", err, pkt)
			return err
		}
		p.outPkts.Add(1)
		if err := p.video.write(buildNALPacket(pkt, p.pps)); err != nil {
			p.recordWriteErr("video", err, pkt)
			return err
		}
		p.outPkts.Add(1)
	}

	fragments := 0
	var frags []*rtp.Packet
	if f := fragmentSingleNAL(pkt, fuaMTU); f != nil {
		frags = f
	} else if f := refragmentFUA(pkt, fuaMTU); f != nil {
		frags = f
	}
	if frags != nil {
		fragments = len(frags)
		for _, frag := range frags {
			if err := p.video.write(frag); err != nil {
				p.recordWriteErr("video", err, pkt)
				return err
			}
			p.outPkts.Add(1)
		}
	} else {
		if err := p.video.write(pkt); err != nil {
			p.recordWriteErr("video", err, pkt)
			return err
		}
		p.outPkts.Add(1)
	}

	if p.loggedFirstKF.CompareAndSwap(false, true) {
		slog.Info("WebRTC first keyframe forwarded",
			"camera", p.cameraName,
			"inNAL", nalType,
			"inSize", sz,
			"spsLen", len(p.sps),
			"ppsLen", len(p.pps),
			"injectedParams", needsParams,
			"fragments", fragments,
			"sps", hex.EncodeToString(p.sps),
			"pps", hex.EncodeToString(p.pps),
		)
	}

	p.videoOK.Add(1)
	p.videoBytes.Add(sz)
	return nil
}

func (p *peerState) recordWriteErr(kind string, err error, pkt *rtp.Packet) {
	if kind == "video" {
		p.videoErr.Add(1)
	} else {
		p.audioErr.Add(1)
	}
	if p.loggedErr.CompareAndSwap(false, true) {
		slog.Warn("WebRTC write error",
			"camera", p.cameraName,
			"kind", kind,
			"error", err.Error(),
			"errType", fmt.Sprintf("%T", err),
			"payloadLen", len(pkt.Payload),
			"seq", pkt.SequenceNumber,
		)
	}
}

func (p *peerState) startStatsLogger(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	p.statsCancel = cancel
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		start := time.Now()
		var lastV, lastVOK, lastA uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				v := p.videoCalls.Load()
				vok := p.videoOK.Load()
				verr := p.videoErr.Load()
				a := p.audioCalls.Load()
				slog.Info("WebRTC peer stats",
					"camera", p.cameraName,
					"t", int(time.Since(start).Seconds()),
					"vCalls", v,
					"vOK", vok,
					"vErr", verr,
					"vBytes", p.videoBytes.Load(),
					"vMax", p.videoMaxSize.Load(),
					"vOver1200", p.videoOver1200.Load(),
					"vOver1400", p.videoOver1400.Load(),
					"vRate", v-lastV,
					"vOKRate", vok-lastVOK,
					"outPkts", p.outPkts.Load(),
					"inIDR", p.inSingleIDR.Load(),
					"inP", p.inSingleP.Load(),
					"inSPS", p.inSingleSPS.Load(),
					"inPPS", p.inSinglePPS.Load(),
					"inSTAPA", p.inSTAPA.Load(),
					"inFUA", p.inFUA.Load(),
					"inOther", p.inOther.Load(),
					"aCalls", a,
					"aRate", a-lastA,
				)
				lastV, lastVOK, lastA = v, vok, a
			}
		}
	}()
}

// containsParameterSets reports whether pkt already carries an SPS (NAL 7) or
// PPS (NAL 8), either as a single NAL or aggregated inside a STAP-A. When true,
// injecting cached parameter sets would just produce duplicates.
func containsParameterSets(pkt *rtp.Packet) bool {
	if len(pkt.Payload) < 1 {
		return false
	}
	nalType := pkt.Payload[0] & 0x1f
	switch nalType {
	case 7, 8:
		return true
	case 24: // STAP-A: scan every aggregated NAL header
		offset := 1
		for offset+2 <= len(pkt.Payload) {
			size := int(pkt.Payload[offset])<<8 | int(pkt.Payload[offset+1])
			offset += 2
			if size < 1 || offset+size > len(pkt.Payload) {
				return false
			}
			if inner := pkt.Payload[offset] & 0x1f; inner == 7 || inner == 8 {
				return true
			}
			offset += size
		}
	}
	return false
}

// buildNALPacket synthesizes a single-NAL RTP packet that piggy-backs on
// template's SSRC, payload type, and timestamp so it lands in the same access
// unit as the IDR that follows. The outbound sequence number is assigned by
// trackState.write, so callers must not rely on the SequenceNumber field here.
func buildNALPacket(template *rtp.Packet, nal []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:     2,
			PayloadType: template.PayloadType,
			Timestamp:   template.Timestamp,
			SSRC:        template.SSRC,
		},
		Payload: append([]byte(nil), nal...),
	}
}

// fuaMTU is the maximum payload size for fragmented NAL units. Browsers
// commonly use 1200 as the effective WebRTC RTP MTU; values below that leave
// safe headroom for SRTP auth tags and any extension headers.
const fuaMTU = 1200

// fragmentSingleNAL splits a single NAL unit larger than mtu into FU-A
// fragments. Each fragment becomes its own RTP packet with the source's
// timestamp/SSRC/PayloadType and a FU-A indicator + header carrying a slice of
// the NAL data. Returns nil if the payload is not a single NAL or fits within
// mtu — callers should pass the original packet through unchanged in that case.
func fragmentSingleNAL(pkt *rtp.Packet, mtu int) []*rtp.Packet {
	payload := pkt.Payload
	if len(payload) <= mtu {
		return nil
	}
	if len(payload) < 1 {
		return nil
	}
	nalHeader := payload[0]
	nalType := nalHeader & 0x1f
	// Only fragment single NAL units (types 1–23). STAP-A (24) and FU-A (28)
	// have their own framing and must be passed through unchanged.
	if nalType < 1 || nalType > 23 {
		return nil
	}

	nalData := payload[1:]
	nri := nalHeader & 0x60
	fuIndicator := nri | 28 // type 28 = FU-A

	const fuHeaderLen = 2 // 1 byte FU indicator + 1 byte FU header
	fragSize := mtu - fuHeaderLen
	if fragSize <= 0 {
		return nil
	}

	fragments := make([]*rtp.Packet, 0, (len(nalData)+fragSize-1)/fragSize)
	offset := 0
	for offset < len(nalData) {
		end := offset + fragSize
		isLast := false
		if end >= len(nalData) {
			end = len(nalData)
			isLast = true
		}
		fuHeader := nalType
		if offset == 0 {
			fuHeader |= 0x80 // start bit
		}
		if isLast {
			fuHeader |= 0x40 // end bit
		}
		fragPayload := make([]byte, fuHeaderLen+(end-offset))
		fragPayload[0] = fuIndicator
		fragPayload[1] = fuHeader
		copy(fragPayload[fuHeaderLen:], nalData[offset:end])

		marker := false
		if isLast {
			marker = pkt.Marker
		}
		fragments = append(fragments, &rtp.Packet{
			Header: rtp.Header{
				Version:     2,
				PayloadType: pkt.PayloadType,
				Timestamp:   pkt.Timestamp,
				SSRC:        pkt.SSRC,
				Marker:      marker,
			},
			Payload: fragPayload,
		})
		offset = end
	}
	return fragments
}

// refragmentFUA re-splits an oversized FU-A packet into smaller FU-A pieces
// while preserving the original Start/End bit semantics. Returns nil when pkt
// is not a FU-A or already fits within mtu so the caller forwards it unchanged.
//
// Some cameras (e.g. Tapo C200 sub-stream) emit FU-A fragments at the camera's
// own MTU — well above the ~1500-byte slots Pion's receive-side packetio buffer
// allocates per packet. Forwarding those oversized FU-A packets through to the
// browser causes Pion to reject them with io.ErrShortBuffer, and the receiver
// silently loses every IDR fragment while smaller P-slice fragments slip
// through. The browser's H.264 decoder then has nothing to bootstrap from and
// the <video> element sits at framesDecoded=0 indefinitely.
func refragmentFUA(pkt *rtp.Packet, mtu int) []*rtp.Packet {
	payload := pkt.Payload
	if len(payload) < 2 || len(payload) <= mtu {
		return nil
	}
	fuIndicator := payload[0]
	if fuIndicator&0x1f != 28 {
		return nil
	}
	fuHeader := payload[1]
	innerNAL := fuHeader & 0x1f
	origStart := fuHeader & 0x80
	origEnd := fuHeader & 0x40

	nalData := payload[2:]
	const fuHeaderLen = 2
	fragSize := mtu - fuHeaderLen
	if fragSize <= 0 {
		return nil
	}

	fragments := make([]*rtp.Packet, 0, (len(nalData)+fragSize-1)/fragSize)
	offset := 0
	for offset < len(nalData) {
		end := offset + fragSize
		isLast := false
		if end >= len(nalData) {
			end = len(nalData)
			isLast = true
		}
		// Only the first emitted piece may carry the original Start bit;
		// only the last emitted piece may carry the original End bit. Middle
		// pieces are always S=0 E=0. This preserves correct reassembly at
		// the receiver regardless of whether the original FU-A was a head,
		// middle, tail, or (rare) single S+E fragment.
		newFuHeader := innerNAL
		if offset == 0 {
			newFuHeader |= origStart
		}
		if isLast {
			newFuHeader |= origEnd
		}
		fragPayload := make([]byte, fuHeaderLen+(end-offset))
		fragPayload[0] = fuIndicator
		fragPayload[1] = newFuHeader
		copy(fragPayload[fuHeaderLen:], nalData[offset:end])

		marker := false
		if isLast {
			marker = pkt.Marker
		}
		fragments = append(fragments, &rtp.Packet{
			Header: rtp.Header{
				Version:     2,
				PayloadType: pkt.PayloadType,
				Timestamp:   pkt.Timestamp,
				SSRC:        pkt.SSRC,
				Marker:      marker,
			},
			Payload: fragPayload,
		})
		offset = end
	}
	return fragments
}

func (p *peerState) writeAudio(pkt *rtp.Packet) error {
	if p.audio == nil {
		return nil
	}
	// Only forward audio after the first video keyframe
	p.mu.Lock()
	if !p.keyframeSeen {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	p.audioCalls.Add(1)
	if err := p.audio.write(pkt); err != nil {
		p.recordWriteErr("audio", err, pkt)
		return err
	}
	p.audioOK.Add(1)
	return nil
}

// isKeyframe checks if an RTP packet contains the start of an H264 IDR frame.
func isKeyframe(pkt *rtp.Packet) bool {
	if len(pkt.Payload) < 2 {
		return false
	}

	nalType := pkt.Payload[0] & 0x1f

	switch {
	case nalType >= 1 && nalType <= 23:
		// Single NAL unit: type 5 = IDR, type 7 = SPS
		return nalType == 5 || nalType == 7
	case nalType == 24:
		// STAP-A: parameter sets can precede the IDR, and some encoders place
		// an AUD first. Scan the complete aggregate instead of assuming the
		// first inner NAL is the random-access unit.
		offset := 1
		for offset+2 <= len(pkt.Payload) {
			size := int(pkt.Payload[offset])<<8 | int(pkt.Payload[offset+1])
			offset += 2
			if size < 1 || offset+size > len(pkt.Payload) {
				return false
			}
			innerNALType := pkt.Payload[offset] & 0x1f
			if innerNALType == 5 || innerNALType == 7 {
				return true
			}
			offset += size
		}
		return false
	case nalType == 28:
		// FU-A: check start bit and NAL type
		startBit := pkt.Payload[1] & 0x80
		fuNALType := pkt.Payload[1] & 0x1f
		return startBit != 0 && (fuNALType == 5 || fuNALType == 7)
	}

	return false
}

// webrtcConsumer implements rtsp.Consumer and forwards RTP to WebRTC peers.
type webrtcConsumer struct {
	cameraName string
	mu         sync.RWMutex
	peers      []*peerState
	sourceAttachment
}

func (wc *webrtcConsumer) OnVideoRTP(pkt *rtp.Packet) {
	for _, p := range wc.snapshotPeers() {
		if err := p.deliverVideo(pkt); err != nil {
			slog.Debug("failed to write video RTP to peer", "error", err)
		}
	}
}

func (wc *webrtcConsumer) OnAudioRTP(pkt *rtp.Packet) {
	for _, p := range wc.snapshotPeers() {
		if err := p.deliverAudio(pkt); err != nil {
			slog.Debug("failed to write audio RTP to peer", "error", err)
		}
	}
}

// snapshotPeers returns a copy of the current peer set. The per-packet writes
// then run without holding wc.mu, so a slow or stuck peer write cannot block
// addPeer/removePeer (a viewer joining or being reaped).
func (wc *webrtcConsumer) snapshotPeers() []*peerState {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	if len(wc.peers) == 0 {
		return nil
	}
	return append([]*peerState(nil), wc.peers...)
}

func (wc *webrtcConsumer) OnDisconnect() {}

func (wc *webrtcConsumer) addPeer(peer *peerState) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	wc.peers = append(wc.peers, peer)
}

func (wc *webrtcConsumer) removePeer(peer *peerState) int {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	for i, p := range wc.peers {
		if p == peer {
			wc.peers = append(wc.peers[:i], wc.peers[i+1:]...)
			break
		}
	}
	return len(wc.peers)
}

// NewStreamManager creates a stream manager that uses an RTSP Hub for direct forwarding.
// iceServers configures the STUN/TURN servers offered to WebRTC peers; pass nil to
// offer none (the privacy-first default).
func NewStreamManager(hub *rtsp.Hub, iceServers []config.ICEServerConfig) *StreamManager {
	return &StreamManager{
		hub:        hub,
		consumers:  make(map[string]*webrtcConsumer),
		iceServers: iceServersFromConfig(iceServers),
	}
}

// iceServersFromConfig converts the YAML ICE server config into pion's
// representation. Entries without URLs are skipped (pion would reject them).
// TURN credentials map to a password-type credential; STUN entries carry none.
func iceServersFromConfig(cfg []config.ICEServerConfig) []webrtc.ICEServer {
	if len(cfg) == 0 {
		return nil
	}
	servers := make([]webrtc.ICEServer, 0, len(cfg))
	for _, s := range cfg {
		if len(s.URLs) == 0 {
			continue
		}
		server := webrtc.ICEServer{URLs: s.URLs}
		if s.Username != "" || s.Credential != "" {
			server.Username = s.Username
			server.Credential = s.Credential
			server.CredentialType = webrtc.ICECredentialTypePassword
		}
		servers = append(servers, server)
	}
	if len(servers) == 0 {
		return nil
	}
	return servers
}

// audioCodecForTrack returns the WebRTC codec parameters for a camera's audio track.
// Only G.711 codecs (PCMU/PCMA) are supported for WebRTC passthrough.
// AAC cameras get audio via MSE streaming instead.
func audioCodecForTrack(at *rtsp.TrackInfo) *webrtc.RTPCodecParameters {
	if at == nil {
		return nil
	}
	switch at.Codec {
	case "PCMU":
		return &webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypePCMU,
				ClockRate: 8000,
				Channels:  1,
			},
			PayloadType: 0,
		}
	case "PCMA":
		return &webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypePCMA,
				ClockRate: 8000,
				Channels:  1,
			},
			PayloadType: 8,
		}
	default:
		slog.Info("audio codec not supported for WebRTC passthrough, use MSE for audio", "codec", at.Codec)
		return nil
	}
}

// profileLevelIDRegex matches a profile-level-id parameter inside an H.264
// fmtp line. The value is exactly 6 hex digits (3 bytes: profile_idc,
// profile_iop, level_idc).
var profileLevelIDRegex = regexp.MustCompile(`profile-level-id=[0-9a-fA-F]{6}`)

// maxAnswerLevelIDC is the highest level_idc we'll ever advertise in an SDP
// answer. Chrome's WebRTC stack universally offers H.264 Level 3.1 (0x1f) and
// silently fails to allocate a decoder when the answer demands a higher level,
// even though level-asymmetry-allowed=1 ostensibly permits it — the connection
// stalls in ICE checking and the peer gives up after ~30s. Cameras like
// Reolink commonly stamp their sub-stream SPS with an inflated 5.1 (0x33) that
// no browser honors. Capping here keeps the answer within the offered
// envelope; the camera's real SPS still rides in the bitstream so Chrome's
// decoder reconfigures itself once frames arrive.
const maxAnswerLevelIDC = 0x1f

// clampLevelIDC returns pli with its level_idc byte clamped to maxAnswerLevelIDC.
// Returns the input unchanged if it isn't a well-formed 6-char hex value.
func clampLevelIDC(pli string) string {
	if len(pli) != 6 {
		return pli
	}
	b, err := hex.DecodeString(pli)
	if err != nil {
		return pli
	}
	if b[2] <= maxAnswerLevelIDC {
		return pli
	}
	return pli[:4] + fmt.Sprintf("%02x", maxAnswerLevelIDC)
}

// rewriteAnswerProfileLevelID replaces every occurrence of
// "profile-level-id=XXXXXX" in answerSDP with the supplied 6-hex-char value.
//
// Pion's MediaEngine stores the remote offer's codec parameters in the
// negotiated codec list, so the answer's profile-level-id always echoes the
// browser's offered value. When that value advertises a lower H.264 level
// than the camera actually emits in-band, the browser configures its decoder
// for the smaller level and silently drops every frame whose SPS exceeds it
// (framesReceived rises but framesDecoded stays at 0).
//
// RFC 6184 explicitly permits the level component (third byte) of
// profile-level-id to differ between offer and answer when the offerer set
// "level-asymmetry-allowed=1". The profile_idc and profile_iop bytes must
// match symmetrically — but Pion's H.264 fmtp matcher already requires those
// to be equal for negotiation to have succeeded, so substituting the full
// camera-derived PLI here is safe in practice. If pli is malformed (not a
// valid 6-char hex string), the input is returned unchanged.
func rewriteAnswerProfileLevelID(answerSDP, pli string) string {
	if len(pli) != 6 {
		return answerSDP
	}
	if _, err := hex.DecodeString(pli); err != nil {
		return answerSDP
	}
	return profileLevelIDRegex.ReplaceAllString(answerSDP, "profile-level-id="+pli)
}

// HandleOffer processes a WebRTC SDP offer and returns an SDP answer.
func (sm *StreamManager) HandleOffer(cameraName, rtspURL string, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	// Build H264 codec capability with profile-level-id from camera SPS.
	// cameraPLI is the 6-hex-char profile-level-id derived from the camera's
	// SPS; when non-empty, the final answer SDP is rewritten so its
	// profile-level-id matches the bitstream (Pion otherwise echoes the
	// browser's offered level, mis-configuring the decoder).
	sdpFmtpLine := "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"
	source := sm.hub.GetOrCreate(rtspURL)
	// Block briefly so cameras that ship SPS/PPS in-band (rather than via
	// sprop-parameter-sets in the SDP) have time to populate them. Without
	// this wait, the answer SDP keeps the default 42001f profile-level-id;
	// when the real bitstream uses a different profile (e.g. High 3.1)
	// Chrome configures a Baseline decoder, rejects every frame, and ICE
	// fails after ~30s.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	source.WaitForVideoParams(waitCtx)
	waitCancel()
	var (
		spsForLog, ppsForLog []byte
		cameraPLI            string
	)
	if vt := source.VideoTrack(); vt != nil && len(vt.SPS) >= 4 {
		cameraPLI = clampLevelIDC(hex.EncodeToString(vt.SPS[1:4]))
		sdpFmtpLine = fmt.Sprintf("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=%s", cameraPLI)
		spsForLog, ppsForLog = vt.SPS, vt.PPS
	}
	slog.Info("WebRTC offer received",
		"camera", cameraName,
		"sdpFmtpLine", sdpFmtpLine,
		"spsLen", len(spsForLog),
		"sps", hex.EncodeToString(spsForLog),
		"ppsLen", len(ppsForLog),
		"pps", hex.EncodeToString(ppsForLog),
	)

	// Register only the codecs we'll actually send.
	//
	// RTCPFeedback is advertised in the answer SDP (a=rtcp-fb lines) so the
	// browser's H.264 decoder commits to a full receive pipeline. Without
	// these, Chrome accepts packets and counts keyframes in inbound-rtp
	// stats but never delivers a single decoded frame to the <video>
	// element — `framesDecoded` stays at 0 indefinitely. We're a one-way
	// forwarder and won't act on NACK/PLI/FIR requests (the camera can't
	// honor them), but advertising support is what unblocks the decoder
	// path. The set mirrors Pion's RegisterDefaultCodecs and matches
	// Chrome's own offer.
	h264Feedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb"},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "transport-cc"},
	}
	h264Capability := webrtc.RTPCodecCapability{
		MimeType:     webrtc.MimeTypeH264,
		ClockRate:    90000,
		SDPFmtpLine:  sdpFmtpLine,
		RTCPFeedback: h264Feedback,
	}
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: h264Capability,
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register video codec: %w", err)
	}

	// Register an audio codec unconditionally. Chrome's offer always includes
	// m=audio; if Pion answers with no audio codec registered, the answer's
	// m=audio is rejected (port=0) and Pion pulls it out of the BUNDLE group.
	// Pion still attaches every ICE candidate to the first m-line in the
	// answer, so when audio is the rejected first m-line the candidates land
	// on a dead port and the active video m-line is left with none — Chrome
	// then never moves past iceConnectionState=new and the stream stalls.
	//
	// Use the camera's actual codec when it's G.711-compatible. Otherwise
	// register PCMU as a placeholder: it keeps the m=audio negotiation alive
	// (non-zero port, included in BUNDLE) without forcing us to transcode.
	audioCodec := audioCodecForTrack(source.AudioTrack())
	if audioCodec == nil {
		audioCodec = &webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypePCMU,
				ClockRate: 8000,
				Channels:  1,
			},
			PayloadType: 0,
		}
	}
	if err := me.RegisterCodec(*audioCodec, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register audio codec: %w", err)
	}
	audioPassthrough := audioCodecForTrack(source.AudioTrack()) != nil

	// Force IPv4 only — IPv6 UDP causes packet loss on some networks
	se := webrtc.SettingEngine{}
	se.SetIPFilter(func(ip net.IP) bool {
		return ip.To4() != nil
	})
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})

	// Disable default interceptors. Pion's NACK responder buffers each outbound
	// RTP packet in a 1460-byte pool slot (interceptor/internal/rtpbuffer),
	// rejecting any packet with payload larger than 1460 bytes with
	// io.ErrShortBuffer. Cameras commonly emit RTP packets up to ~1460–1500
	// bytes, so leaving this on silently drops the majority of video — only
	// fragments small enough to fit slip through. We're a one-way forwarder:
	// the camera can't honor NACK retransmission requests, so caching outbound
	// packets buys us nothing. TWCC and stats interceptors are likewise dead
	// weight for our use case.
	ir := &interceptor.Registry{}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	)
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: sm.iceServers,
	})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		h264Capability,
		"video",
		fmt.Sprintf("vedetta-%s", cameraName),
	)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create video track: %w", err)
	}

	if _, err := pc.AddTrack(videoTrack); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add video track: %w", err)
	}

	peer := &peerState{
		pc:         pc,
		cameraName: cameraName,
		video:      &trackState{track: videoTrack},
	}
	peer.startStatsLogger(context.Background())
	// The stats logger runs until ICE disconnect on the success path. If we
	// abandon the peer on any error below, cancel it here so its goroutine does
	// not leak.
	handoffOK := false
	defer func() {
		if !handoffOK {
			peer.statsCancel()
		}
	}()
	if vt := source.VideoTrack(); vt != nil {
		if len(vt.SPS) > 0 {
			peer.sps = append([]byte(nil), vt.SPS...)
		}
		if len(vt.PPS) > 0 {
			peer.pps = append([]byte(nil), vt.PPS...)
		}
	}

	// Add audio track if the camera's audio is browser-compatible (G.711).
	// When it isn't (e.g. AAC), add an inactive audio transceiver instead so
	// the answer's m=audio is accepted (non-zero port) and stays inside the
	// BUNDLE group — otherwise Chrome's ICE candidate pairing breaks. See
	// the placeholder codec comment above.
	if audioPassthrough {
		audioTrack, err := webrtc.NewTrackLocalStaticRTP(
			audioCodec.RTPCodecCapability,
			"audio",
			fmt.Sprintf("vedetta-%s-audio", cameraName),
		)
		if err != nil {
			slog.Warn("failed to create audio track, continuing without audio", "error", err)
		} else if _, err := pc.AddTrack(audioTrack); err != nil {
			slog.Warn("failed to add audio track, continuing without audio", "error", err)
		} else {
			peer.audio = &trackState{track: audioTrack}
			slog.Info("WebRTC audio enabled", "camera", cameraName, "codec", source.AudioTrack().Codec)
		}
	} else {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionInactive,
		}); err != nil {
			slog.Warn("failed to add inactive audio transceiver", "error", err)
		}
	}

	if sm.hub == nil {
		_ = pc.Close()
		return nil, fmt.Errorf("no RTSP hub configured")
	}

	// Get or create the consumer for this RTSP URL
	consumer := sm.getOrCreateConsumer(cameraName, rtspURL)
	consumer.addPeer(peer)
	var activateOnce sync.Once
	removePeer := func() {
		consumer.removePeer(peer)
		sm.removeConsumerIfEmpty(rtspURL, consumer)
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Info("WebRTC ICE state changed", "camera", cameraName, "state", state.String())
		if state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted {
			activateOnce.Do(func() { peer.activateTransport(source) })
		}
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateClosed {
			if peer.statsCancel != nil {
				peer.statsCancel()
			}
			removePeer()
			_ = pc.Close()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		removePeer()
		_ = pc.Close()
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		removePeer()
		_ = pc.Close()
		return nil, fmt.Errorf("create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		removePeer()
		_ = pc.Close()
		return nil, fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering to complete
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	finalAnswer := pc.LocalDescription()
	if cameraPLI != "" {
		finalAnswer = &webrtc.SessionDescription{
			Type: finalAnswer.Type,
			SDP:  rewriteAnswerProfileLevelID(finalAnswer.SDP, cameraPLI),
		}
	}
	// The peer is fully wired up and handed to the consumer; the stats logger
	// now lives until ICE disconnect.
	handoffOK = true
	return finalAnswer, nil
}

func (sm *StreamManager) getOrCreateConsumer(cameraName, rtspURL string) *webrtcConsumer {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	source := sm.hub.GetOrCreate(rtspURL)
	if c, ok := sm.consumers[rtspURL]; ok {
		if c.isAttachedTo(source) {
			// Fill cameraName if the consumer was created without one (belt-and-suspenders).
			if c.cameraName == "" {
				c.cameraName = cameraName
			}
			return c
		}
		c.detachFromSource(c)
		delete(sm.consumers, rtspURL)
	}

	c := &webrtcConsumer{cameraName: cameraName}
	sm.consumers[rtspURL] = c

	c.attachToSource(source, c)

	return c
}

func (sm *StreamManager) removeConsumerIfEmpty(rtspURL string, expected *webrtcConsumer) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	current, ok := sm.consumers[rtspURL]
	if !ok || current != expected {
		return
	}

	expected.mu.RLock()
	count := len(expected.peers)
	expected.mu.RUnlock()
	if count > 0 {
		return
	}

	expected.detachFromSource(expected)
	delete(sm.consumers, rtspURL)
}

// ClientCounts returns the number of connected WebRTC peers per camera name.
func (sm *StreamManager) ClientCounts() map[string]int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make(map[string]int, len(sm.consumers))
	for _, c := range sm.consumers {
		c.mu.RLock()
		out[c.cameraName] += len(c.peers)
		c.mu.RUnlock()
	}
	return out
}

// Close shuts down all sessions and peer connections.
func (sm *StreamManager) Close() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, consumer := range sm.consumers {
		consumer.mu.Lock()
		for _, peer := range consumer.peers {
			_ = peer.pc.Close()
		}
		consumer.peers = nil
		consumer.mu.Unlock()

		consumer.detachFromSource(consumer)
	}
	sm.consumers = make(map[string]*webrtcConsumer)
}
