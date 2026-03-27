# HLS Playback for Recorded Video

## Problem

Vedetta records to fMP4 (fragmented MP4) segments. Browsers cannot play fMP4 natively via `video.src` because fMP4 moov boxes have empty sample tables. The current workaround remuxes fMP4 to progressive MP4 on demand, which takes 5-10 seconds and produces laggy playback.

## Solution

Serve recorded video via HLS (HTTP Live Streaming) using byte-range addressing over existing fMP4 files. No recording pipeline changes needed.

## How It Works

1. Browser requests `/api/cameras/{name}/playback.m3u8?start=<RFC3339>`
2. Server finds the relevant segment(s) in the database
3. Server scans fragment positions in the fMP4 (extending `indexFile()` to capture sync sample flags)
4. Server generates an m3u8 playlist with `#EXT-X-MAP` for the init segment and `#EXT-X-BYTERANGE` entries grouping fragments into chunks aligned to keyframes
5. Browser uses hls.js (or native HLS on Safari) to play the playlist
6. hls.js requests byte ranges from the fMP4 file via a segment-serving endpoint
7. `http.ServeFile` handles Range requests automatically

## API Changes

### New endpoints

- `GET /api/cameras/{name}/playback.m3u8?start=<RFC3339>&duration=<seconds>`
  Returns an HLS playlist. Default duration: 600s (10 min), maximum: 3600s. The playlist references one or more fMP4 segment files via byte-range entries. Returns 404 if no recording exists for the requested time.

- `GET /api/cameras/{name}/segments/{id}`
  Serves a raw fMP4 segment file. Uses `http.ServeFile` for Range request support. The segment ID maps to the database segment record. The handler validates that the segment belongs to the camera in the URL path.

### Removed endpoints

The existing `GET /api/cameras/{name}/playback?start=<RFC3339>` endpoint (progressive MP4 remux) is replaced by the m3u8 endpoint above.

## m3u8 Playlist Format

HLS version 7 is used because it formally supports fMP4 media segments with byte-range addressing.

```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="/api/cameras/front_door/segments/42",BYTERANGE="1156@0"
#EXTINF:3.333,
#EXT-X-BYTERANGE:1245678@1156
/api/cameras/front_door/segments/42
#EXTINF:3.333,
#EXT-X-BYTERANGE:1198234@1246834
/api/cameras/front_door/segments/42
...
#EXT-X-ENDLIST
```

- `#EXT-X-MAP` points to ftyp+moov (the init segment) at the start of the file
- Each `#EXTINF` + `#EXT-X-BYTERANGE` entry covers fragments from one keyframe to the next
- `#EXT-X-PLAYLIST-TYPE:VOD` tells hls.js the playlist is complete

### Multi-segment playlists

When playback spans multiple fMP4 files, a new `#EXT-X-MAP` directive is emitted before the first entry of each new segment file. This is necessary because each fMP4 file has its own moov with potentially different codec parameters.

```
#EXT-X-MAP:URI="/api/cameras/front_door/segments/42",BYTERANGE="1156@0"
#EXTINF:3.333,
...entries for segment 42...
#EXT-X-MAP:URI="/api/cameras/front_door/segments/43",BYTERANGE="1156@0"
#EXTINF:3.333,
...entries for segment 43...
```

## Playlist Generation

### Required changes to indexFile()

The existing `indexFile()` function needs to be extended. The `fragment` struct must gain an `isSync bool` field indicating whether the fragment starts with a keyframe. This is determined from trun sample flags: bit 16 (`sample_is_non_sync_sample`). The handler must also check `FirstSampleFlags` in the trun box and `DefaultSampleFlags` from tfhd.

### Fragment grouping algorithm

1. Scan all fragments using the extended `indexFile()`
2. Identify video fragments where `isSync == true` — these are HLS segment boundaries
3. For each keyframe-to-keyframe interval, compute the byte range spanning from the first moof to the end of the last mdat (both video and audio fragments included)
4. Emit one `#EXTINF` + `#EXT-X-BYTERANGE` entry per interval

