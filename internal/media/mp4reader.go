package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	gomp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// hlsInUsePaths tracks fMP4 file paths currently being served via HLS.
// The recompression job checks this before replacing a file.
var hlsInUsePaths sync.Map

// markHLSPathInUse registers a file path as actively being served.
func markHLSPathInUse(path string) {
	hlsInUsePaths.Store(path, struct{}{})
}

// unmarkHLSPathInUse removes a file path from the in-use set.
func unmarkHLSPathInUse(path string) {
	hlsInUsePaths.Delete(path)
}

// HLSPathInUse reports whether a file is currently being served via HLS.
func HLSPathInUse(path string) bool {
	_, ok := hlsInUsePaths.Load(path)
	return ok
}

// ProbeDuration reads the duration of an MP4 file.
// For standard MP4: reads moov/mvhd. For fMP4: computes from fragments.
func ProbeDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Try moov-based duration first (standard MP4)
	dur, err := probeMoovDuration(f)
	if err == nil && dur > 0 {
		return dur, nil
	}

	// For fMP4 (moov duration is 0 or no moov), compute from fragments
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return probeFMP4Duration(f)
}

func probeMoovDuration(r io.ReadSeeker) (time.Duration, error) {
	for {
		var boxHeader [8]byte
		if _, err := io.ReadFull(r, boxHeader[:]); err != nil {
			return 0, fmt.Errorf("read box header: %w", err)
		}

		size := int64(binary.BigEndian.Uint32(boxHeader[:4]))
		boxType := string(boxHeader[4:8])

		switch size {
		case 1:
			var extSize [8]byte
			if _, err := io.ReadFull(r, extSize[:]); err != nil {
				return 0, fmt.Errorf("read extended size: %w", err)
			}
			size = int64(binary.BigEndian.Uint64(extSize[:]))
			size -= 16
		case 0:
			return 0, fmt.Errorf("unsupported box size 0")
		default:
			size -= 8
		}

		if boxType == "moov" {
			return findMvhdDuration(r, size)
		}

		if _, err := r.Seek(size, io.SeekCurrent); err != nil {
			return 0, fmt.Errorf("skip box %s: %w", boxType, err)
		}
	}
}

func findMvhdDuration(r io.ReadSeeker, moovSize int64) (time.Duration, error) {
	end, _ := r.Seek(0, io.SeekCurrent)
	end += moovSize

	for {
		pos, _ := r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}

		var boxHeader [8]byte
		if _, err := io.ReadFull(r, boxHeader[:]); err != nil {
			return 0, fmt.Errorf("read box header in moov: %w", err)
		}

		size := int64(binary.BigEndian.Uint32(boxHeader[:4]))
		boxType := string(boxHeader[4:8])

		if size == 1 {
			var extSize [8]byte
			if _, err := io.ReadFull(r, extSize[:]); err != nil {
				return 0, err
			}
			size = int64(binary.BigEndian.Uint64(extSize[:]))
			size -= 16
		} else {
			size -= 8
		}

		if boxType == "mvhd" {
			return parseMvhd(r)
		}

		if _, err := r.Seek(size, io.SeekCurrent); err != nil {
			return 0, err
		}
	}

	return 0, fmt.Errorf("mvhd box not found")
}

func parseMvhd(r io.Reader) (time.Duration, error) {
	var version [1]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return 0, err
	}

	var flags [3]byte
	if _, err := io.ReadFull(r, flags[:]); err != nil {
		return 0, err
	}

	if version[0] == 0 {
		var buf [16]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		timescale := binary.BigEndian.Uint32(buf[8:12])
		duration := binary.BigEndian.Uint32(buf[12:16])
		if timescale == 0 {
			return 0, nil
		}
		return time.Duration(float64(duration) / float64(timescale) * float64(time.Second)), nil
	}

	var buf [28]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	timescale := binary.BigEndian.Uint32(buf[16:20])
	duration := binary.BigEndian.Uint64(buf[20:28])
	if timescale == 0 {
		return 0, nil
	}
	return time.Duration(float64(duration) / float64(timescale) * float64(time.Second)), nil
}

