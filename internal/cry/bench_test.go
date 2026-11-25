package cry

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkScore times one two-second window through the real model. It needs
// the model directory in VEDETTA_CRY_MODEL and skips without it.
func BenchmarkScore(b *testing.B) {
	dir := os.Getenv("VEDETTA_CRY_MODEL")
	if dir == "" {
		b.Skip("set VEDETTA_CRY_MODEL to benchmark the cry model")
	}
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	hash := sha256.Sum256(data)
	m, err := Load(path, hex.EncodeToString(hash[:]))
	if err != nil {
		b.Fatal(err)
	}
	pcm := make([]int16, 16000)
	state := uint32(1)
	for i := range pcm {
		state = state*1664525 + 1013904223
		pcm[i] = int16(state>>16) / 8
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Score(pcm); err != nil {
			b.Fatal(err)
		}
	}
}
