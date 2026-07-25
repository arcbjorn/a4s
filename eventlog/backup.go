package eventlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BackupManifest describes one controller state backup.
//
// The manifest travels beside the archive rather than inside it, so a restore
// can check what it is about to install before reading the payload. The head
// hash is the important field: the event log is hash-chained, so recording the
// final hash anchors the whole history outside the file and closes the
// truncation gap the chain alone cannot detect.
type BackupManifest struct {
	Version int `json:"version"`
	// TakenAt is when the backup was made.
	TakenAt time.Time `json:"taken_at"`
	// Records is how many records the backup holds.
	Records int `json:"records"`
	// HeadSequence and HeadHash anchor the tip of the chain. A restored log
	// whose tip differs has been truncated or replaced.
	HeadSequence uint64 `json:"head_sequence"`
	HeadHash     string `json:"head_hash,omitempty"`
	// Checksum is the SHA-256 of the archived database, so corruption in
	// transit or at rest is detected before the file is trusted.
	Checksum string `json:"checksum"`
	// Bytes is the archived size, a cheap first check before hashing.
	Bytes int64 `json:"bytes"`
	// Format names how the archive was written, so a future change to the
	// backup mechanism can be recognized rather than misread.
	Format string `json:"format"`
}

// BackupVersion is the manifest format version.
const BackupVersion = 2

// backupFormat identifies a SQLite database produced by VACUUM INTO.
const backupFormat = "sqlite"

const manifestSuffix = ".manifest.json"

// Backup writes a verified copy of the event log to destination.
//
// The copy is made with VACUUM INTO, which produces a transactionally
// consistent database without stopping writers. Copying the file directly
// would race with WAL checkpointing and could capture a torn state that looks
// intact; a backup that is trusted and unusable is worse than none.
//
// The chain is verified before anything is written, so a backup can never
// capture a corrupt log and present it as a recovery point.
func (f *File) Backup(destination string) (BackupManifest, error) {
	if !filepath.IsAbs(destination) {
		return BackupManifest{}, fmt.Errorf("backup path must be absolute")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return BackupManifest{}, fmt.Errorf("event log is closed")
	}

	records, err := f.readRecords()
	if err != nil {
		return BackupManifest{}, err
	}
	if err := verifyChain(records); err != nil {
		return BackupManifest{}, fmt.Errorf("refusing to back up a corrupt log: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup directory: %w", err)
	}
	// VACUUM INTO refuses to overwrite, so a stale archive is removed first.
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return BackupManifest{}, fmt.Errorf("clear previous backup: %w", err)
	}
	// The destination cannot be a bound parameter here, so it is quoted.
	if _, err := f.db.Exec("VACUUM INTO " + quoteLiteral(destination)); err != nil {
		return BackupManifest{}, fmt.Errorf("write backup: %w", err)
	}
	// The log is 0600 because it is authoritative control-plane history; a copy
	// of it deserves exactly the same protection.
	if err := os.Chmod(destination, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("secure backup permissions: %w", err)
	}

	payload, err := os.ReadFile(destination)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup: %w", err)
	}
	manifest := BackupManifest{
		Version:  BackupVersion,
		Format:   backupFormat,
		TakenAt:  time.Now().UTC(),
		Records:  len(records),
		Checksum: hashBytes(payload),
		Bytes:    int64(len(payload)),
	}
	if len(records) > 0 {
		head := records[len(records)-1]
		manifest.HeadSequence = head.Sequence
		manifest.HeadHash = head.Hash
	}

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupManifest{}, fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeFileAtomic(destination+manifestSuffix, append(encoded, '\n'), 0o600); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}

