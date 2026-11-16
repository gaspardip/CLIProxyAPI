package misc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// syncFile flushes f to stable storage. Tests replace it to observe the order of operations and to
// make a sync fail.
var syncFile = func(f *os.File) error { return f.Sync() }

// WritePrivateFileAtomic replaces path with data, readable by its owner only (0600), so that a
// crash, a full disk or a concurrent reader finds either the whole old file or the whole new one,
// never a truncated mix of the two.
//
// The data goes to a temporary file beside path, is synced, and is renamed over path. The
// temporary name does not end in ".json", so auth scanners ignore it. The directory is synced last
// so the rename itself survives power loss, and it must already exist.
//
// shortcut: a symlink at path is replaced rather than followed, as in codex.WriteJSONFileAtomic;
// follow it if auth files are ever symlinked.
func WritePrivateFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, errCreate := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if errCreate != nil {
		return fmt.Errorf("create temp file for %s: %w", path, errCreate)
	}
	// After a successful rename the temporary name is gone and this remove does nothing.
	defer func() { _ = os.Remove(tmp.Name()) }()

	_, errWrite := tmp.Write(data)
	if errWrite = errors.Join(errWrite, syncFile(tmp), tmp.Close()); errWrite != nil {
		return fmt.Errorf("write temp file for %s: %w", path, errWrite)
	}
	if errRename := os.Rename(tmp.Name(), path); errRename != nil {
		return fmt.Errorf("replace %s: %w", path, errRename)
	}
	if errSync := syncDir(dir); errSync != nil {
		return fmt.Errorf("sync directory of %s: %w", path, errSync)
	}
	return nil
}

// syncDir flushes a directory entry created by rename. Windows cannot sync directory handles.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, errOpen := os.Open(dir)
	if errOpen != nil {
		return errOpen
	}
	return errors.Join(syncFile(d), d.Close())
}
