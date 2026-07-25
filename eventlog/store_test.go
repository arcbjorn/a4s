package eventlog

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func testEvent(sequence uint64, message string) control.Event {
	return control.Event{
		Sequence: sequence,
		At:       time.Unix(1700000000+int64(sequence), 0).UTC(),
		Type:     control.EventObservationRecorded,
		Actor:    "test",
		Message:  message,
		Target:   fmt.Sprintf("web-%d", sequence),
	}
}

// appendedStore builds a store holding the given number of records.
func appendedStore(t *testing.T, count int) (*File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= count; index++ {
		if err := store.Append(testEvent(store.NextSequence(),
			fmt.Sprintf("event %d", index))); err != nil {
			t.Fatalf("append %d: %v", index, err)
		}
	}
	return store, path
}

func TestStorePersistsAndReplaysHashChain(t *testing.T) {
	store, path := appendedStore(t, 3)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	records := reopened.Records()
	if len(records) != 3 {
		t.Fatalf("recovered %d records, want 3", len(records))
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("recovered chain does not verify: %v", err)
	}
	if reopened.NextSequence() != 4 {
		t.Fatalf("next sequence = %d, want 4", reopened.NextSequence())
	}
}

// The chain is what distinguishes rows SQLite committed from rows a4s wrote.
// Editing history through sqlite3 must be detected.
func TestStoreDetectsRecordEditedThroughSQL(t *testing.T) {
	store, path := appendedStore(t, 3)
	store.Close()

	// Edit the middle record the way someone with file access would.
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE records SET event = json_set(event, '$.message', 'tampered') WHERE sequence = 2`,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Refused at open rather than on first read. Recovery is the normal startup
	// path, so a control plane must not come up believing edited history.
	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("a log with an edited record opened cleanly")
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Fatalf("the failure did not name verification: %v", err)
	}
}

// Deleting a record through SQL must be caught by the head/count disagreement
// before any history is served.
func TestStoreDetectsDeletedRecord(t *testing.T) {
	store, path := appendedStore(t, 3)
	store.Close()

	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM records WHERE sequence = 3`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("a log with a deleted record opened cleanly")
	} else if !strings.Contains(err.Error(), "modified outside a4s") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The schema itself must refuse a duplicate sequence, so a gap or a fork
// cannot be introduced even by direct SQL.
func TestSchemaRefusesDuplicateSequence(t *testing.T) {
	store, path := appendedStore(t, 2)
	store.Close()

	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(
		`INSERT INTO records (sequence, previous_hash, hash, recorded_at, event)
		 VALUES (2, '', 'x', 'now', '{}')`)
	if err == nil {
		t.Fatal("the schema accepted a duplicate sequence")
	}
}

// Two records cannot share a hash, or "which record does this follow" becomes
// ambiguous.
func TestSchemaRefusesDuplicateHash(t *testing.T) {
	store, path := appendedStore(t, 2)
	records := store.Records()
	store.Close()

	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(
		`INSERT INTO records (sequence, previous_hash, hash, recorded_at, event)
		 VALUES (3, ?, ?, 'now', '{}')`, records[1].Hash, records[1].Hash)
	if err == nil {
		t.Fatal("the schema accepted a duplicate hash")
	}
}

func TestAppendRejectsOutOfOrderSequence(t *testing.T) {
	store, _ := appendedStore(t, 2)
	defer store.Close()

	if err := store.Append(testEvent(99, "wrong")); err == nil {
		t.Fatal("append accepted an out-of-order sequence")
	}
	// The rejected append must not have advanced anything.
	if store.NextSequence() != 3 {
		t.Fatalf("next sequence = %d after a rejected append, want 3", store.NextSequence())
	}
}

