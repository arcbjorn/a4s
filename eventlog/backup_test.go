package eventlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func loggedStore(t *testing.T, events int) (*File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.log")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	for index := range events {
		if err := store.Append(control.Event{
			Sequence: store.NextSequence(),
			At:       time.Now().UTC(),
			Type:     "allocation.running",
			Actor:    "test",
			Message:  "event",
			Target:   "web-" + string(rune('a'+index)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return store, path
}

func TestBackupRoundTrips(t *testing.T) {
	store, _ := loggedStore(t, 3)
	archive := filepath.Join(t.TempDir(), "backup", "events.log")

	manifest, err := store.Backup(archive)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if manifest.Records != 3 {
		t.Fatalf("manifest records = %d, want 3", manifest.Records)
	}
	if manifest.HeadSequence != 3 || manifest.HeadHash == "" {
		t.Fatalf("manifest head = %d/%q", manifest.HeadSequence, manifest.HeadHash)
	}

	if _, err := VerifyBackup(archive); err != nil {
		t.Fatalf("verify: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored.log")
	if _, err := Restore(archive, restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	reopened, err := Open(restored)
	if err != nil {
		t.Fatalf("open restored log: %v", err)
	}
	defer reopened.Close()
	if len(reopened.Records()) != 3 {
		t.Fatalf("restored records = %d, want 3", len(reopened.Records()))
	}
}

// A backup whose bytes were altered must be refused. Without this the archive
// would be trusted purely because a manifest exists beside it.
func TestVerifyBackupRefusesCorruptedArchive(t *testing.T) {
	store, _ := loggedStore(t, 2)
	archive := filepath.Join(t.TempDir(), "events.log")
	if _, err := store.Backup(archive); err != nil {
		t.Fatal(err)
	}

	payload, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := strings.Replace(string(payload), "event", "evil!", 1)
	if err := os.WriteFile(archive, []byte(corrupted), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := VerifyBackup(archive); err == nil {
		t.Fatal("expected a corrupted archive to be refused")
	}
}

// Truncation is the failure the hash chain alone cannot detect, which is why
// the manifest anchors the head hash outside the file.
func TestVerifyBackupDetectsTruncation(t *testing.T) {
	store, _ := loggedStore(t, 4)
	archive := filepath.Join(t.TempDir(), "events.log")
	if _, err := store.Backup(archive); err != nil {
		t.Fatal(err)
	}

	payload, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(payload), "\n")
	// Drop the final record. Every remaining record still chains correctly, so
	// only the recorded head can reveal the loss.
	truncated := strings.Join(lines[:len(lines)-2], "")
	if err := os.WriteFile(archive, []byte(truncated), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = VerifyBackup(archive)
	if err == nil {
		t.Fatal("expected truncation to be detected")
	}
	if !strings.Contains(err.Error(), "truncated") &&
		!strings.Contains(err.Error(), "bytes") &&
		!strings.Contains(err.Error(), "checksum") {
		t.Fatalf("unexpected truncation error: %v", err)
	}
}

// A restore must refuse before touching the destination, or a corrupt archive
// would destroy the history it was supposed to recover.
func TestRestoreRefusesCorruptArchiveWithoutTouchingDestination(t *testing.T) {
	store, _ := loggedStore(t, 2)
	archive := filepath.Join(t.TempDir(), "events.log")
	if _, err := store.Backup(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("not a log\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "live.log")
	live, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Append(control.Event{
		Sequence: live.NextSequence(), At: time.Now().UTC(),
		Type: "goal.accepted", Actor: "operator", Message: "keep me",
	}); err != nil {
		t.Fatal(err)
	}
	live.Close()

	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(archive, destination); err == nil {
		t.Fatal("expected a corrupt archive to be refused")
	}
	after, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused restore modified the destination log")
	}
}

// Restoring over an existing log must preserve the old one rather than
// silently discarding authoritative history.
func TestRestorePreservesSupersededLog(t *testing.T) {
	store, _ := loggedStore(t, 2)
	archive := filepath.Join(t.TempDir(), "events.log")
	if _, err := store.Backup(archive); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	destination := filepath.Join(directory, "live.log")
	live, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Append(control.Event{
		Sequence: live.NextSequence(), At: time.Now().UTC(),
		Type: "goal.accepted", Actor: "operator", Message: "prior history",
	}); err != nil {
		t.Fatal(err)
	}
	live.Close()

	if _, err := Restore(archive, destination); err != nil {
		t.Fatalf("restore: %v", err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var preserved bool
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "superseded") {
			preserved = true
		}
	}
	if !preserved {
		t.Fatal("the superseded log was discarded rather than preserved")
	}
}

func TestVerifyBackupRequiresManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stray.log")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(path); err == nil {
		t.Fatal("expected an archive without a manifest to be refused")
	}
}

// A backup of an empty log is legitimate and must round trip, because a fresh
// controller is a valid state to recover to.
func TestBackupOfEmptyLog(t *testing.T) {
	store, _ := loggedStore(t, 0)
	archive := filepath.Join(t.TempDir(), "events.log")
	manifest, err := store.Backup(archive)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if manifest.Records != 0 {
		t.Fatalf("records = %d, want 0", manifest.Records)
	}
	if _, err := VerifyBackup(archive); err != nil {
		t.Fatalf("verify empty backup: %v", err)
	}
}

func TestBackupFilePermissionsAreRestrictive(t *testing.T) {
	store, _ := loggedStore(t, 1)
	archive := filepath.Join(t.TempDir(), "events.log")
	if _, err := store.Backup(archive); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("backup mode = %o, want 600", mode)
	}
}
