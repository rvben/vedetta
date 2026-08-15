package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/rvben/vedetta/internal/media"
)

// runTranscode implements the hidden `vedetta transcode -w <width> -h <height>
// -output <staging-path> <source-path>` subcommand. The recompressor invokes it
// as a short-lived child process: the OpenH264 encode path can corrupt the heap
// on certain inputs, and isolating it in a throwaway process means such a crash
// kills only this child and fails a single clip, instead of taking down the
// long-running NVR.
//
// It transcodes <path> into a disposable staging output and writes the
// marker-prefixed JSON TranscodeResult to stdout. The parent validates and
// atomically commits that output only after this worker exits successfully.
func runTranscode(args []string) {
	fs := flag.NewFlagSet("transcode", flag.ExitOnError)
	width := fs.Int("w", 0, "target width")
	height := fs.Int("h", 0, "target height")
	output := fs.String("output", "", "staging output path")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	rest := fs.Args()
	if len(rest) != 1 || *width <= 0 || *height <= 0 || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: vedetta transcode -w <width> -h <height> -output <staging-path> <source-path>")
		os.Exit(2)
	}

	res, err := media.TranscodeSegmentTo(rest[0], *output, *width, *height)
	if err != nil {
		fmt.Fprintf(os.Stderr, "transcode failed: %v\n", err)
		os.Exit(1)
	}

	payload, err := json.Marshal(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode result: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "%s%s\n", media.TranscodeResultMarker, payload)
}
