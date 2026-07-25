package eventlog

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"

	_ "modernc.org/sqlite"
)

// driverName is the pure-Go SQLite driver.
//
// It is pure Go on purpose: a4s cross-compiles to linux/amd64 and linux/arm64
// with CGO_ENABLED=0, and a CGO-linked driver would end that. The cost is a
// dependency; the alternative was either dropping static cross-builds or
// keeping a hand-rolled file format for state that deserves a real database.
const driverName = "sqlite"

// busyTimeout bounds how long a writer waits for a competing one.
//
// Two writers should not happen: one server owns its event log. The timeout is
// what turns the case where it does happen anyway — an operator running a CLI
// command against a live log — into a short wait instead of an immediate error.
const busyTimeout = 5 * time.Second

// File is the durable, hash-chained control-plane event log.
//
// The name is kept for its callers, but the storage is SQLite in WAL mode. The
// hash chain is retained on top of the database rather than replaced by it:
// SQLite protects against torn pages and partial writes, while the chain
// protects against someone editing history through sqlite3 directly. Those are
// different threats, and the second is the one an audit trail exists for.
type File struct {
	mu   sync.Mutex
	path string
	db   *sql.DB
	// head mirrors the chain tip so an append does not need to read it back.
	// It is authoritative only while the mutex is held; the database row is the
	// durable copy and is verified against this on every append.
	head Record
	// count is the number of records, kept for NextSequence.
	count uint64
	// migrated records how many records were imported from a legacy log.
	migrated int
	// truncated reports a trailing partial record dropped from a legacy log at
	// import time. SQLite makes this impossible for new appends; the field
	// remains so a migrated deployment can still report what it recovered.
	truncated int
}

// Record is one entry in the chain.
type Record struct {
	Sequence     uint64        `json:"sequence"`
	PreviousHash string        `json:"previous_hash,omitempty"`
	Hash         string        `json:"hash"`
	Event        control.Event `json:"event"`
}

// Open opens or creates an event log.
//
// A legacy newline-delimited log at the same path is imported automatically,
// so an existing deployment upgrades by replacing the binary. Recovery and
// migration are both part of the normal startup path rather than special
// cases, which is what keeps them from rotting.
func Open(path string) (*File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("event log path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create event log directory: %w", err)
	}

	// A legacy log is detected before the database is created, because the
	// check is "is the existing file a jsonl log" and creating the database
	// first would answer that question about the wrong file.
	legacy, err := legacyLogAt(path)
	if err != nil {
		return nil, err
	}

	databasePath := path
	if legacy != nil {
		// The legacy file keeps its name. The database is created beside it and
		// the old log is preserved, so a rollback to the previous build still
		// finds the history it expects.
		databasePath = path + sqliteSuffix
	}

	db, err := openDatabase(databasePath)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	store := &File{path: databasePath, db: db}
	if err := store.loadHead(); err != nil {
		db.Close()
		return nil, err
	}
	if legacy != nil {
		if err := store.importLegacy(legacy); err != nil {
			db.Close()
			return nil, err
		}
	}
	return store, nil
}

// openDatabase opens SQLite with the pragmas the control plane needs.
func openDatabase(path string) (*sql.DB, error) {
	// journal_mode=WAL       readers never block the writer, and a crash
	//                        recovers from the write-ahead log.
	// synchronous=FULL       every commit is fsynced. This is the durability
	//                        the previous implementation got from syncing each
	//                        append, and losing it would mean acknowledging an
	//                        event that a power loss could still erase.
	// foreign_keys=ON        enforced rather than advisory.
	// busy_timeout           wait rather than fail on a contended write.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"+
			"&_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)",
		path, busyTimeout.Milliseconds())

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}

	// One writer. SQLite serializes writes anyway, and a pool would only add
	// contention between connections that cannot proceed in parallel.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open event log: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("secure event log permissions: %w", err)
	}
	return db, nil
}

// openReadOnly opens a database without writing to it.
//
// The immutable flag tells SQLite the file will not change, which stops it
// from creating WAL or shared-memory sidecars and from checkpointing on close.
// That is what lets a backup be verified repeatedly without altering it.
func openReadOnly(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1&_pragma=busy_timeout(%d)",
		path, busyTimeout.Milliseconds())
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open event log read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open event log read-only: %w", err)
	}
	return db, nil
}

// loadHead reads the chain tip and record count.
func (f *File) loadHead() error {
	var sequence uint64
	var hash string
	if err := f.db.QueryRow(
		`SELECT sequence, hash FROM chain_head WHERE id = 1`).Scan(&sequence, &hash); err != nil {
		return fmt.Errorf("read chain head: %w", err)
	}

	var count uint64
	if err := f.db.QueryRow(`SELECT count(*) FROM records`).Scan(&count); err != nil {
		return fmt.Errorf("count event records: %w", err)
	}
	// The head and the table must agree. A mismatch means something wrote to
	// one without the other, which is exactly the direct-sqlite3 edit the chain
	// exists to detect.
	if count != sequence {
		return fmt.Errorf(
			"event log holds %d records but its chain head is at %d: history was modified outside a4s",
			count, sequence)
	}

	f.count = count
	f.head = Record{Sequence: sequence, Hash: hash}
	return nil
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return nil
	}
	err := f.db.Close()
	f.db = nil
	return err
}

