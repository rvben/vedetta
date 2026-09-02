package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// configLocks holds one mutex per config file path. Every exported function in
// this package that rewrites a config file holds its path's mutex across the
// whole read, modify and write cycle, so two settings updates arriving at once
// (two browser tabs, or the UI and an automation) apply one after the other
// instead of each writing a document built from a snapshot taken before the
// other's change. Keying by path means edits to different files never wait on
// each other. The unexported helpers below assume the caller holds the lock.
var (
	configLocksMu sync.Mutex
	configLocks   = make(map[string]*sync.Mutex)
)

// lockConfig locks the mutex guarding path and returns its unlock func, for use
// as "defer lockConfig(path)()".
func lockConfig(path string) func() {
	key := configLockKey(path)

	configLocksMu.Lock()
	mu, ok := configLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		configLocks[key] = mu
	}
	configLocksMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// configLockKey resolves path to the absolute, cleaned form so that a relative
// and an absolute reference to the same file share one mutex.
func configLockKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// writeConfigFile replaces path with data by writing a temporary file in the
// same directory and renaming it over the target. The rename is atomic, so a
// reader either sees the previous config or the new one, never the empty or
// half-written file that a truncating write exposes; a crash or power loss
// mid-write leaves the previous config in place. The file and its directory are
// synced so the replacement survives a power loss rather than merely a process
// crash.
func writeConfigFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("creating temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// CreateTemp makes the file 0600; match the target's mode explicitly so the
	// replacement carries the permissions the operator set.
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("setting config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing config: %w", err)
	}
	committed = true

	syncDir(dir)
	return nil
}

// syncDir flushes the directory entry so the rename itself is durable. A
// filesystem that does not support syncing a directory reports an error here,
// which costs durability of the rename but not correctness of the file, so it is
// not surfaced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// configFileMode returns the permissions to give the rewritten file, preserving
// whatever the existing file carries.
func configFileMode(path string) (os.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat config: %w", err)
	}
	return info.Mode().Perm(), nil
}
