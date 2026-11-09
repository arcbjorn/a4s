package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want the mode of the latest write", info.Mode().Perm())
	}
	assertNoTemporaries(t, filepath.Dir(path))
}

func TestWriteFileAtomicAppliesRestrictiveMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := WriteFileAtomic(path, []byte("material"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteAtomicLeavesDestinationOnWriterFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(path, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("encoder failed")
	err := WriteAtomic(path, 0o600, func(w io.Writer) error {
		if _, err := w.Write([]byte("partial")); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("want the writer's error wrapped, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "intact" {
		t.Fatalf("a failed write replaced the destination with %q", got)
	}
	assertNoTemporaries(t, filepath.Dir(path))
}

func TestWriteAtomicFailsForMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "state.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("writing into a missing directory must fail")
	}
}

func TestWriteFileExclusiveRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := WriteFileExclusive(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFileExclusive(path, []byte("replacement"), 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("want fs.ErrExist, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("an exclusive write replaced existing content with %q", got)
	}
}

func TestWriteFileExclusiveRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// O_EXCL refuses a symlink even when its target does not exist, which is
	// what stops a planted link from redirecting a private key elsewhere.
	if err := WriteFileExclusive(link, []byte("key"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("want fs.ErrExist for a dangling symlink, got %v", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the symlink target must not have been created")
	}
}

func TestWriteFileExclusiveAppliesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := WriteFileExclusive(path, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %v; a private file must not be group or world accessible", info.Mode().Perm())
	}
}

func TestSyncDirReportsMissingDirectory(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("syncing a missing directory must fail")
	}
}

func assertNoTemporaries(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("left a temporary file behind: %s", entry.Name())
		}
	}
}