// probeFMP4Duration computes duration from fMP4 fragments.
func probeFMP4Duration(r io.ReadSeeker) (time.Duration, error) {
	var maxDecodeTime uint64
	var lastDuration uint32
	var timeScale uint32

	_, err := gomp4.ReadBoxStructure(r, func(h *gomp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeMoov():
			return h.Expand()
		case gomp4.BoxTypeTrak():
			return h.Expand()
		case gomp4.BoxTypeMdia():
			return h.Expand()
		case gomp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mdhd := box.(*gomp4.Mdhd)
			if timeScale == 0 {
				timeScale = mdhd.Timescale
			}
			return nil, nil
		case gomp4.BoxTypeMoof():
			return h.Expand()
		case gomp4.BoxTypeTraf():
			return h.Expand()
		case gomp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdt := box.(*gomp4.Tfdt)
			decodeTime := tfdt.GetBaseMediaDecodeTime()
			if decodeTime >= maxDecodeTime {
				maxDecodeTime = decodeTime
			}
			return nil, nil
		case gomp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*gomp4.Trun)
			var totalDur uint32
			for _, e := range trun.Entries {
				totalDur += e.SampleDuration
			}
			lastDuration = totalDur
			return nil, nil
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}

	if timeScale == 0 {
		timeScale = 90000
	}

	totalTicks := maxDecodeTime + uint64(lastDuration)
	return time.Duration(float64(totalTicks) / float64(timeScale) * float64(time.Second)), nil
}

// trafEntry is the per-track timing inside a moof.
type trafEntry struct {
	trackID    uint32
	decodeTime uint64
	duration   uint32
	isSync     bool // true if the first sample in this traf is a sync sample (keyframe)
}

// fragment represents a single moof+mdat pair on disk. A moof contains one or
// more trafs (one per track), all sharing the moof's mdat. Per-track timing is
// stored in trafs.
type fragment struct {
	moofOffset int64
	moofSize   int64
	mdatOffset int64
	mdatSize   int64
	trafs      []trafEntry
}

// traf returns the entry for trackID, or nil if not present.
func (f *fragment) traf(trackID uint32) *trafEntry {
	for i := range f.trafs {
		if f.trafs[i].trackID == trackID {
			return &f.trafs[i]
		}
	}
	return nil
}

// boxLoc stores the position and size of a top-level box.
type boxLoc struct {
	offset int64
	size   int64
}