// A failed append must leave no partial state: no record without a head
// advance, and no head advance without a record.
func TestFailedAppendLeavesNoPartialState(t *testing.T) {
	store, path := appendedStore(t, 2)

	if err := store.Append(testEvent(store.NextSequence(), "fine")); err != nil {
		t.Fatal(err)
	}
	before := store.NextSequence()
	if err := store.Append(testEvent(before+5, "gap")); err == nil {
		t.Fatal("expected a sequence gap to be refused")
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed append: %v", err)
	}
	defer reopened.Close()
	if reopened.NextSequence() != before {
		t.Fatalf("next sequence = %d, want %d", reopened.NextSequence(), before)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("chain broken after a failed append: %v", err)
	}
}

func TestEmptyLogOpensCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open empty log: %v", err)
	}
	defer store.Close()

	if len(store.Records()) != 0 {
		t.Fatalf("a fresh log holds %d records", len(store.Records()))
	}
	if store.NextSequence() != 1 {
		t.Fatalf("next sequence = %d, want 1", store.NextSequence())
	}
	if err := store.Verify(); err != nil {
		t.Fatalf("empty chain does not verify: %v", err)
	}
}

// The log holds authoritative control-plane history and must not be world
// readable.
func TestLogPermissionsAreRestrictive(t *testing.T) {
	store, path := appendedStore(t, 1)
	defer store.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("event log mode = %o, want 600", mode)
	}
}

// Appends are serialized, so concurrent callers produce a valid chain rather
// than a fork. The race detector makes this meaningful.
func TestConcurrentAppendsProduceAValidChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var group sync.WaitGroup
	var accepted int64
	var mutex sync.Mutex
	for range 25 {
		group.Add(1)
		go func() {
			defer group.Done()
			// Each caller reads the next sequence and appends, which is exactly
			// how the server uses this API.
			mutex.Lock()
			defer mutex.Unlock()
			if err := store.Append(testEvent(store.NextSequence(), "concurrent")); err == nil {
				accepted++
			}
		}()
	}
	group.Wait()

	if err := store.Verify(); err != nil {
		t.Fatalf("concurrent appends broke the chain: %v", err)
	}
	if got := len(store.Records()); int64(got) != accepted {
		t.Fatalf("stored %d records but accepted %d", got, accepted)
	}
}

// Replay must be a pure function of the log, or a rebuilt world would depend
// on when it was rebuilt.
func TestReplayIsDeterministic(t *testing.T) {
	store, _ := appendedStore(t, 5)
	defer store.Close()

	first, err := store.ReplayEvents()
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		next, err := store.ReplayEvents()
		if err != nil {
			t.Fatal(err)
		}
		if len(next) != len(first) {
			t.Fatalf("replay returned %d events then %d", len(first), len(next))
		}
		for index := range first {
			if next[index].Sequence != first[index].Sequence ||
				next[index].Message != first[index].Message {
				t.Fatalf("replay is not deterministic at %d", index)
			}
		}
	}
}

func TestReplayEvidenceReturnsOnlyObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	plain := testEvent(1, "no evidence")
	if err := store.Append(plain); err != nil {
		t.Fatal(err)
	}
	observed := testEvent(2, "with evidence")
	observed.Evidence = &control.Evidence{
		Kind: control.EvidenceAllocationRunning, Target: "web-0",
	}
	if err := store.Append(observed); err != nil {
		t.Fatal(err)
	}

	evidence, err := store.ReplayEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 {
		t.Fatalf("replayed %d evidence items, want 1", len(evidence))
	}
	if evidence[0].Target != "web-0" {
		t.Fatalf("unexpected evidence: %+v", evidence[0])
	}
}

