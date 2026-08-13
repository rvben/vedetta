package api

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

func TestPWAManifestImagesExistInEmbeddedStaticFiles(t *testing.T) {
	manifestBytes, err := fs.ReadFile(staticFiles, "static/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
		Screenshots []struct {
			Src string `json:"src"`
		} `json:"screenshots"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	images := append(manifest.Icons, manifest.Screenshots...)
	if len(images) == 0 {
		t.Fatal("manifest contains no install images")
	}
	for _, image := range images {
		path := "static/" + strings.TrimPrefix(image.Src, "/")
		if _, err := fs.Stat(staticFiles, path); err != nil {
			t.Errorf("manifest image %q does not resolve to embedded asset %q: %v", image.Src, path, err)
		}
	}
}
