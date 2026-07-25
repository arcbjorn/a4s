package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// writeLegacyLog builds a newline-delimited log exactly as the previous
// implementation wrote one, so migration is tested against the real format
// rather than against an idealized version of it.
func writeLegacyLog(t *testing.T, dir string, count int) string {
	t.Helper()
	path := filepath.Join(dir, "events.log")
	var builder strings.Builder
	previous := ""
	for index := 1; index <= count; index++ {
		event := testEvent(uint64(index), fmt.Sprintf("legacy %d", index))
		hash, err := eventHash(previous, event)
		if err != nil {
			t.Fatal(err)
		}
		record := Record{
			Sequence: uint64(index), PreviousHash: previous, Hash: hash, Event: event,
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
		previous = hash
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The headline migration case: an existing deployment upgrades by replacing
// the binary, and its history is intact afterwards.
func TestLegacyLogMigratesOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyLog(t, dir, 4)

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy log: %v", err)
	}
	defer store.Close()

	if store.Migrated() != 4 {
		t.Fatalf("migrated %d records, want 4", store.Migrated())
	}
	records := store.Records()
	if len(records) != 4 {
		t.Fatalf("store holds %d records, want 4", len(records))
	}
	if err := store.Verify(); err != nil {
		t.Fatalf("migrated chain does not verify: %v", err)
	}
	if store.NextSequence() != 5 {
		t.Fatalf("next sequence = %d, want 5", store.NextSequence())
	}
	// The hashes must be the originals, not recomputed ones, or the migrated
	// history would no longer match any backup taken before the upgrade.
	if records[3].Hash == "" || records[3].PreviousHash != records[2].Hash {
		t.Fatalf("migrated chain links are wrong: %+v", records[3])
	}
}

// A rollback to the previous build must still find its log, so the original
// file is preserved rather than consumed.
func TestMigrationPreservesTheLegacyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyLog(t, dir, 2)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the legacy log was removed: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the legacy log was modified during migration")
	}
	if store.Path() == path {
		t.Fatal("the database overwrote the legacy log")
	}
}

// Restarting a migrated deployment must not import the legacy file again.
func TestMigrationIsIdempotentAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyLog(t, dir, 3)

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(testEvent(first.NextSequence(), "after migration")); err != nil {
		t.Fatalf("append after migration: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	if got := len(second.Records()); got != 4 {
		t.Fatalf("reopen produced %d records, want 4: the legacy log was re-imported", got)
	}
	if second.Migrated() != 0 {
		t.Fatalf("reopen reported %d migrated records, want 0", second.Migrated())
	}
	if err := second.Verify(); err != nil {
		t.Fatalf("chain broken after restart: %v", err)
	}
}

// A torn final write is a crash artifact, not tampering. The previous
// implementation recovered from it, and migration must too, or a deployment
// that lost power once could never upgrade.
func TestMigrationRecoversTornFinalWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyLog(t, dir, 3)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Append half a record, as an interrupted fsync would leave.
	torn := string(raw) + `{"sequence":4,"previous_hash":"aa","ha`
	if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("a torn final write made the log unopenable: %v", err)
	}
	defer store.Close()

	if got := len(store.Records()); got != 3 {
		t.Fatalf("recovered %d records, want the 3 complete ones", got)
	}
	if store.Truncated() != 4 {
		t.Fatalf("truncated line = %d, want 4", store.Truncated())
	}
	if err := store.Verify(); err != nil {
		t.Fatalf("recovered chain does not verify: %v", err)
	}
}

// Corruption in the middle of a legacy log cannot be explained by an
// interrupted append. Migrating it would launder a broken history into a new
// store and lose the evidence that it was broken.
func TestMigrationRefusesMidFileCorruption(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyLog(t, dir, 4)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	lines[1] = strings.Replace(lines[1], "legacy 2", "tampered", 1)
	if err := os.WriteFile(path,
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("a tampered legacy log migrated cleanly")
	} else if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An undecodable line that is not last was not caused by a crash.
func TestMigrationRefusesMidFileGarbage(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyLog(t, dir, 3)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	lines[1] = "{not json"
	if err := os.WriteFile(path,
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("a legacy log with mid-file garbage migrated cleanly")
	}
}

// A SQLite database must not be mistaken for a legacy log, whatever it is
// named. An operator's file naming is not a reliable type signal.
func TestDatabaseIsNotTreatedAsLegacy(t *testing.T) {
	dir := t.TempDir()
	// A database deliberately named like a jsonl log.
	path := filepath.Join(dir, "events.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(testEvent(1, "native")); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Migrated() != 0 {
		t.Fatal("a SQLite database was treated as a legacy log")
	}
	if len(reopened.Records()) != 1 {
		t.Fatalf("records = %d, want 1", len(reopened.Records()))
	}
}

// A file that is neither must be refused rather than silently replaced.
func TestUnrecognizedFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	if err := os.WriteFile(path, []byte("this is not an event log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("an unrecognized file was accepted as an event log")
	}
}

// An empty legacy file is a fresh start, not a migration.
func TestEmptyLegacyFileOpensCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open empty legacy file: %v", err)
	}
	defer store.Close()
	if store.NextSequence() != 1 {
		t.Fatalf("next sequence = %d, want 1", store.NextSequence())
	}
}

// Evidence must survive migration, or a restarted server would rebuild an
// empty world from a full history.
func TestMigratedEvidenceRebuildsTheWorld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")

	event := testEvent(1, "with evidence")
	event.Evidence = &control.Evidence{
		Kind: control.EvidenceAllocationRunning, Target: "web-0",
		Observed: map[string]string{"node": "base"},
	}
	hash, err := eventHash("", event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(Record{Sequence: 1, Hash: hash, Event: event})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	evidence, err := store.ReplayEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].Target != "web-0" {
		t.Fatalf("migrated evidence is wrong: %+v", evidence)
	}
	if evidence[0].Observed["node"] != "base" {
		t.Fatalf("migrated evidence lost its observations: %+v", evidence[0])
	}
}

// FuzzReplay drives arbitrary bytes through the legacy import path.
//
// Whatever survives must be a verifiable chain from the start. An importer
// that accepted a broken chain would put unverifiable history into a store
// that everything downstream trusts.
func FuzzReplay(f *testing.F) {
	dir := f.TempDir()
	path := writeLegacyLog(&testing.T{}, dir, 3)
	if genuine, err := os.ReadFile(path); err == nil {
		f.Add(genuine)
	}
	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Add([]byte("{not json"))
	f.Add([]byte(`{"sequence":1,"hash":"x","event":{}}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		target := filepath.Join(t.TempDir(), "events.log")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Skip()
		}
		store, err := Open(target)
		if err != nil {
			return
		}
		defer store.Close()

		if err := store.Verify(); err != nil {
			t.Fatalf("an unverifiable chain was imported: %v", err)
		}
		previous := ""
		for index, record := range store.Records() {
			if record.Sequence != uint64(index+1) {
				t.Fatalf("record %d has sequence %d", index, record.Sequence)
			}
			if record.PreviousHash != previous {
				t.Fatalf("record %d breaks the chain", index)
			}
			previous = record.Hash
		}
	})
}