### Fragment ordering invariant

The fMP4 writer produces fragments in file order: video moof+mdat followed by audio moof+mdat, alternating. This means fragments within a time window are contiguous byte ranges. Audio/video boundary misalignment (audio frames don't align exactly with video keyframes) is handled by using inclusive byte ranges — hls.js uses timing metadata from moof/trun to determine what to decode, not byte boundaries.

## Frontend Changes

### hls.js integration

Vendor `hls.min.js` (~250KB) in `internal/api/static/`. Add a script tag to `camera.html`. No npm or build step needed.

### Playback function

```javascript
function startPlayback(timestamp) {
  var url = '/api/cameras/' + name + '/playback.m3u8?start=' + isoStr;
  var video = el('live-video');

  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    // Safari: native HLS
    video.src = url;
    video.addEventListener('error', function onErr() {
      video.removeEventListener('error', onErr);
      toast('No recording found for this timestamp', 'error');
      returnToLive();
    });
  } else if (Hls.isSupported()) {
    var hls = new Hls();
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, function() {
      video.play();
    });
    hls.on(Hls.Events.ERROR, function(event, data) {
      if (data.fatal) {
        toast('Playback error', 'error');
        hls.destroy();
        returnToLive();
      }
    });
  }
}
```

### Error handling

The m3u8 endpoint returns 404 if no recording exists. hls.js emits `Hls.Events.ERROR` with `data.fatal = true` on manifest load failure. Safari's native HLS fires the video element's `error` event. Both paths show a toast and return to live view. This replaces the HEAD pre-check.

### What gets removed

- `playNextSegment()` chaining logic (hls.js handles multi-segment playlists)
- `RemuxToProgressive()` usage in playback handler
- `.playback-cache` directory logic
- The HEAD pre-check before playback (replaced by HLS error events)

### What stays

- `playbackMode`, `playbackStartTime` state for timeline sync
- `updatePlayheadForPlayback()` driven by `video.ontimeupdate`
- `returnToLive()` cleanup
- Event clip playback (clips are short progressive MP4s, no change needed)

## Impact on Existing Features

| Feature | Impact |
|---------|--------|
| Event clips | None — clips use `TrimMP4`/`ConcatMP4`, produce progressive MP4 |
| Thumbnails | None — separate code path |
| Retention cleanup | None — deletes segment files and DB records as before |
| Timeline API | None — returns metadata, not video |
| Recording export | None — concatenates segments into downloadable file |
| Live streaming | None — separate MSE/WebRTC/MJPEG code paths |
| Recording pipeline | None — continues writing fMP4 segments as before |

## Migration

1. Deploy m3u8 and segment-serving endpoints alongside the old playback endpoint
2. Switch frontend to HLS playback
3. Remove old `/api/cameras/{name}/playback` endpoint in a follow-up
4. Clean up `.playback-cache` directories (one-time, can be done by retention cleanup)
5. `RemuxToProgressive()` can be removed once the old endpoint is gone (keep `TrimMP4`/`ConcatMP4` for clips)

## Implementation Steps

1. Extend `indexFile()` with sync sample detection in `internal/media/mp4reader.go`
2. Add HLS playlist generator function in `internal/media/`
3. Add segment-serving endpoint (`/api/cameras/{name}/segments/{id}`) with camera ownership validation
4. Add m3u8 endpoint (`/api/cameras/{name}/playback.m3u8`) with duration cap (max 3600s)
5. Vendor hls.js in `internal/api/static/`
6. Update `startPlayback()` in app.js to use HLS with error handling
7. Remove progressive MP4 remux from playback path
8. Clean up `.playback-cache` logic

## Browser Compatibility

- Safari (iOS/macOS): Native HLS support, no library needed
- Chrome/Firefox/Edge: hls.js provides MSE-based HLS playback
- Fallback: if MSE is unavailable (extremely rare), show error message