// VerifyBackup checks an archive against its manifest without restoring it.
//
// This is what makes a backup more than a guess: an operator can prove a
// recovery point is intact on a schedule, rather than discovering during an
// incident that the only copy was truncated.
func VerifyBackup(archive string) (BackupManifest, error) {
	manifest, err := readManifest(archive)
	if err != nil {
		return BackupManifest{}, err
	}
	payload, err := os.ReadFile(archive)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup: %w", err)
	}
	if int64(len(payload)) != manifest.Bytes {
		return BackupManifest{}, fmt.Errorf(
			"backup is %d bytes, manifest expects %d", len(payload), manifest.Bytes)
	}
	if got := hashBytes(payload); got != manifest.Checksum {
		return BackupManifest{}, fmt.Errorf("backup checksum mismatch: the archive is corrupt")
	}

	// Opening the archive proves it is a usable database, not merely bytes
	// matching a checksum, and re-deriving the chain proves it is a usable
	// history.
	records, err := recordsIn(archive)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("backup is not a readable event log: %w", err)
	}
	if len(records) != manifest.Records {
		return BackupManifest{}, fmt.Errorf(
			"backup holds %d records, manifest expects %d", len(records), manifest.Records)
	}
	if err := verifyChain(records); err != nil {
		return BackupManifest{}, fmt.Errorf("backup chain is broken: %w", err)
	}
	if len(records) > 0 {
		head := records[len(records)-1]
		if head.Hash != manifest.HeadHash || head.Sequence != manifest.HeadSequence {
			// The chain verifies internally but does not end where the manifest
			// says it should, which is what a truncated log looks like.
			return BackupManifest{}, fmt.Errorf(
				"backup head is %d/%s, manifest expects %d/%s: the log was truncated or replaced",
				head.Sequence, head.Hash, manifest.HeadSequence, manifest.HeadHash)
		}
	}
	return manifest, nil
}

// recordsIn opens an archive read-only and returns its records.
//
// Read-only is load-bearing, not a precaution. Opening a database normally lets
// SQLite change the journal mode and checkpoint on close, which rewrites bytes
// in the file. That would make verifying a backup alter it, so a second verify
// of an untouched archive would report a checksum mismatch and an operator
// would be told their only recovery point was corrupt.
func recordsIn(archive string) ([]Record, error) {
	db, err := openReadOnly(archive)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	store := &File{path: archive, db: db}
	if err := store.loadHead(); err != nil {
		return nil, err
	}
	return store.readRecords()
}

// Restore installs a verified backup at path.
//
// Verification happens before the destination is touched, so a corrupt archive
// is refused rather than written over the history it was meant to recover. If
// a log already exists at the destination it is preserved alongside the
// restored copy, because overwriting authoritative history on an operator's
// behalf is not a decision this function should make silently.
func Restore(archive, path string) (BackupManifest, error) {
	if !filepath.IsAbs(path) {
		return BackupManifest{}, fmt.Errorf("restore path must be absolute")
	}
	manifest, err := VerifyBackup(archive)
	if err != nil {
		return BackupManifest{}, err
	}
	payload, err := os.ReadFile(archive)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return BackupManifest{}, fmt.Errorf("create restore directory: %w", err)
	}

	if existing, err := os.Stat(path); err == nil && existing.Size() > 0 {
		aside := fmt.Sprintf("%s.superseded-%d", path, time.Now().UTC().Unix())
		if err := os.Rename(path, aside); err != nil {
			return BackupManifest{}, fmt.Errorf("preserve existing log: %w", err)
		}
		// WAL and shared-memory sidecars belong to the log that was moved
		// aside. Leaving them would let SQLite recover the old log's tail into
		// the restored database.
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Rename(path+suffix, aside+suffix)
		}
	}
	if err := writeFileAtomic(path, payload, 0o600); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}

func readManifest(archive string) (BackupManifest, error) {
	raw, err := os.ReadFile(archive + manifestSuffix)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return BackupManifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if manifest.Version != BackupVersion {
		return BackupManifest{}, fmt.Errorf(
			"unsupported backup version %d: this build writes and reads version %d",
			manifest.Version, BackupVersion)
	}
	return manifest, nil
}

func hashBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes through a temporary file and renames, so a crash
// during a backup leaves either the previous file or the complete new one and
// never a half-written archive presented as a recovery point.
func writeFileAtomic(path string, payload []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".a4s-backup-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set backup permissions: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return fmt.Errorf("write backup: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync backup: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close backup: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("install backup: %w", err)
	}
	// Syncing the directory makes the rename itself durable, so a crash cannot
	// leave the entry unrecorded.
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		handle.Close()
	}
	return nil
}
