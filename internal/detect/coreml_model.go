package detect

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rvben/vedetta/internal/artifact"
)

const (
	coreMLBackendName = "coreml"

	// The default Core ML model is tools/coreml/export.py's conversion of the
	// default ONNX model (modelSHA256).
	coreMLArchiveName   = "yolov8n-coreml.zip"
	coreMLArchiveURL    = "https://github.com/rvben/vedetta/releases/download/v0.0.1-models/" + coreMLArchiveName
	coreMLArchiveSHA256 = "289e1a47a760ebf1f5fdd1fa29b1381601e3e81097945bbec25204b20b92e4ed"

	coreMLPackageExt  = ".mlpackage"
	coreMLCompiledExt = ".mlmodelc"

	// Names inside a cache entry.
	coreMLEntryModel = "model" + coreMLCompiledExt
	coreMLEntryID    = "id"

	// maxCoreMLArchiveEntries bounds how many files an archive may unpack. A
	// model package holds three.
	maxCoreMLArchiveEntries = 64
)

// errCoreMLNotCompiledIn reports a Core ML backend that this binary cannot
// provide at all, as opposed to one that is present but failed to start.
var errCoreMLNotCompiledIn = errors.New("core ML not available: it needs macOS and a cgo build")

// coreMLModels serializes compilation, so detectors starting together compile
// a model once and the rest find it cached.
var coreMLModels sync.Mutex

// openCoreMLBackend loads the Core ML model for modelPath, compiling it first
// when no compiled copy is cached.
func openCoreMLBackend(modelPath string) (Backend, error) {
	if !coreMLBackendCompiledIn {
		return nil, fmt.Errorf("core ML backend: %w", errCoreMLNotCompiledIn)
	}

	if modelPath == "" && !coreMLDefaultModelSupported() {
		return nil, errors.New("core ML backend: the default model needs macOS 15 or later; set detect.model_path to a model exported for this system")
	}

	cacheDir := filepath.Join(modelCacheDir(), "coreml")
	compiled, err := prepareCoreMLModel(modelPath, cacheDir, compileCoreMLModel, "")
	if err != nil {
		return nil, fmt.Errorf("core ML backend: %w", err)
	}
	// Read before the load, so that it names the copy the load was given and
	// not one published while it ran.
	loaded := coreMLCacheID(compiled)
	backend, err := NewCoreMLBackend(compiled)
	if err == nil {
		return backend, nil
	}
	if strings.HasSuffix(modelPath, coreMLCompiledExt) {
		return nil, fmt.Errorf("core ML backend: %w", err)
	}

	// A compiled model is specific to the system that compiled it, so a cached
	// one can stop loading after an upgrade. Compiling again from the package
	// tells that apart from a model that is wrong.
	slog.Warn("cached Core ML model failed to load, compiling it again", "path", compiled, "error", err)
	compiled, err = prepareCoreMLModel(modelPath, cacheDir, compileCoreMLModel, loaded)
	if err != nil {
		return nil, fmt.Errorf("core ML backend: %w", err)
	}
	backend, err = NewCoreMLBackend(compiled)
	if err != nil {
		return nil, fmt.Errorf("core ML backend: %w", err)
	}
	return backend, nil
}