// TrimMP4 copies fragments from an fMP4 file that fall within [start, start+duration].
func TrimMP4(inputPath, outputPath string, start, duration time.Duration) error {
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Index all box locations (includes per-track timescales)
	initBoxes, fragments, trackTimeScales, err := indexFile(in)
	if err != nil {
		return fmt.Errorf("index file: %w", err)
	}

	// Copy init segment boxes
	for _, loc := range initBoxes {
		if _, err := in.Seek(loc.offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(out, in, loc.size); err != nil {
			return err
		}
	}

	videoTrackID := findVideoTrack(fragments, trackTimeScales)

	// Copy matching fragments, adjusting timestamps. Window matching uses the
	// video traf since the requested [start, start+duration) is in wall time
	// and the video timeline is the canonical clock.
	var newSeqNum uint32 = 1
	// Pre-populate base times so the first kept fragment's tfdt is rewritten to 0.
	// patchTraf skips trafs whose trackID is missing from this map.
	newBaseTimes := make(map[uint32]uint64)
	for trackID := range trackTimeScales {
		newBaseTimes[trackID] = 0
	}
	for _, frag := range fragments {
		var refTraf *trafEntry
		if t := frag.traf(videoTrackID); t != nil {
			refTraf = t
		} else if len(frag.trafs) > 0 {
			refTraf = &frag.trafs[0]
		} else {
			continue
		}
		ts := trackTimeScales[refTraf.trackID]
		if ts == 0 {
			ts = 90000
		}
		startTick := uint64(start.Seconds() * float64(ts))
		endTick := startTick + uint64(duration.Seconds()*float64(ts))

		fragEnd := refTraf.decodeTime + uint64(refTraf.duration)
		if refTraf.decodeTime >= endTick || fragEnd <= startTick {
			continue
		}

		if err := copyFragmentAdjusted(in, out, frag, newSeqNum, newBaseTimes); err != nil {
			return fmt.Errorf("copy fragment: %w", err)
		}
		newSeqNum++
		for _, tr := range frag.trafs {
			newBaseTimes[tr.trackID] += uint64(tr.duration)
		}
	}

	return nil
}

// TrimMP4ToWriter writes a trimmed fMP4 starting at the given offset to w.
// This is used for HTTP playback so the browser receives video starting at
// the requested position without needing client-side seeking.
func TrimMP4ToWriter(inputPath string, w io.Writer, start time.Duration) error {
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	initBoxes, fragments, trackTimeScales, err := indexFile(in)
	if err != nil {
		return fmt.Errorf("index file: %w", err)
	}

	// Copy init segment boxes (ftyp, moov)
	for _, loc := range initBoxes {
		if _, err := in.Seek(loc.offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(w, in, loc.size); err != nil {
			return err
		}
	}

	videoTrackID := findVideoTrack(fragments, trackTimeScales)

	// Copy fragments from the start offset onward
	var newSeqNum uint32 = 1
	newBaseTimes := make(map[uint32]uint64)
	for trackID := range trackTimeScales {
		newBaseTimes[trackID] = 0
	}
	for _, frag := range fragments {
		var refTraf *trafEntry
		if t := frag.traf(videoTrackID); t != nil {
			refTraf = t
		} else if len(frag.trafs) > 0 {
			refTraf = &frag.trafs[0]
		} else {
			continue
		}
		ts := trackTimeScales[refTraf.trackID]
		if ts == 0 {
			ts = 90000
		}
		startTick := uint64(start.Seconds() * float64(ts))
		if refTraf.decodeTime+uint64(refTraf.duration) <= startTick {
			continue
		}

		if err := copyFragmentAdjusted(in, w, frag, newSeqNum, newBaseTimes); err != nil {
			return fmt.Errorf("copy fragment: %w", err)
		}
		newSeqNum++
		for _, tr := range frag.trafs {
			newBaseTimes[tr.trackID] += uint64(tr.duration)
		}
	}

	return nil
}

// ConcatMP4 concatenates multiple fMP4 files with continuous timing.
func ConcatMP4(inputs []string, outputPath string, start, duration time.Duration) error {
	if len(inputs) == 0 {
		return fmt.Errorf("no inputs")
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	var globalSeqNum uint32 = 1
	globalBaseTimes := make(map[uint32]uint64)
	var videoTimeScale uint32
	var videoTrackID uint32
	initWritten := false

	for _, path := range inputs {
		in, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}

		initBoxes, fragments, trackTimeScales, err := indexFile(in)
		if err != nil {
			in.Close()
			return fmt.Errorf("index %s: %w", path, err)
		}

		if videoTrackID == 0 {
			videoTrackID = findVideoTrack(fragments, trackTimeScales)
			videoTimeScale = trackTimeScales[videoTrackID]
			for trackID := range trackTimeScales {
				if _, ok := globalBaseTimes[trackID]; !ok {
					globalBaseTimes[trackID] = 0
				}
			}
		}

		if !initWritten {
			for _, loc := range initBoxes {
				if _, err := in.Seek(loc.offset, io.SeekStart); err != nil {
					in.Close()
					return err
				}
				if _, err := io.CopyN(out, in, loc.size); err != nil {
					in.Close()
					return err
				}
			}
			initWritten = true
		}

		for _, frag := range fragments {
			if err := copyFragmentAdjusted(in, out, frag, globalSeqNum, globalBaseTimes); err != nil {
				in.Close()
				return fmt.Errorf("copy fragment from %s: %w", path, err)
			}
			globalSeqNum++
			for _, tr := range frag.trafs {
				globalBaseTimes[tr.trackID] += uint64(tr.duration)
			}
		}

		in.Close()
	}

	// Apply start/duration trimming if requested
	if start > 0 || duration > 0 {
		if videoTimeScale == 0 {
			videoTimeScale = 90000
		}
		totalDur := time.Duration(float64(globalBaseTimes[videoTrackID]) / float64(videoTimeScale) * float64(time.Second))
		if start > 0 || (duration > 0 && duration < totalDur) {
			// Close output before rename so TrimMP4 can rewrite it
			out.Close()
			tmpPath := outputPath + ".tmp"
			if err := os.Rename(outputPath, tmpPath); err != nil {
				return err
			}
			defer os.Remove(tmpPath)
			return TrimMP4(tmpPath, outputPath, start, duration)
		}
	}

	return nil
}

// indexFile scans an fMP4 file and returns init box locations, fragment metadata,
// and per-track timescales (from mdhd boxes in the init segment).
func indexFile(r io.ReadSeeker) (initBoxes []boxLoc, fragments []fragment, trackTimeScales map[uint32]uint32, err error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, nil, nil, err
	}

	trackTimeScales = make(map[uint32]uint32)
	var currentTrackID uint32

	// First pass with ReadBoxStructure to get fragment timing info.
	// One fragment is emitted per moof (with one trafEntry per traf inside it).
	var currentFrag *fragment
	var currentTraf *trafEntry
	_, err = gomp4.ReadBoxStructure(r, func(h *gomp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeFtyp(), gomp4.BoxTypeMoov(), gomp4.BoxTypeStyp():
			initBoxes = append(initBoxes, boxLoc{
				offset: int64(h.BoxInfo.Offset),
				size:   int64(h.BoxInfo.Size),
			})
			if h.BoxInfo.Type == gomp4.BoxTypeMoov() {
				return h.Expand()
			}
			return nil, nil

		case gomp4.BoxTypeTrak():
			currentTrackID = 0
			return h.Expand()

		case gomp4.BoxTypeTkhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tkhd := box.(*gomp4.Tkhd)
			currentTrackID = tkhd.TrackID
			return nil, nil

		case gomp4.BoxTypeMdia():
			return h.Expand()

		case gomp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mdhd := box.(*gomp4.Mdhd)
			if currentTrackID != 0 {
				trackTimeScales[currentTrackID] = mdhd.Timescale
			}
			return nil, nil

		case gomp4.BoxTypeMoof():
			currentFrag = &fragment{
				moofOffset: int64(h.BoxInfo.Offset),
				moofSize:   int64(h.BoxInfo.Size),
			}
			currentTraf = nil
			return h.Expand()

		case gomp4.BoxTypeTraf():
			if currentFrag != nil {
				currentFrag.trafs = append(currentFrag.trafs, trafEntry{})
				currentTraf = &currentFrag.trafs[len(currentFrag.trafs)-1]
			}
			return h.Expand()

		case gomp4.BoxTypeTfhd():
			if currentTraf != nil {
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				tfhd := box.(*gomp4.Tfhd)
				currentTraf.trackID = tfhd.TrackID
				if tfhd.GetFlags()&0x000020 != 0 {
					currentTraf.isSync = tfhd.DefaultSampleFlags&0x00010000 == 0
				} else {
					currentTraf.isSync = true
				}
			}
			return nil, nil

		case gomp4.BoxTypeTfdt():
			if currentTraf != nil {
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				tfdt := box.(*gomp4.Tfdt)
				currentTraf.decodeTime = tfdt.GetBaseMediaDecodeTime()
			}
			return nil, nil

		case gomp4.BoxTypeTrun():
			if currentTraf != nil {
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				trun := box.(*gomp4.Trun)
				var totalDur uint32
				for _, e := range trun.Entries {
					totalDur += e.SampleDuration
				}
				currentTraf.duration += totalDur

				trunFlags := trun.GetFlags()
				if trunFlags&0x000004 != 0 {
					currentTraf.isSync = trun.FirstSampleFlags&0x00010000 == 0
				} else if trunFlags&0x000400 != 0 && len(trun.Entries) > 0 {
					currentTraf.isSync = trun.Entries[0].SampleFlags&0x00010000 == 0
				}
			}
			return nil, nil

		case gomp4.BoxTypeMdat():
			if currentFrag != nil {
				currentFrag.mdatOffset = int64(h.BoxInfo.Offset)
				currentFrag.mdatSize = int64(h.BoxInfo.Size)
				fragments = append(fragments, *currentFrag)
				currentFrag = nil
				currentTraf = nil
			}
			return nil, nil
		}
		return nil, nil
	})

	return initBoxes, fragments, trackTimeScales, err
}

