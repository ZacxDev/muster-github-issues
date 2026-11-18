// Package atomicfile replaces a small state file durably: a crash leaves
// either the old content or the new, never a mix.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces dir/name with b: a temporary file, fsynced, renamed over it,
// then the directory fsynced so the rename survives a crash.
func Write(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory, so a file just created or renamed in it
// survives a crash.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