// prepareCoreMLModel returns the path of the compiled model for modelPath.
//
// modelPath names a compiled model (.mlmodelc), which is used as it is, or a
// model package (.mlpackage), or is empty for the default model, which is
// downloaded. A package is compiled into cacheDir under a name derived from its
// content, so a changed package never meets a stale compiled copy.
//
// failed, when not empty, is the coreMLCacheID of a cached copy that did not
// load. That copy is discarded and compiled again; a copy with another id was
// published since by another detector or another process, and is used as it is.
func prepareCoreMLModel(modelPath, cacheDir string, compile func(src, dst string) error, failed string) (string, error) {
	switch {
	case modelPath == "":
	case strings.HasSuffix(modelPath, coreMLCompiledExt):
		if info, err := os.Stat(modelPath); err != nil {
			return "", fmt.Errorf("compiled model: %w", err)
		} else if !info.IsDir() {
			return "", fmt.Errorf("compiled model %s is not a directory", modelPath)
		}
		return modelPath, nil
	case strings.HasSuffix(modelPath, coreMLPackageExt):
		// A package is often reached through a link that names the current
		// version, and the walk that hashes it does not follow one.
		resolved, err := filepath.EvalSymlinks(modelPath)
		if err != nil {
			return "", fmt.Errorf("model package: %w", err)
		}
		modelPath = resolved
	default:
		return "", fmt.Errorf("model_path %q is not a Core ML model: the coreml backend takes a %s or %s directory, or no model_path for the default model (see tools/coreml)",
			modelPath, coreMLPackageExt, coreMLCompiledExt)
	}

	coreMLModels.Lock()
	defer coreMLModels.Unlock()

	// The default model's key comes from the checksum it is pinned to, which
	// needs no download when the compiled copy is already cached.
	key := coreMLArchiveSHA256
	if modelPath != "" {
		sum, err := hashDir(modelPath)
		if err != nil {
			return "", fmt.Errorf("read model package: %w", err)
		}
		key = sum
	}
	// A cache entry is a directory holding the compiled model and the id of
	// this compilation, published together by one rename.
	entry := filepath.Join(cacheDir, key[:16])
	compiled := filepath.Join(entry, coreMLEntryModel)

	if _, err := os.Stat(compiled); err == nil {
		if failed == "" || coreMLCacheID(compiled) != failed {
			return compiled, nil
		}
		if err := os.RemoveAll(entry); err != nil {
			return "", fmt.Errorf("discard cached model: %w", err)
		}
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	// Unpacking and compiling happen in a scratch directory beside the final
	// path, so the rename that publishes the result is atomic and a failure
	// leaves nothing half-written behind.
	work, err := os.MkdirTemp(cacheDir, "build-")
	if err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	pkg := modelPath
	if pkg == "" {
		pkg, err = fetchDefaultCoreMLPackage(filepath.Dir(cacheDir), work)
		if err != nil {
			return "", err
		}
	}

	staged := filepath.Join(work, "entry")
	if err := os.Mkdir(staged, 0o755); err != nil {
		return "", fmt.Errorf("create cache entry: %w", err)
	}
	slog.Info("compiling Core ML model", "package", pkg)
	if err := compile(pkg, filepath.Join(staged, coreMLEntryModel)); err != nil {
		return "", fmt.Errorf("compile %s: %w", pkg, err)
	}
	if err := os.WriteFile(filepath.Join(staged, coreMLEntryID), []byte(rand.Text()), 0o644); err != nil {
		return "", fmt.Errorf("write cache entry id: %w", err)
	}
	if err := os.Rename(staged, entry); err != nil {
		// Another process sharing this cache can publish the same model
		// between the check above and this rename. Its copy came from the same
		// content, so it is the one to use.
		if _, statErr := os.Stat(compiled); statErr == nil {
			return compiled, nil
		}
		return "", fmt.Errorf("store compiled model: %w", err)
	}
	slog.Info("Core ML model compiled", "path", compiled)
	return compiled, nil
}

// coreMLCacheID returns the id of the compilation a cached compiled model came
// from, or "" when the path is not a readable cache entry. A rebuilt entry has
// the same path as the one it replaced and a different id, which is what tells
// the two apart.
func coreMLCacheID(compiled string) string {
	id, err := os.ReadFile(filepath.Join(filepath.Dir(compiled), coreMLEntryID))
	if err != nil {
		return ""
	}
	return string(id)
}

// fetchDefaultCoreMLPackage makes the default model archive available in
// archiveDir, downloading it when no verified copy is there, unpacks it into
// workDir, and returns the path of the model package inside.
func fetchDefaultCoreMLPackage(archiveDir, workDir string) (string, error) {
	archive, err := ensureCachedArtifact(artifact.Spec{
		Name:     coreMLArchiveName,
		URL:      coreMLArchiveURL,
		SHA256:   coreMLArchiveSHA256,
		MaxBytes: maxModelBytes,
	}, filepath.Join(archiveDir, coreMLArchiveName))
	if err != nil {
		return "", fmt.Errorf("default Core ML model: %w", err)
	}

	dest := filepath.Join(workDir, "package")
	if err := unzipInto(archive, dest, maxArchiveBytes); err != nil {
		return "", fmt.Errorf("unpack %s: %w", coreMLArchiveName, err)
	}
	packages, err := filepath.Glob(filepath.Join(dest, "*"+coreMLPackageExt))
	if err != nil {
		return "", err
	}
	if len(packages) != 1 {
		return "", fmt.Errorf("%s holds %d model packages, want 1", coreMLArchiveName, len(packages))
	}
	return packages[0], nil
}

// unzipInto extracts every file of the archive at zipPath below dest, which it
// creates. It refuses an entry that would land outside dest, an entry that is
// not a regular file or directory, and an archive that unpacks to more than
// maxBytes.
func unzipInto(zipPath, dest string, maxBytes int64) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	if len(zr.File) > maxCoreMLArchiveEntries {
		return fmt.Errorf("archive has %d entries, at most %d allowed", len(zr.File), maxCoreMLArchiveEntries)
	}

	remaining := maxBytes
	for _, f := range zr.File {
		if !filepath.IsLocal(f.Name) {
			return fmt.Errorf("entry %q escapes the archive", f.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("entry %q is not a regular file", f.Name)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		written, err := extractZipFile(f, target, remaining)
		if err != nil {
			return fmt.Errorf("entry %q: %w", f.Name, err)
		}
		remaining -= written
	}
	return nil
}

// extractZipFile writes one archive entry to target and returns its size. It
// fails once the entry exceeds limit bytes.
func extractZipFile(f *zip.File, target string, limit int64) (int64, error) {
	src, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	if written > limit {
		return 0, errors.New("archive unpacks to more than the allowed size")
	}
	return written, nil
}

// hashDir returns a SHA-256 over the relative path and content of every file
// below dir, so it changes when any file is added, removed, renamed or edited.
func hashDir(dir string) (string, error) {
	h := sha256.New()
	// WalkDir visits entries in lexical order, which fixes the hash for a
	// given tree.
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// The size ahead of the content keeps a file boundary from moving
		// without the hash changing.
		if _, err := fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), info.Size()); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, file)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