// copyFragmentAdjusted copies a moof+mdat pair, rewriting the sequence number
// and base decode time.
func copyFragmentAdjusted(src io.ReadSeeker, dst io.Writer, frag fragment, seqNum uint32, baseTimes map[uint32]uint64) error {
	// Read the entire moof into memory (typically small)
	if _, err := src.Seek(frag.moofOffset, io.SeekStart); err != nil {
		return err
	}
	moofData := make([]byte, frag.moofSize)
	if _, err := io.ReadFull(src, moofData); err != nil {
		return err
	}

	patchMoof(moofData, seqNum, baseTimes)

	if _, err := dst.Write(moofData); err != nil {
		return err
	}

	// Copy mdat as-is
	if _, err := src.Seek(frag.mdatOffset, io.SeekStart); err != nil {
		return err
	}
	_, err := io.CopyN(dst, src, frag.mdatSize)
	return err
}

// HLSSegmentInfo describes one HLS segment's location within an fMP4 file.
type HLSSegmentInfo struct {
	ByteStart int64
	ByteEnd   int64
	Duration  float64
}

// HLSPlaylistResult contains the generated playlist and segment index for serving.
type HLSPlaylistResult struct {
	Playlist string
	// FileSegments maps segment file paths to their HLS segment ranges, keyed
	// by a segment index (0-based across all files).
	Segments []HLSSegmentRef
}