// A store written by a newer build must be refused rather than interpreted
// through a schema that no longer describes it.
func TestFutureSchemaVersionIsRefused(t *testing.T) {
	store, path := appendedStore(t, 1)
	store.Close()

	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_migrations (version, name, applied_at)
		 VALUES (?, 'from the future', datetime('now'))`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("a store from a newer build opened cleanly")
	} else if !strings.Contains(err.Error(), "upgrade a4s") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Migrations must be idempotent: reopening an existing store must not reapply
// them or fail.
func TestMigrationsAreIdempotent(t *testing.T) {
	store, path := appendedStore(t, 2)
	store.Close()

	for range 3 {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if len(reopened.Records()) != 2 {
			t.Fatalf("records = %d after reopen, want 2", len(reopened.Records()))
		}
		reopened.Close()
	}
}

// WAL is what lets readers proceed during a write. Losing it silently would
// change the concurrency story without anyone noticing.
func TestWALAndDurabilityPragmasAreSet(t *testing.T) {
	store, _ := appendedStore(t, 1)
	defer store.Close()

	var mode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	var synchronous int
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	// 2 is FULL: every commit is fsynced, which is the durability an
	// acknowledged event requires.
	if synchronous != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", synchronous)
	}
}

func TestAppendAfterCloseIsRefused(t *testing.T) {
	store, _ := appendedStore(t, 1)
	store.Close()
	if err := store.Append(testEvent(2, "after close")); err == nil {
		t.Fatal("append succeeded on a closed log")
	}
}

// A second process opening the same log must not corrupt it.
//
// The file store had no defence here beyond an in-process mutex: two servers
// sharing a log would interleave appends and fork the chain. SQLite serializes
// writers, and the conditional head update turns a lost race into a refused
// append rather than a silent fork.
func TestSecondOpenerCannotForkTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Append(testEvent(1, "first")); err != nil {
		t.Fatal(err)
	}

	// A second store on the same file, as a second process would have.
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	// Both believe the next sequence is 2. Only one may succeed.
	firstErr := first.Append(testEvent(2, "from first"))
	secondErr := second.Append(testEvent(2, "from second"))
	if firstErr == nil && secondErr == nil {
		t.Fatal("both openers appended sequence 2: the chain forked")
	}
	if firstErr != nil && secondErr != nil {
		t.Fatalf("neither append succeeded: %v / %v", firstErr, secondErr)
	}

	// Whichever won, the chain must still verify from a fresh reader.
	first.Close()
	second.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after contention: %v", err)
	}
	defer reopened.Close()
	if err := reopened.Verify(); err != nil {
		t.Fatalf("contention broke the chain: %v", err)
	}
	if got := len(reopened.Records()); got != 2 {
		t.Fatalf("records = %d, want 2", got)
	}
}

// FuzzOpen asserts that arbitrary bytes at the event-log path either fail to
// open or yield a chain that verifies. A store that opened a corrupt or forged
// file and reported it as valid history would be the bug worth finding, and a
// crash-only fuzz test would not catch it.
func FuzzOpen(f *testing.F) {
	// A genuine store, so the fuzzer starts from valid database bytes.
	store, path := appendedStore(&testing.T{}, 3)
	store.Close()
	if genuine, err := os.ReadFile(path); err == nil {
		f.Add(genuine)
	}
	f.Add([]byte(""))
	f.Add([]byte("SQLite format 3\x00"))
	f.Add([]byte("{not json"))
	f.Add([]byte(`{"sequence":1,"hash":"x","event":{}}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		target := filepath.Join(t.TempDir(), "events.db")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Skip()
		}
		store, err := Open(target)
		if err != nil {
			return
		}
		defer store.Close()

		if err := store.Verify(); err != nil {
			t.Fatalf("an unverifiable chain opened cleanly: %v", err)
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

// A store whose event blobs cannot be decoded must refuse to open.
//
// Found by FuzzOpen. loadHead only checks that the chain head and the row count
// agree, which this store satisfies: it opened cleanly and then failed in
// whichever caller read records first — the projection rebuild, a backup, or a
// replay. Failing at open keeps that from becoming a control plane that is up
// but cannot read its own history.
func TestStoreRefusesUndecodableEvents(t *testing.T) {
	store, path := appendedStore(t, 3)
	store.Close()

	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// Valid SQLite, invalid JSON: the row count and chain head still agree.
	if _, err := db.Exec(
		`UPDATE records SET event = '{"sequence":2,"at":}' WHERE sequence = 2`,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("a log with an undecodable event opened cleanly")
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Fatalf("the failure did not name verification: %v", err)
	}
}
