package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReplayRejectsPartialAndShortPCMWithoutLoadingAssets(t *testing.T) {
	for _, pcm := range [][]byte{nil, make([]byte, 31999), make([]byte, 32001)} {
		var output bytes.Buffer
		err := run("missing", "invalid", bytes.NewReader(pcm), &output)
		if err == nil || !strings.Contains(err.Error(), "PCM16") || output.Len() != 0 {
			t.Fatalf("accepted invalid audio or leaked output: %v", err)
		}
	}
}