// HLSSegmentRef maps an HLS segment index to a file path and byte range.
type HLSSegmentRef struct {
	FilePath  string
	ByteStart int64
	ByteEnd   int64
}

// GenerateHLSPlaylist builds an HLS m3u8 playlist for one or more fMP4 files.
// Instead of byte-range addressing (which fails with multi-track moofs), it uses
// indexed segment URLs. The server re-segments each chunk on the fly using
// ServeHLSSegment. The baseURI is used to construct segment URLs like
// "{baseURI}/hls/{segNum}.m4s" and init segment URLs like "{baseURI}/hls/init.mp4".
//
// Each file's fragments carry decode times that restart at zero, so an
// EXT-X-DISCONTINUITY is emitted at every file boundary. fileStarts optionally
// supplies each file's wall-clock start (the instant of media tick 0); when
// present, an EXT-X-PROGRAM-DATE-TIME tag anchors the first segment of each
// file so players can map playback positions back to wall-clock time. When the
// requested start falls inside the retained first GOP, EXT-X-START asks the
// player to present the exact offset after decoding that keyframe. Pass nil to
// omit the program-date-time tags.
func GenerateHLSPlaylist(paths []string, baseURIs []string, fileStarts []time.Time, start time.Duration) (*HLSPlaylistResult, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no paths provided")
	}
	if len(paths) != len(baseURIs) {
		return nil, fmt.Errorf("paths and baseURIs length mismatch")
	}
	if fileStarts != nil && len(fileStarts) != len(paths) {
		return nil, fmt.Errorf("paths and fileStarts length mismatch")
	}

	type hlsSeg struct {
		fileIdx   int
		duration  float64
		startTick uint64 // decode time of the segment's first fragment
		videoTS   uint32 // the file's video timescale, for tick-to-time conversion
		ref       HLSSegmentRef
	}
	var segments []hlsSeg

	type initInfo struct {
		fileIdx   int
		byteStart int64
		byteLen   int64
	}
	var segInits []initInfo

	for fileIdx, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}

		initBoxes, fragments, trackTimeScales, err := indexFile(f)
		f.Close()
		if err != nil {
			// On EOF the file is still being written. Keep any fragments already
			// parsed so in-progress segments contribute their complete GOPs to the
			// playlist. Skip only if no usable data was recovered or the error is
			// unrelated to truncation.
			if (!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) || len(fragments) == 0 {
				continue
			}
		}

		var initSize int64
		if len(initBoxes) > 0 {
			last := initBoxes[len(initBoxes)-1]
			initSize = last.offset + last.size
		}

		videoTrackID := findVideoTrack(fragments, trackTimeScales)
		videoTS := trackTimeScales[videoTrackID]
		if videoTS == 0 {
			videoTS = 90000
		}

		var startTick uint64
		if fileIdx == 0 && start > 0 {
			startTick = uint64(start.Seconds() * float64(videoTS))
		}

		const targetSegDur = 4.0
		var curByteStart int64 = -1
		var curByteEnd int64
		var curDurTicks uint64
		var curStartTick uint64

		flush := func() {
			if curByteStart < 0 {
				return
			}
			dur := float64(curDurTicks) / float64(videoTS)
			segments = append(segments, hlsSeg{
				fileIdx:   fileIdx,
				duration:  dur,
				startTick: curStartTick,
				videoTS:   videoTS,
				ref: HLSSegmentRef{
					FilePath:  path,
					ByteStart: curByteStart,
					ByteEnd:   curByteEnd,
				},
			})
			segInits = append(segInits, initInfo{fileIdx: fileIdx, byteStart: 0, byteLen: initSize})
			curByteStart = -1
			curByteEnd = 0
			curDurTicks = 0
		}

		for _, frag := range fragments {
			vTraf := frag.traf(videoTrackID)
			if vTraf == nil {
				continue
			}
			if fileIdx == 0 && start > 0 {
				fragEndTick := vTraf.decodeTime + uint64(vTraf.duration)
				if fragEndTick <= startTick {
					continue
				}
			}

			fragEnd := frag.mdatOffset + frag.mdatSize
			curDurSec := float64(curDurTicks) / float64(videoTS)
			if vTraf.isSync && curByteStart >= 0 && curDurSec >= targetSegDur {
				flush()
			}

			if curByteStart < 0 {
				curByteStart = frag.moofOffset
				curStartTick = vTraf.decodeTime
			}
			if fragEnd > curByteEnd {
				curByteEnd = fragEnd
			}
			curDurTicks += uint64(vTraf.duration)
		}
		flush()
	}

	if len(segments) == 0 {
		return nil, fmt.Errorf("no segments produced")
	}

	var maxDur float64
	for _, seg := range segments {
		if seg.duration > maxDur {
			maxDur = seg.duration
		}
	}
	targetDuration := int(math.Ceil(maxDur))
	if targetDuration < 1 {
		targetDuration = 1
	}

	// Build the m3u8 playlist with indexed segment URLs
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDuration)
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	if start > 0 && segments[0].fileIdx == 0 && segments[0].videoTS > 0 {
		firstMediaStart := float64(segments[0].startTick) / float64(segments[0].videoTS)
		preferredStart := start.Seconds() - firstMediaStart
		if preferredStart > 0.001 {
			// PRECISE=YES is important here: starting only at the containing GOP
			// recreates the visible "snap to a few start times" this tag removes.
			fmt.Fprintf(&b, "#EXT-X-START:TIME-OFFSET=%.6f,PRECISE=YES\n", preferredStart)
		}
	}

	var refs []HLSSegmentRef
	lastFileIdx := -1
	for i, seg := range segments {
		init := segInits[i]
		if init.fileIdx != lastFileIdx {
			if lastFileIdx != -1 {
				// Decode times restart at zero in every file; without an
				// explicit discontinuity players treat the jump backwards as
				// corrupt input or stall trying to reconcile timestamps.
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			// Init segment served directly (not byte-range) for Safari compatibility
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s/hls/init.mp4\"\n",
				baseURIs[init.fileIdx])
			if fileStarts != nil {
				// Anchor this discontinuity sequence to wall-clock time:
				// file start (media tick 0) plus the first fragment's offset.
				pdt := fileStarts[init.fileIdx].Add(
					time.Duration(seg.startTick * uint64(time.Second) / uint64(seg.videoTS)))
				fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
					pdt.UTC().Format("2006-01-02T15:04:05.000Z"))
			}
			lastFileIdx = init.fileIdx
		}

		fmt.Fprintf(&b, "#EXTINF:%.6f,\n", seg.duration)
		fmt.Fprintf(&b, "%s/hls/%d\n", baseURIs[seg.fileIdx], len(refs))
		refs = append(refs, seg.ref)
	}

	b.WriteString("#EXT-X-ENDLIST\n")

	return &HLSPlaylistResult{
		Playlist: b.String(),
		Segments: refs,
	}, nil
}

