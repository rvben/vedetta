package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter is a size-based rotating log writer. When the active file would
// exceed maxBytes it is renamed to "<path>.1" (shifting older backups up) and a
// fresh file is opened, keeping at most maxBackups rotated files. It is safe for
// concurrent use, so it can back an slog handler directly.
//
// It exists so vedetta's own logs can never grow without bound (the unbounded
// log was a real operational liability). Crash dumps still go to the process's
// real stderr, which the out-of-process supervisor keeps small by killing a
// wedged process promptly.
type RotatingWriter struct {
	path       string
	maxBytes   int64
	maxBackups int

	mu     sync.Mutex
	file   *os.File
	size   int64
	closed bool
}

// NewRotatingWriter opens (or creates) path for appending and rotates it at
// maxBytes, keeping maxBackups rotated files. A non-positive maxBytes disables
// rotation; a negative maxBackups is treated as zero.
//
// The parent directory is created if it does not exist. A log path under a
// directory nobody has made yet is the normal case on a first run, and the
// caller's only recourse for an error here is to fall back to stdout, which
// silently reinstates the unbounded log this type exists to prevent.
func NewRotatingWriter(path string, maxBytes int64, maxBackups int) (*RotatingWriter, error) {
	if maxBackups < 0 {
		maxBackups = 0
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	rw := &RotatingWriter{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := rw.open(); err != nil {
		return nil, err
	}
	return rw, nil
}

// open attaches a file handle for the active path in append mode, adopting the
// existing size. Adopting rather than resetting is what makes an already
// oversized file (a log written before rotation was configured) rotate on the
// first write instead of growing further.
func (rw *RotatingWriter) open() error {
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", rw.path, err)
	}
	size := int64(0)
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	rw.file = f
	rw.size = size
	return nil
}

// Write appends p, rotating first if it would push the active file over the cap.
func (rw *RotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return 0, os.ErrClosed
	}

	// A previous rotation may have failed to open a replacement file. Retrying
	// here, per write, means a transient failure (a directory briefly gone, a
	// full disk) costs only the lines written while it lasted. Giving up at
	// rotation time would end logging for the life of the process.
	if rw.file == nil {
		if err := rw.open(); err != nil {
			return 0, err
		}
	}

	if rw.maxBytes > 0 && rw.size > 0 && rw.size+int64(len(p)) > rw.maxBytes {
		if err := rw.rotate(); err != nil && rw.file == nil {
			// No usable file survived the rotation. Report it instead of
			// writing through a closed descriptor; the next Write reopens.
			return 0, err
		}
	}

	n, err := rw.file.Write(p)
	rw.size += int64(n)
	return n, err
}

// rotate closes the active file, shifts backups up by one (dropping the oldest),
// renames the active file to "<path>.1", and opens a fresh active file.
//
// The active file is closed first, so every path out of here either installs a
// usable replacement or leaves rw.file nil for Write to retry. Returning with
// the old handle still in place would send every subsequent write to a closed
// descriptor, ending logging silently and permanently.
func (rw *RotatingWriter) rotate() error {
	// A failed Close still releases the descriptor, so the rotation proceeds
	// either way; the error is reported, not acted on.
	closeErr := rw.file.Close()
	rw.file = nil
	rw.size = 0

	if rw.maxBackups > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", rw.path, rw.maxBackups)) // drop the oldest
		for i := rw.maxBackups - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", rw.path, i), fmt.Sprintf("%s.%d", rw.path, i+1))
		}
		_ = os.Rename(rw.path, fmt.Sprintf("%s.1", rw.path))
	}

	// With no backups kept, the rename above is skipped and the truncating open
	// below is the rotation: the active file is emptied in place.
	if err := rw.reopenTruncated(); err != nil {
		return errors.Join(closeErr, err)
	}
	return closeErr
}

func (rw *RotatingWriter) reopenTruncated() error {
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("reopen log file %s: %w", rw.path, err)
	}
	rw.file = f
	rw.size = 0
	return nil
}

// Close closes the active log file. Writes after Close report os.ErrClosed
// rather than reopening the file, so a log record emitted during shutdown
// cannot leave a descriptor behind.
func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.closed = true
	if rw.file == nil {
		return nil
	}
	err := rw.file.Close()
	rw.file = nil
	return err
}
