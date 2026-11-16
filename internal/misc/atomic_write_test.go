package misc

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// authDir returns a temp directory and the path of auth.json in it, which holds "old" when existing.
func authDir(t *testing.T, existing bool) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "auth.json")
	if existing {
		if errWrite := os.WriteFile(path, []byte("old"), 0o644); errWrite != nil {
			t.Fatalf("write old file: %v", errWrite)
		}
	}
	return dir, path
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read %s: %v", path, errRead)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), got, want)
	}
}

func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read dir: %v", errRead)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{name}) {
		t.Fatalf("directory holds %v, want only %s", names, name)
	}
}

// stubSyncFile replaces syncFile until the test ends.
func stubSyncFile(t *testing.T, stub func(*os.File) error) {
	t.Helper()
	original := syncFile
	t.Cleanup(func() { syncFile = original })
	syncFile = stub
}

func TestWritePrivateFileAtomicWritesOwnerOnlyFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
	}{
		{"replaces an existing file", true},
		{"creates a missing file", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := authDir(t, tc.existing)

			if errWrite := WritePrivateFileAtomic(path, []byte("new")); errWrite != nil {
				t.Fatalf("WritePrivateFileAtomic() error = %v", errWrite)
			}

			assertContent(t, path, "new")
			assertOnlyFile(t, dir, "auth.json")
			if runtime.GOOS != "windows" {
				info, errStat := os.Stat(path)
				if errStat != nil {
					t.Fatalf("stat: %v", errStat)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("mode = %v, want 0600", info.Mode().Perm())
				}
			}
		})
	}
}

func TestWritePrivateFileAtomicFailsWhenDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "auth.json")

	if errWrite := WritePrivateFileAtomic(path, []byte("new")); errWrite == nil {
		t.Fatal("WritePrivateFileAtomic() error = nil, want one: the directory does not exist")
	}
}

func TestWritePrivateFileAtomicKeepsOldFileWhenSyncFails(t *testing.T) {
	dir, path := authDir(t, true)
	errDisk := errors.New("disk failure")
	stubSyncFile(t, func(*os.File) error { return errDisk })

	errWrite := WritePrivateFileAtomic(path, []byte("new"))

	if !errors.Is(errWrite, errDisk) {
		t.Fatalf("WritePrivateFileAtomic() error = %v, want it to wrap %v", errWrite, errDisk)
	}
	assertContent(t, path, "old")
	assertOnlyFile(t, dir, "auth.json")
}

// The data must reach the disk before the rename can make it the live file, and the rename itself
// must reach the disk before the save counts as done.
func TestWritePrivateFileAtomicSyncsFileBeforeRenameAndDirectoryAfter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot sync directory handles")
	}
	dir, path := authDir(t, true)
	realSync := syncFile
	var steps []string
	stubSyncFile(t, func(f *os.File) error {
		live, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatalf("read live file during sync: %v", errRead)
		}
		kind := "file"
		if f.Name() == dir {
			kind = "directory"
		}
		steps = append(steps, kind+" synced while the live file held "+string(live))
		if strings.HasSuffix(f.Name(), ".json") {
			t.Errorf("scratch file %q ends in .json, so auth scanners would pick it up", f.Name())
		}
		return realSync(f)
	})

	if errWrite := WritePrivateFileAtomic(path, []byte("new")); errWrite != nil {
		t.Fatalf("WritePrivateFileAtomic() error = %v", errWrite)
	}

	want := []string{"file synced while the live file held old", "directory synced while the live file held new"}
	if !slices.Equal(steps, want) {
		t.Fatalf("sync steps = %q, want %q", steps, want)
	}
}
