package logging

import (
	"log/slog"
	"strings"
)

// ParseLevel maps a configured level name to a slog.Level. Recognized names are
// "debug", "info", "warn" (or "warning") and "error", case-insensitively, and an
// empty name selects Info.
//
// ok is false for a name that is neither empty nor recognized. A typo must be
// reportable rather than silently resolving to Info: an operator who wrote
// "verbose" expecting debug output would otherwise see a normal-looking log and
// no indication that the setting did nothing.
func ParseLevel(name string) (level slog.Level, ok bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}
