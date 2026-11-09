// Package fsutil holds the crash-safe file writes every a4s component that
// persists state relies on.
//
// Durability claims elsewhere in a4s — that desired state survives a restart,
// that a keyset cannot be left half-written, that a backup is either complete
// or absent — reduce to the same three steps: write the bytes, fsync the file,
// and fsync the directory that records its name. Getting any one of them wrong
// is invisible in tests and only shows up as an empty or missing file after a
// power loss, so they live here once rather than being re-derived per package.
//
// The package depends only on the standard library.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// WriteFileAtomic replaces path with data so a reader, or a process restarting
// after a crash, sees either the previous content or the new content in full,
// never a truncated mixture.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return WriteAtomic(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteAtomic is WriteFileAtomic for content produced incrementally. write
// receives the temporary file; if it returns an error the destination is left
// untouched and the temporary file is removed.
//
// The temporary file is created beside the destination, because rename is only
// atomic within one filesystem.
func WriteAtomic(path string, perm fs.FileMode, write func(io.Writer) error) (err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	name := temporary.Name()
	defer func() {
		if err != nil {
			_ = temporary.Close()
			_ = os.Remove(name)
		}
	}()

	// Permissions are set before any content is written, so material meant to
	// be private is never briefly readable under CreateTemp's default mode.
	if err := temporary.Chmod(perm); err != nil {
		return fmt.Errorf("set permissions on %s: %w", path, err)
	}
	if err := write(temporary); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return SyncDir(directory)
}

// WriteFileExclusive creates path with data, refusing to replace a file that
// already exists.
//
// O_EXCL makes the existence check and the create a single step, so two
// concurrent writers cannot both believe they created the file. It is the right
// call for material whose loss is unrecoverable, such as a private key: an
// accidental re-run must fail rather than destroy an identity something else
// still trusts.
func WriteFileExclusive(path string, data []byte, perm fs.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir makes a create, rename, or removal inside directory durable.
//
// fsync on a file persists its content but not the directory entry that names
// it; without this a crash shortly after a rename can leave the old name in
// place or the new one missing.
func SyncDir(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", directory, err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil && !unsupportedSync(err) {
		return fmt.Errorf("sync directory %s: %w", directory, err)
	}
	return nil
}

// unsupportedSync reports a filesystem that cannot fsync a directory at all.
// Some network and FUSE filesystems refuse it; there is nothing further to
// make durable there, and failing every write would make them unusable.
func unsupportedSync(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}
