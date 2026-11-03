// cryreplay evaluates local PCM with Vedetta's production scorer. No network,
// camera connections, event writes or notifications are created.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/rvben/vedetta/internal/cry"
)

func main() {
	manifest := flag.String("manifest", "", "Pinned Vedetta model manifest")
	digest := flag.String("sha256", "", "Manifest SHA-256")
	flag.Parse()
	if err := run(*manifest, *digest, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(manifest, digest string, input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, 8000*3600*2+1))
	if err != nil {
		return err
	}
	if len(raw) < 32000 || len(raw) > 8000*3600*2 || len(raw)%2 != 0 {
		return fmt.Errorf("input must be PCM16 little-endian, mono 8 kHz, 2 seconds to 1 hour")
	}
	model, err := cry.Load(manifest, digest)
	if err != nil {
		return err
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	windows, err := cry.ReplayPCM(model, pcm)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"windows": windows, "model_manifest_sha256": digest, "sample_rate": 8000, "window_seconds": 2, "hop_seconds": .48})
}
