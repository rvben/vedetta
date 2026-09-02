package config

import (
	"fmt"
	"strings"
)

// ValidateRecording checks the recording settings the recorder cannot act on if
// they are wrong. Load runs it on every start, and it is exported so an HTTP
// handler can run the same rules on a submitted form and answer 400 with the
// message rather than writing a config file the next start refuses to read.
// Every message names the field as it is spelled in YAML and the value that was
// rejected.
func ValidateRecording(cfg RecordingConfig) error {
	if cfg.SegmentLength <= 0 {
		return fmt.Errorf("recording.segment_length: must be greater than zero (got %s)", cfg.SegmentLength)
	}
	if cfg.PreCapture <= 0 {
		return fmt.Errorf("recording.pre_capture: must be greater than zero (got %s)", cfg.PreCapture)
	}
	if cfg.PostCapture <= 0 {
		return fmt.Errorf("recording.post_capture: must be greater than zero (got %s)", cfg.PostCapture)
	}
	if cfg.MaxEventDuration < 0 {
		return fmt.Errorf("recording.max_event_duration: must not be negative (got %s)", cfg.MaxEventDuration)
	}
	if cfg.RetainDays < 0 {
		return fmt.Errorf("recording.retain_days: must not be negative (got %d)", cfg.RetainDays)
	}
	if cfg.EventRetain < 0 {
		return fmt.Errorf("recording.event_retain_days: must not be negative (got %d)", cfg.EventRetain)
	}
	if s := strings.TrimSpace(cfg.MaxStorage); s != "" {
		// parseByteSize accepts a signed number, so a negative size parses and
		// would then read as a storage cap the recorder is already over.
		size, err := parseByteSize(s)
		if err != nil {
			return fmt.Errorf("recording.max_storage: %q is not a valid size: %w", cfg.MaxStorage, err)
		}
		if size < 0 {
			return fmt.Errorf("recording.max_storage: must not be negative (got %q)", cfg.MaxStorage)
		}
	}
	if s := strings.TrimSpace(cfg.MinDiskFree); s != "" {
		size, err := parseByteSize(s)
		if err != nil {
			return fmt.Errorf("recording.min_disk_free: %q is not a valid size: %w", cfg.MinDiskFree, err)
		}
		if size < 0 {
			return fmt.Errorf("recording.min_disk_free: must not be negative (got %q)", cfg.MinDiskFree)
		}
	}
	return nil
}

// sameCameraName reports whether two camera names identify the same camera.
// The comparison ignores case, which is the rule the rest of the package
// already applies: SanitizeCameraName lowercases every name the setup flow,
// the camera API and ONVIF discovery produce, so "Garage" and "garage" are one
// camera there. A name is also a path component, and on a case-insensitive
// filesystem the two would share a recording directory.
func sameCameraName(a, b string) bool {
	return strings.EqualFold(a, b)
}