// ServeHLSSegment reads a byte range from an fMP4 file containing one or more
// moof+mdat pairs, unmarshals them, and re-marshals as clean fMP4 that MSE/hls.js
// can consume. This is needed because per-GOP recordings use multi-track moofs
// (video+audio trafs in a single moof) which browsers reject.
func ServeHLSSegment(w io.Writer, filePath string, byteStart, byteEnd int64) error {
	markHLSPathInUse(filePath)
	defer unmarkHLSPathInUse(filePath)

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	size := byteEnd - byteStart
	if _, err := f.Seek(byteStart, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	// Unmarshal the raw moof+mdat pairs into structured Parts
	var parts fmp4.Parts
	if err := parts.Unmarshal(buf); err != nil {
		return fmt.Errorf("unmarshal fmp4: %w", err)
	}

	// Clean up parts for MSE compatibility:
	// 1. Split multi-track Parts into single-track Parts (browsers reject multi-traf moofs)
	// 2. Strip in-band SPS/PPS NALs from video samples (some cameras embed them in keyframes,
	//    which conflicts with the init segment's avcC and causes Safari bufferAppendError)
	var singleTrackParts fmp4.Parts
	for _, p := range parts {
		tracks := p.Tracks
		if len(tracks) > 1 {
			for _, tr := range tracks {
				stripInBandParamSets(tr)
				singleTrackParts = append(singleTrackParts, &fmp4.Part{
					SequenceNumber: p.SequenceNumber,
					Tracks:         []*fmp4.PartTrack{tr},
				})
			}
		} else {
			for _, tr := range tracks {
				stripInBandParamSets(tr)
			}
			singleTrackParts = append(singleTrackParts, p)
		}
	}

	ws := &writeSeeker{buf: &bytes.Buffer{}}
	if err := singleTrackParts.Marshal(ws); err != nil {
		return fmt.Errorf("marshal fmp4: %w", err)
	}

	_, err = w.Write(ws.buf.Bytes())
	return err
}

// stripInBandParamSets removes SPS (NAL type 7) and PPS (NAL type 8) NAL units
// from video sample payloads. Some cameras embed these in-band before keyframes,
// which conflicts with the init segment's avcC and causes Safari's MSE to reject
// the data with bufferAppendError.
func stripInBandParamSets(tr *fmp4.PartTrack) {
	for _, s := range tr.Samples {
		if len(s.Payload) < 5 {
			continue
		}
		// Check first NAL type — if it's SPS or PPS, strip parameter set NALs
		firstNALType := s.Payload[4] & 0x1f
		if firstNALType != 7 && firstNALType != 8 {
			continue // not a parameter set, skip
		}
		// Rebuild payload without SPS/PPS NALs
		var cleaned []byte
		pos := 0
		for pos+4 < len(s.Payload) {
			nalLen := int(binary.BigEndian.Uint32(s.Payload[pos : pos+4]))
			if pos+4+nalLen > len(s.Payload) {
				break
			}
			nalType := s.Payload[pos+4] & 0x1f
			if nalType != 7 && nalType != 8 { // keep everything except SPS/PPS
				cleaned = append(cleaned, s.Payload[pos:pos+4+nalLen]...)
			}
			pos += 4 + nalLen
		}
		if len(cleaned) > 0 {
			s.Payload = cleaned
		}
	}
}

// writeSeeker wraps a bytes.Buffer to implement io.WriteSeeker for fmp4.Marshal.
type writeSeeker struct {
	buf *bytes.Buffer
	pos int
}

func (ws *writeSeeker) Write(p []byte) (int, error) {
	// If writing past current position, extend the buffer
	if ws.pos < ws.buf.Len() {
		// Overwrite existing bytes
		copy(ws.buf.Bytes()[ws.pos:], p)
		ws.pos += len(p)
		if ws.pos > ws.buf.Len() {
			ws.buf.Truncate(ws.pos)
		}
		return len(p), nil
	}
	n, err := ws.buf.Write(p)
	ws.pos += n
	return n, err
}

func (ws *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	var newPos int
	switch whence {
	case io.SeekStart:
		newPos = int(offset)
	case io.SeekCurrent:
		newPos = ws.pos + int(offset)
	case io.SeekEnd:
		newPos = ws.buf.Len() + int(offset)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("negative seek position")
	}
	// Extend buffer if seeking past end
	if newPos > ws.buf.Len() {
		ws.buf.Write(make([]byte, newPos-ws.buf.Len()))
	}
	ws.pos = newPos
	return int64(newPos), nil
}

// findVideoTrack identifies the video track ID from fragments and timescales.
// It picks the track with the highest timescale (video is typically 90000),
// falling back to the track present in the most fragments.
func findVideoTrack(fragments []fragment, trackTimeScales map[uint32]uint32) uint32 {
	// Try highest timescale first
	var bestID uint32
	var bestTS uint32
	for id, ts := range trackTimeScales {
		if ts > bestTS {
			bestTS = ts
			bestID = id
		}
	}
	if bestID != 0 {
		return bestID
	}

	// Fallback: track present in the most fragments
	counts := make(map[uint32]int)
	for _, f := range fragments {
		for _, tr := range f.trafs {
			counts[tr.trackID]++
		}
	}
	var maxCount int
	for id, c := range counts {
		if c > maxCount {
			maxCount = c
			bestID = id
		}
	}
	return bestID
}

// patchMoof modifies mfhd.SequenceNumber and each traf's tfdt.BaseMediaDecodeTime
// in raw moof bytes. Each traf is patched with the baseTime for its own trackID,
// so video and audio timelines stay independent after rebasing.
func patchMoof(data []byte, seqNum uint32, baseTimes map[uint32]uint64) {
	i := 8 // Skip moof box header
	for i+8 <= len(data) {
		boxSize := int(binary.BigEndian.Uint32(data[i : i+4]))
		boxType := string(data[i+4 : i+8])

		if boxSize < 8 || i+boxSize > len(data) {
			break
		}

		switch boxType {
		case "mfhd":
			if boxSize >= 16 {
				binary.BigEndian.PutUint32(data[i+12:i+16], seqNum)
			}
		case "traf":
			patchTraf(data[i+8:i+boxSize], baseTimes)
		}

		i += boxSize
	}
}

// patchTraf reads the traf's tfhd to learn its trackID, then rewrites its tfdt
// with baseTimes[trackID]. Tracks not present in baseTimes are left untouched.
func patchTraf(data []byte, baseTimes map[uint32]uint64) {
	var trackID uint32
	i := 0
	for i+8 <= len(data) {
		boxSize := int(binary.BigEndian.Uint32(data[i : i+4]))
		boxType := string(data[i+4 : i+8])

		if boxSize < 8 || i+boxSize > len(data) {
			break
		}

		if boxType == "tfhd" && boxSize >= 16 {
			trackID = binary.BigEndian.Uint32(data[i+12 : i+16])
		}

		i += boxSize
	}

	baseTime, ok := baseTimes[trackID]
	if !ok {
		return
	}

	i = 0
	for i+8 <= len(data) {
		boxSize := int(binary.BigEndian.Uint32(data[i : i+4]))
		boxType := string(data[i+4 : i+8])

		if boxSize < 8 || i+boxSize > len(data) {
			break
		}

		if boxType == "tfdt" {
			if boxSize >= 16 {
				version := data[i+8]
				if version == 0 {
					binary.BigEndian.PutUint32(data[i+12:i+16], uint32(baseTime))
				} else if boxSize >= 20 {
					binary.BigEndian.PutUint64(data[i+12:i+20], baseTime)
				}
			}
		}

		i += boxSize
	}
}