// Append records one event, extending the hash chain.
//
// The write and the chain-head update happen in one transaction, so a crash
// cannot leave a record whose head was never advanced or a head pointing at a
// record that was never written.
func (f *File) Append(event control.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return fmt.Errorf("event log is closed")
	}

	expected := f.count + 1
	if event.Sequence != expected {
		return fmt.Errorf("event sequence %d, want %d", event.Sequence, expected)
	}
	hash, err := eventHash(f.head.Hash, event)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}

	transaction, err := f.db.Begin()
	if err != nil {
		return fmt.Errorf("begin append: %w", err)
	}
	defer transaction.Rollback()

	if _, err := transaction.Exec(
		`INSERT INTO records (sequence, previous_hash, hash, recorded_at, event)
		 VALUES (?, ?, ?, ?, ?)`,
		expected, f.head.Hash, hash, event.At.UTC().Format(time.RFC3339Nano),
		string(payload)); err != nil {
		return fmt.Errorf("append event record: %w", err)
	}

	// Advancing the head only from the sequence this append observed means two
	// writers cannot both believe they extended the same chain: the second
	// update matches no row and the append fails rather than forking history.
	result, err := transaction.Exec(
		`UPDATE chain_head SET sequence = ?, hash = ? WHERE id = 1 AND sequence = ?`,
		expected, hash, f.head.Sequence)
	if err != nil {
		return fmt.Errorf("advance chain head: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance chain head: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("event log chain head moved underneath this append")
	}

	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit event record: %w", err)
	}

	f.head = Record{Sequence: expected, PreviousHash: f.head.Hash, Hash: hash, Event: event}
	f.count = expected
	return nil
}

// Records returns every record in sequence order.
func (f *File) Records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.readRecords()
	if err != nil {
		// The previous implementation held records in memory and could not
		// fail here. Callers treat this as "the history", so returning nothing
		// on a read error is the honest answer rather than a partial one.
		return nil
	}
	return records
}

func (f *File) readRecords() ([]Record, error) {
	if f.db == nil {
		return nil, fmt.Errorf("event log is closed")
	}
	rows, err := f.db.Query(
		`SELECT sequence, previous_hash, hash, event FROM records ORDER BY sequence`)
	if err != nil {
		return nil, fmt.Errorf("read event records: %w", err)
	}
	defer rows.Close()

	records := make([]Record, 0, f.count)
	for rows.Next() {
		var record Record
		var payload string
		if err := rows.Scan(&record.Sequence, &record.PreviousHash,
			&record.Hash, &payload); err != nil {
			return nil, fmt.Errorf("scan event record: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &record.Event); err != nil {
			return nil, fmt.Errorf("decode event %d: %w", record.Sequence, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read event records: %w", err)
	}
	return records, nil
}

func (f *File) NextSequence() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count + 1
}

// ReplayEvents returns every recorded event in sequence order, for read-only
// analysis such as explanation and diagnosis.
func (f *File) ReplayEvents() ([]control.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.readRecords()
	if err != nil {
		return nil, err
	}
	events := make([]control.Event, 0, len(records))
	for _, record := range records {
		events = append(events, record.Event)
	}
	return events, nil
}

// ReplayEvidence returns every recorded observation in sequence order. It is
// what allows a restarted server to rebuild its world projection from durable
// history rather than from memory.
func (f *File) ReplayEvidence() ([]control.Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.readRecords()
	if err != nil {
		return nil, err
	}
	evidence := make([]control.Evidence, 0, len(records))
	for _, record := range records {
		if record.Event.Evidence == nil {
			continue
		}
		evidence = append(evidence, *record.Event.Evidence)
	}
	return evidence, nil
}

// Verify re-derives the whole chain and reports the first inconsistency.
//
// SQLite guarantees the rows are the ones that were committed; it says nothing
// about whether they are the ones a4s wrote. This is the check that catches an
// edit made through sqlite3, and it is why the chain was kept.
func (f *File) Verify() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	records, err := f.readRecords()
	if err != nil {
		return err
	}
	return verifyChain(records)
}

// Truncated reports how many records were dropped from a legacy log during
// import. Zero means nothing was recovered, which is the normal case.
func (f *File) Truncated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.truncated
}

// Migrated reports how many records were imported from a legacy log.
func (f *File) Migrated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.migrated
}

// Path reports where this store lives, which differs from the requested path
// when a legacy log was migrated beside it.
func (f *File) Path() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.path
}

// quoteLiteral escapes a string for use in SQL that cannot be parameterized.
//
// Only VACUUM INTO needs this: SQLite does not accept a bound parameter for
// its destination. Every other statement in this package uses placeholders.
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
