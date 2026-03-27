package media

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	gomp4 "github.com/abema/go-mp4"
)

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

		if size == 1 {
			var extSize [8]byte
			if _, err := io.ReadFull(r, extSize[:]); err != nil {
				return 0, fmt.Errorf("read extended size: %w", err)
			}
			size = int64(binary.BigEndian.Uint64(extSize[:]))
			size -= 16
		} else if size == 0 {
			return 0, fmt.Errorf("unsupported box size 0")
		} else {
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

// fragment represents a moof+mdat pair with timing metadata.
type fragment struct {
	moofOffset int64
	moofSize   int64
	mdatOffset int64
	mdatSize   int64
	decodeTime uint64
	duration   uint32
	trackID    uint32
	isSync     bool // true if the first sample in this fragment is a sync sample (keyframe)
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

	// Copy matching fragments, adjusting timestamps
	var newSeqNum uint32 = 1
	newBaseTimes := make(map[uint32]uint64) // per-track base times
	for _, frag := range fragments {
		ts := trackTimeScales[frag.trackID]
		if ts == 0 {
			ts = 90000
		}
		startTick := uint64(start.Seconds() * float64(ts))
		endTick := startTick + uint64(duration.Seconds()*float64(ts))

		fragEnd := frag.decodeTime + uint64(frag.duration)
		if frag.decodeTime >= endTick || fragEnd <= startTick {
			continue
		}

		baseTime := newBaseTimes[frag.trackID]
		if err := copyFragmentAdjusted(in, out, frag, newSeqNum, baseTime); err != nil {
			return fmt.Errorf("copy fragment: %w", err)
		}
		newSeqNum++
		newBaseTimes[frag.trackID] = baseTime + uint64(frag.duration)
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

	// Copy fragments from the start offset onward
	var newSeqNum uint32 = 1
	newBaseTimes := make(map[uint32]uint64)
	for _, frag := range fragments {
		ts := trackTimeScales[frag.trackID]
		if ts == 0 {
			ts = 90000
		}
		startTick := uint64(start.Seconds() * float64(ts))
		if frag.decodeTime+uint64(frag.duration) <= startTick {
			continue
		}

		baseTime := newBaseTimes[frag.trackID]
		if err := copyFragmentAdjusted(in, w, frag, newSeqNum, baseTime); err != nil {
			return fmt.Errorf("copy fragment: %w", err)
		}
		newSeqNum++
		newBaseTimes[frag.trackID] = baseTime + uint64(frag.duration)
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
	var globalBaseTime uint64
	var timeScale uint32
	initWritten := false

	for _, path := range inputs {
		in, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}

		// Read timescale from the first file
		if timeScale == 0 {
			ts, _ := readTimeScale(in)
			if ts > 0 {
				timeScale = ts
			}
			if _, err := in.Seek(0, io.SeekStart); err != nil {
				in.Close()
				return err
			}
		}

		initBoxes, fragments, _, err := indexFile(in)
		if err != nil {
			in.Close()
			return fmt.Errorf("index %s: %w", path, err)
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
			if err := copyFragmentAdjusted(in, out, frag, globalSeqNum, globalBaseTime); err != nil {
				in.Close()
				return fmt.Errorf("copy fragment from %s: %w", path, err)
			}
			globalSeqNum++
			globalBaseTime += uint64(frag.duration)
		}

		in.Close()
	}

	// Apply start/duration trimming if requested
	if start > 0 || duration > 0 {
		if timeScale == 0 {
			timeScale = 90000
		}
		totalDur := time.Duration(float64(globalBaseTime) / float64(timeScale) * float64(time.Second))
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

// readTimeScale extracts the track timescale from moov/trak/mdia/mdhd.
func readTimeScale(r io.ReadSeeker) (uint32, error) {
	var ts uint32
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
			if ts == 0 {
				ts = mdhd.Timescale
			}
			return nil, nil
		}
		return nil, nil
	})
	return ts, err
}

// indexFile scans an fMP4 file and returns init box locations, fragment metadata,
// and per-track timescales (from mdhd boxes in the init segment).
func indexFile(r io.ReadSeeker) (initBoxes []boxLoc, fragments []fragment, trackTimeScales map[uint32]uint32, err error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, nil, nil, err
	}

	trackTimeScales = make(map[uint32]uint32)
	var currentTrackID uint32

	// First pass with ReadBoxStructure to get fragment timing info
	var currentFrag *fragment
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
			return h.Expand()

		case gomp4.BoxTypeTraf():
			// Each traf within a moof represents a different track.
			// Emit one fragment per traf so multi-track moofs (per-GOP)
			// produce separate video and audio fragment entries.
			if currentFrag != nil && currentFrag.trackID != 0 {
				// Save previous traf's fragment before starting a new one
				fragments = append(fragments, *currentFrag)
				currentFrag = &fragment{
					moofOffset: currentFrag.moofOffset,
					moofSize:   currentFrag.moofSize,
				}
			}
			return h.Expand()

		case gomp4.BoxTypeTfhd():
			if currentFrag != nil {
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				tfhd := box.(*gomp4.Tfhd)
				currentFrag.trackID = tfhd.TrackID
				if tfhd.GetFlags()&0x000020 != 0 {
					currentFrag.isSync = tfhd.DefaultSampleFlags&0x00010000 == 0
				} else {
					currentFrag.isSync = true
				}
			}
			return nil, nil

		case gomp4.BoxTypeTfdt():
			if currentFrag != nil {
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				tfdt := box.(*gomp4.Tfdt)
				currentFrag.decodeTime = tfdt.GetBaseMediaDecodeTime()
			}
			return nil, nil

		case gomp4.BoxTypeTrun():
			if currentFrag != nil {
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				trun := box.(*gomp4.Trun)
				var totalDur uint32
				for _, e := range trun.Entries {
					totalDur += e.SampleDuration
				}
				currentFrag.duration += totalDur

				trunFlags := trun.GetFlags()
				if trunFlags&0x000004 != 0 {
					currentFrag.isSync = trun.FirstSampleFlags&0x00010000 == 0
				} else if trunFlags&0x000400 != 0 && len(trun.Entries) > 0 {
					currentFrag.isSync = trun.Entries[0].SampleFlags&0x00010000 == 0
				}
			}
			return nil, nil

		case gomp4.BoxTypeMdat():
			if currentFrag != nil {
				currentFrag.mdatOffset = int64(h.BoxInfo.Offset)
				currentFrag.mdatSize = int64(h.BoxInfo.Size)
				fragments = append(fragments, *currentFrag)
				currentFrag = nil
			}
			return nil, nil
		}
		return nil, nil
	})

	return initBoxes, fragments, trackTimeScales, err
}

