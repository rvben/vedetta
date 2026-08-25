package logging

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriterRotatesAndCapsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 100-byte cap, keep 2 backups.
	rw, err := NewRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	line := []byte(strings.Repeat("x", 60) + "\n") // 61 bytes; two lines exceed 100
	for i := 0; i < 6; i++ {
		if _, err := rw.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// The active file and exactly two rotated backups must exist; the third
	// must have been pruned by the maxBackups cap.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active log missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("backup .1 missing: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("backup .2 missing: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("backup .3 must not exist with maxBackups=2 (err=%v)", err)
	}
}

func TestRotatingWriterKeepsActiveFileUnderCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rw, err := NewRotatingWriter(path, 100, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	for i := 0; i < 50; i++ {
		if _, err := rw.Write([]byte(strings.Repeat("y", 40) + "\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	// A single write (41 bytes) never exceeds the 100-byte cap, so the active
	// file must always stay below the cap plus one write.
	if info.Size() > 100 {
		t.Fatalf("active log grew past cap: %d bytes", info.Size())
	}
}

// Rotation must move lines, not drop them. The size assertions above are
// satisfied by a writer that discards everything, so this is what proves the
// cap is enforced by rotating rather than by losing log lines.
func TestRotatingWriterPreservesEveryLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	const lines = 40
	rw, err := NewRotatingWriter(path, 100, lines)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	for i := 0; i < lines; i++ {
		if _, err := fmt.Fprintf(rw, "line-%02d-%s\n", i, strings.Repeat("z", 50)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	all := readActiveAndBackups(t, path, lines)
	for i := 0; i < lines; i++ {
		if marker := fmt.Sprintf("line-%02d-", i); !strings.Contains(all, marker) {
			t.Errorf("line %d lost across rotation (marker %q not found)", i, marker)
		}
	}
}

// The case that actually fired in production: rotation was configured on a
// service whose log had already grown to 419 MB, so the writer adopted a file
// that was over the cap before its first write. Adopting the existing size is
// what makes that file rotate immediately instead of growing further.
func TestRotatingWriterRotatesAnAlreadyOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	legacy := []byte(strings.Repeat("legacy\n", 100)) // 700 bytes, far over the cap
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatalf("seed legacy log: %v", err)
	}

	rw, err := NewRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	if _, err := rw.Write([]byte("fresh\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The oversized file becomes the first backup, intact...
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(backup, legacy) {
		t.Errorf("backup is not the legacy log verbatim: %d bytes, want %d", len(backup), len(legacy))
	}

	// ...and the active file restarts from the new line alone, under the cap.
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if string(active) != "fresh\n" {
		t.Errorf("active log = %q, want %q", active, "fresh\n")
	}
}

// A log path under a directory that does not exist yet is the normal first-run
// case. Failing here sends the caller back to stdout, which reinstates the
// unbounded log this type exists to prevent.
func TestNewRotatingWriterCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Library", "Logs", "vedetta", "app.log")

	rw, err := NewRotatingWriter(path, 1024, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter on a missing directory: %v", err)
	}
	defer rw.Close()

	if _, err := rw.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("log = %q, want %q", got, "hello\n")
	}
}

// Writes are retried against a reopened file when one is missing, so a Write
// arriving after Close must be refused explicitly. Silently reopening would
// leak a descriptor for a log record emitted during shutdown.
func TestRotatingWriterRefusesWritesAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rw, err := NewRotatingWriter(path, 1024, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	if _, err := rw.Write([]byte("before\n")); err != nil {
		t.Fatalf("write before close: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := rw.Write([]byte("after\n")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Write after Close error = %v, want os.ErrClosed", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != "before\n" {
		t.Errorf("log = %q, want the post-Close write to have been dropped", got)
	}
	// Close is idempotent: main.go defers it, and a second call must not report
	// a spurious failure at shutdown.
	if err := rw.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A non-positive cap disables rotation. This is the configuration a container
// deployment uses when the runtime owns log retention, so it must not rotate
// behind the operator's back.
func TestRotatingWriterWithoutCapNeverRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rw, err := NewRotatingWriter(path, 0, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	for i := 0; i < 20; i++ {
		if _, err := rw.Write([]byte(strings.Repeat("q", 99) + "\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if info.Size() != 2000 {
		t.Errorf("active log = %d bytes, want all 2000 in one file", info.Size())
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotation happened with no cap configured (err=%v)", err)
	}
}

// With no backups kept, rotation truncates in place: the cap is still enforced,
// and no ".1" is left behind to double the on-disk footprint.
func TestRotatingWriterWithoutBackupsTruncatesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rw, err := NewRotatingWriter(path, 100, 0)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	for i := 0; i < 6; i++ {
		if _, err := fmt.Fprintf(rw, "line-%d-%s\n", i, strings.Repeat("w", 60)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("backup created with maxBackups=0 (err=%v)", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if info.Size() > 100 {
		t.Errorf("active log = %d bytes, want it held under the 100-byte cap", info.Size())
	}
	// The positive control: truncating must not stop the writer, so the most
	// recent line has to be there.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if !strings.Contains(string(got), "line-5-") {
		t.Errorf("latest line missing after truncating rotation: %q", got)
	}
}

// readActiveAndBackups concatenates the active log and every backup up to n.
func readActiveAndBackups(t *testing.T, path string, n int) string {
	t.Helper()
	var b strings.Builder
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	b.Write(data)
	for i := 1; i <= n; i++ {
		data, err := os.ReadFile(fmt.Sprintf("%s.%d", path, i))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read backup .%d: %v", i, err)
		}
		b.Write(data)
	}
	return b.String()
}
