package detect

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/config"
)

// writeTestPackage creates a directory named like a model package holding one
// file with the given content.
func writeTestPackage(t *testing.T, content string) string {
	t.Helper()
	pkg := filepath.Join(t.TempDir(), "model"+coreMLPackageExt)
	if err := os.MkdirAll(filepath.Join(pkg, "Data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "Data", "weights.bin"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return pkg
}

// countingCompiler stands in for the Core ML compiler: it creates dst and
// counts its calls.
type countingCompiler struct {
	calls int
	err   error
}

func (c *countingCompiler) compile(_, dst string) error {
	c.calls++
	if c.err != nil {
		return c.err
	}
	return os.Mkdir(dst, 0o755)
}

func TestPrepareCoreMLModel_CompilesAPackageOnce(t *testing.T) {
	pkg := writeTestPackage(t, "weights")
	cache := t.TempDir()
	compiler := &countingCompiler{}

	first, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	second, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}

	if first != second {
		t.Fatalf("paths differ: %q and %q", first, second)
	}
	if compiler.calls != 1 {
		t.Fatalf("compiled %d times, want 1", compiler.calls)
	}
	if filepath.Dir(first) != cache || !strings.HasSuffix(first, coreMLCompiledExt) {
		t.Fatalf("compiled model at %q, want a %s directory in %q", first, coreMLCompiledExt, cache)
	}
	if info, err := os.Stat(first); err != nil || !info.IsDir() {
		t.Fatalf("compiled model is not a directory: %v", err)
	}
}

// A link to a package is the package: same compiled model, and no failure from
// hashing the link itself.
func TestPrepareCoreMLModel_FollowsALinkToThePackage(t *testing.T) {
	pkg := writeTestPackage(t, "weights")
	link := filepath.Join(t.TempDir(), "current"+coreMLPackageExt)
	if err := os.Symlink(pkg, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	cache := t.TempDir()
	compiler := &countingCompiler{}

	direct, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if err != nil {
		t.Fatalf("prepare package: %v", err)
	}
	linked, err := prepareCoreMLModel(link, cache, compiler.compile, nil)
	if err != nil {
		t.Fatalf("prepare link: %v", err)
	}
	if linked != direct || compiler.calls != 1 {
		t.Fatalf("link gave %q after %d compiles, want %q after 1", linked, compiler.calls, direct)
	}
}

// A package whose content changed must not be served the compiled copy of the
// old content.
func TestPrepareCoreMLModel_ChangedPackageIsCompiledAgain(t *testing.T) {
	pkg := writeTestPackage(t, "weights")
	cache := t.TempDir()
	compiler := &countingCompiler{}

	before, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "Data", "weights.bin"), []byte("retrained"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatalf("changed package reused %q", before)
	}
	if compiler.calls != 2 {
		t.Fatalf("compiled %d times, want 2", compiler.calls)
	}
}

func TestPrepareCoreMLModel_RebuildsTheCopyThatFailedOnce(t *testing.T) {
	pkg := writeTestPackage(t, "weights")
	cache := t.TempDir()
	compiler := &countingCompiler{}

	compiled, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(compiled, "stale")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	failed, err := os.Stat(compiled)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := prepareCoreMLModel(pkg, cache, compiler.compile, failed); err != nil {
		t.Fatal(err)
	}
	if compiler.calls != 2 {
		t.Fatalf("compiled %d times after a failed load, want 2", compiler.calls)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy that failed survived the rebuild: %v", err)
	}

	// A second detector, or another process sharing the cache, that saw the
	// same copy fail must use the rebuilt one rather than delete it under the
	// first.
	fresh := filepath.Join(compiled, "fresh")
	if err := os.WriteFile(fresh, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareCoreMLModel(pkg, cache, compiler.compile, failed); err != nil {
		t.Fatal(err)
	}
	if compiler.calls != 2 {
		t.Fatalf("compiled %d times, want the rebuilt model reused", compiler.calls)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the rebuilt model was replaced: %v", err)
	}
}

// Two processes sharing a cache can compile the same model at once. The one
// that finishes second finds the final path taken and must use what is there.
func TestPrepareCoreMLModel_UsesTheCopyAnotherProcessPublished(t *testing.T) {
	pkg := writeTestPackage(t, "weights")
	cache := t.TempDir()
	key, err := hashDir(pkg)
	if err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(cache, key[:16]+coreMLCompiledExt)

	compile := func(_, dst string) error {
		// The other process publishes while this one is still compiling. A
		// file inside makes the directory one a rename cannot replace.
		if err := os.Mkdir(published, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(published, "model.mil"), []byte("theirs"), 0o644); err != nil {
			return err
		}
		return os.Mkdir(dst, 0o755)
	}

	got, err := prepareCoreMLModel(pkg, cache, compile, nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got != published {
		t.Fatalf("got %q, want the published %q", got, published)
	}
	if _, err := os.Stat(filepath.Join(published, "model.mil")); err != nil {
		t.Fatalf("the published model was disturbed: %v", err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache holds %d entries, want only the published model", len(entries))
	}
}

func TestPrepareCoreMLModel_FailedCompileLeavesTheCacheEmpty(t *testing.T) {
	pkg := writeTestPackage(t, "weights")
	cache := t.TempDir()
	want := errors.New("compiler said no")
	compiler := &countingCompiler{err: want}

	_, err := prepareCoreMLModel(pkg, cache, compiler.compile, nil)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want one wrapping the compiler's", err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache holds %d entries after a failed compile, want 0", len(entries))
	}
}

func TestPrepareCoreMLModel_UsesACompiledModelAsItIs(t *testing.T) {
	compiled := filepath.Join(t.TempDir(), "model"+coreMLCompiledExt)
	if err := os.Mkdir(compiled, 0o755); err != nil {
		t.Fatal(err)
	}
	compiler := &countingCompiler{}

	got, err := prepareCoreMLModel(compiled, t.TempDir(), compiler.compile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != compiled || compiler.calls != 0 {
		t.Fatalf("got %q after %d compiles, want %q after none", got, compiler.calls, compiled)
	}

	missing := filepath.Join(t.TempDir(), "absent"+coreMLCompiledExt)
	if _, err := prepareCoreMLModel(missing, t.TempDir(), compiler.compile, nil); err == nil {
		t.Fatal("a missing compiled model was accepted")
	}
}

// An ONNX model_path left over from another backend must be refused, not
// replaced by the default model behind the operator's back.
func TestPrepareCoreMLModel_RejectsOtherModelFormats(t *testing.T) {
	compiler := &countingCompiler{}
	_, err := prepareCoreMLModel("/models/yolov8n.onnx", t.TempDir(), compiler.compile, nil)
	if err == nil {
		t.Fatal("an ONNX model was accepted")
	}
	if !strings.Contains(err.Error(), "yolov8n.onnx") {
		t.Fatalf("error %q does not name the rejected path", err)
	}
	if compiler.calls != 0 {
		t.Fatalf("compiled %d times, want 0", compiler.calls)
	}
}

func TestDetector_CoreMLWithoutSupportFailsInsteadOfFallingBack(t *testing.T) {
	if coreMLBackendCompiledIn {
		t.Skip("this build carries the Core ML backend")
	}
	d := &Detector{}
	err := d.init(config.DetectConfig{Backend: coreMLBackendName})
	if !errors.Is(err, errCoreMLNotCompiledIn) {
		t.Fatalf("error = %v, want one wrapping errCoreMLNotCompiledIn", err)
	}
	if d.backend != nil {
		t.Fatalf("a %s backend was started in place of Core ML", d.backend.Name())
	}
}

// The default model reaches the compiler through a checksum-verified archive.
// Seeding the archive stands in for the download, which takes the same path
// from there on.
func TestFetchDefaultCoreMLPackage_UnpacksAVerifiedArchive(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("models", coreMLArchiveName))
	if err != nil {
		t.Skipf("models/%s not found, skipping", coreMLArchiveName)
	}
	// An archive that is not the pinned one would be discarded and fetched
	// again, and this test must not reach the network.
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != coreMLArchiveSHA256 {
		t.Skipf("models/%s is not the pinned archive, skipping", coreMLArchiveName)
	}
	archiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(archiveDir, coreMLArchiveName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	pkg, err := fetchDefaultCoreMLPackage(archiveDir, t.TempDir())
	if err != nil {
		t.Fatalf("fetchDefaultCoreMLPackage: %v", err)
	}
	if !strings.HasSuffix(pkg, coreMLPackageExt) {
		t.Fatalf("got %q, want a %s directory", pkg, coreMLPackageExt)
	}
	if _, err := os.Stat(filepath.Join(pkg, "Manifest.json")); err != nil {
		t.Fatalf("unpacked package has no manifest: %v", err)
	}
}

func TestHashDir_TracksNamesAndContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := func() string {
		t.Helper()
		s, err := hashDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	write("a", "one")
	write("b", "two")
	base := sum()
	if again := sum(); again != base {
		t.Fatalf("hash is not stable: %s then %s", base, again)
	}

	write("b", "twO")
	edited := sum()
	if edited == base {
		t.Fatal("an edited file left the hash unchanged")
	}

	write("b", "two")
	if err := os.Rename(filepath.Join(dir, "b"), filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	if sum() == base {
		t.Fatal("a renamed file left the hash unchanged")
	}

	// Moving bytes across a file boundary keeps the concatenated content the
	// same, so only the per-file framing can tell these trees apart.
	if err := os.Remove(filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	write("a", "onet")
	write("b", "wo")
	if sum() == base {
		t.Fatal("moving bytes between files left the hash unchanged")
	}
}

type zipEntry struct {
	name    string
	content string
	mode    os.FileMode
}

func writeTestZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Store}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnzipInto_ExtractsNestedFiles(t *testing.T) {
	archive := writeTestZip(t, []zipEntry{
		{name: "m.mlpackage/Manifest.json", content: "{}"},
		{name: "m.mlpackage/Data/weights.bin", content: "abc"},
	})
	dest := filepath.Join(t.TempDir(), "out")

	if err := unzipInto(archive, dest, 1024); err != nil {
		t.Fatalf("unzipInto: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "m.mlpackage", "Data", "weights.bin"))
	if err != nil || string(got) != "abc" {
		t.Fatalf("extracted %q, %v; want %q", got, err, "abc")
	}
}

func TestUnzipInto_RefusesUnsafeArchives(t *testing.T) {
	tooMany := make([]zipEntry, maxCoreMLArchiveEntries+1)
	for i := range tooMany {
		tooMany[i] = zipEntry{name: "f" + strings.Repeat("x", i)}
	}

	cases := []struct {
		name    string
		entries []zipEntry
		limit   int64
	}{
		{"parent traversal", []zipEntry{{name: "../escaped", content: "x"}}, 1024},
		{"nested traversal", []zipEntry{{name: "a/../../escaped", content: "x"}}, 1024},
		{"absolute path", []zipEntry{{name: "/escaped", content: "x"}}, 1024},
		{"symlink", []zipEntry{{name: "link", content: "/etc/passwd", mode: os.ModeSymlink | 0o777}}, 1024},
		{"one file over the size limit", []zipEntry{{name: "big", content: "12345"}}, 4},
		{"files together over the size limit", []zipEntry{{name: "a", content: "123"}, {name: "b", content: "45"}}, 4},
		{"too many entries", tooMany, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "out")
			if err := unzipInto(writeTestZip(t, tc.entries), dest, tc.limit); err == nil {
				t.Fatal("archive was accepted")
			}
			if _, err := os.Lstat(filepath.Join(root, "escaped")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an entry was written outside the destination: %v", err)
			}
		})
	}
}

// The limit is on the total, so an archive that exactly fills it is fine.
func TestUnzipInto_AcceptsAnArchiveAtTheSizeLimit(t *testing.T) {
	archive := writeTestZip(t, []zipEntry{{name: "a", content: "12"}, {name: "b", content: "34"}})
	if err := unzipInto(archive, filepath.Join(t.TempDir(), "out"), 4); err != nil {
		t.Fatalf("unzipInto: %v", err)
	}
}