// copyFragmentAdjusted copies a moof+mdat pair, rewriting the sequence number
// and base decode time.
func copyFragmentAdjusted(src io.ReadSeeker, dst io.Writer, frag fragment, seqNum uint32, baseTime uint64) error {
	// Read the entire moof into memory (typically small)
	if _, err := src.Seek(frag.moofOffset, io.SeekStart); err != nil {
		return err
	}
	moofData := make([]byte, frag.moofSize)
	if _, err := io.ReadFull(src, moofData); err != nil {
		return err
	}

	patchMoof(moofData, seqNum, baseTime)

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

// GenerateHLSPlaylist builds an HLS m3u8 playlist with byte-range addressing for
// one or more fMP4 files. Each HLS segment starts at a video keyframe. The start
// parameter skips fragments before that time offset in the first file. The paths
// slice contains filesystem paths used to open and index each file, while uris
// contains the corresponding URIs that appear in the playlist output.
func GenerateHLSPlaylist(paths []string, uris []string, start time.Duration) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no paths provided")
	}
	if len(paths) != len(uris) {
		return "", fmt.Errorf("paths and uris length mismatch")
	}

	type hlsSegment struct {
		uri        string
		byteStart  int64
		byteLength int64
		duration   float64 // seconds
	}

	var segments []hlsSegment

	// Track which file index each segment belongs to, so we can insert
	// EXT-X-MAP tags when the file changes.
	type initInfo struct {
		uri       string
		byteStart int64
		byteLen   int64
	}
	var segmentInits []initInfo
	var fileInits []initInfo

	for fileIdx, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", path, err)
		}

		initBoxes, fragments, trackTimeScales, err := indexFile(f)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("index %s: %w", path, err)
		}

		// Compute init segment byte range (ftyp+moov+styp combined)
		var initStart, initEnd int64
		if len(initBoxes) > 0 {
			initStart = initBoxes[0].offset
			last := initBoxes[len(initBoxes)-1]
			initEnd = last.offset + last.size
		}
		fi := initInfo{
			uri:       uris[fileIdx],
			byteStart: initStart,
			byteLen:   initEnd - initStart,
		}
		fileInits = append(fileInits, fi)

		// Identify the video track: pick the track with the highest timescale,
		// falling back to the track with the most fragments.
		videoTrackID := findVideoTrack(fragments, trackTimeScales)
		videoTS := trackTimeScales[videoTrackID]
		if videoTS == 0 {
			videoTS = 90000
		}

		// Filter fragments by start time (first file only)
		var startTick uint64
		if fileIdx == 0 && start > 0 {
			startTick = uint64(start.Seconds() * float64(videoTS))
		}

		// Group fragments into HLS segments aligned to video keyframes.
		// Target ~4 seconds per segment to reduce parsing overhead (each segment
		// contains many per-frame moof+mdat pairs from the fMP4 recording format).
		const targetSegmentDuration = 4.0 // seconds
		var curByteStart int64 = -1
		var curByteEnd int64
		var curDurationTicks uint64

		flushSegment := func() {
			if curByteStart < 0 {
				return
			}
			dur := float64(curDurationTicks) / float64(videoTS)
			segments = append(segments, hlsSegment{
				uri:        uris[fileIdx],
				byteStart:  curByteStart,
				byteLength: curByteEnd - curByteStart,
				duration:   dur,
			})
			segmentInits = append(segmentInits, fi)
			curByteStart = -1
			curByteEnd = 0
			curDurationTicks = 0
		}

		for _, frag := range fragments {
			// Skip fragments before start offset
			fragTS := trackTimeScales[frag.trackID]
			if fragTS == 0 {
				fragTS = 90000
			}
			if fileIdx == 0 && start > 0 {
				fragStartTick := frag.decodeTime
				// Convert to video timescale for comparison if different track
				if frag.trackID != videoTrackID && fragTS != videoTS {
					fragStartTick = uint64(float64(fragStartTick) / float64(fragTS) * float64(videoTS))
				}
				fragEndTick := fragStartTick + uint64(float64(frag.duration)/float64(fragTS)*float64(videoTS))
				if fragEndTick <= startTick {
					continue
				}
			}

			fragEnd := frag.mdatOffset + frag.mdatSize

			// Start a new segment at a video keyframe, but only if we've
			// accumulated enough duration to meet the target.
			curDurSec := float64(curDurationTicks) / float64(videoTS)
			if frag.trackID == videoTrackID && frag.isSync && curByteStart >= 0 && curDurSec >= targetSegmentDuration {
				flushSegment()
			}

			if curByteStart < 0 {
				curByteStart = frag.moofOffset
			}
			if fragEnd > curByteEnd {
				curByteEnd = fragEnd
			}

			// Accumulate duration from video track fragments only
			if frag.trackID == videoTrackID {
				curDurationTicks += uint64(frag.duration)
			}
		}
		flushSegment()
	}

	if len(segments) == 0 {
		return "", fmt.Errorf("no segments produced")
	}

	// Compute target duration (ceiling of max segment duration)
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

	// Build the m3u8 playlist
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDuration)
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	var lastInitURI string
	for i, seg := range segments {
		init := segmentInits[i]

		// Emit a new EXT-X-MAP when the source file changes
		if init.uri != lastInitURI {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\",BYTERANGE=\"%d@%d\"\n",
				init.uri, init.byteLen, init.byteStart)
			lastInitURI = init.uri
		}

		fmt.Fprintf(&b, "#EXTINF:%.6f,\n", seg.duration)
		fmt.Fprintf(&b, "#EXT-X-BYTERANGE:%d@%d\n", seg.byteLength, seg.byteStart)
		fmt.Fprintf(&b, "%s\n", seg.uri)
	}

	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), nil
}

// findVideoTrack identifies the video track ID from fragments and timescales.
// It picks the track with the highest timescale (video is typically 90000),
// falling back to the track with the most fragments.
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

	// Fallback: track with most fragments
	counts := make(map[uint32]int)
	for _, f := range fragments {
		counts[f.trackID]++
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

// patchMoof modifies mfhd.SequenceNumber and tfdt.BaseMediaDecodeTime in raw moof bytes.
func patchMoof(data []byte, seqNum uint32, baseTime uint64) {
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
			patchTraf(data[i+8:i+boxSize], baseTime)
		}

		i += boxSize
	}
}

func patchTraf(data []byte, baseTime uint64) {
	i := 0
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
