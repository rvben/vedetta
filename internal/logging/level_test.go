package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name  string
		want  slog.Level
		wantO bool
	}{
		{"", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},

		// Case and surrounding whitespace are accidents of hand-edited YAML, not
		// a different intent.
		{"DEBUG", slog.LevelDebug, true},
		{"  Warn  ", slog.LevelWarn, true},

		// A name that is neither empty nor recognized must report ok=false. An
		// operator who wrote "verbose" expecting debug output would otherwise get
		// a normal-looking log and no indication the setting did nothing.
		{"verbose", slog.LevelInfo, false},
		{"trace", slog.LevelInfo, false},
		{"5", slog.LevelInfo, false},
	}

	for _, tt := range tests {
		got, ok := ParseLevel(tt.name)
		if got != tt.want || ok != tt.wantO {
			t.Errorf("ParseLevel(%q) = (%v, %v), want (%v, %v)",
				tt.name, got, ok, tt.want, tt.wantO)
		}
	}
}

// The gate holds a slog.Leveler rather than a fixed level so that one
// *slog.LevelVar governs the local handler and the OTLP export arm together. A
// snapshot taken at construction would leave the two disagreeing after any
// later change.
func TestLevelGateFollowsALevelVar(t *testing.T) {
	var v slog.LevelVar
	v.Set(slog.LevelInfo)
	g := newLevelGate(&v, slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if g.Enabled(t.Context(), slog.LevelDebug) {
		t.Error("debug enabled while the var is at info")
	}

	v.Set(slog.LevelDebug)
	if !g.Enabled(t.Context(), slog.LevelDebug) {
		t.Error("debug still gated after the var moved to debug; the gate is not following it")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
