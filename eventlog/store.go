package eventlog

import (
	"bytes"
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

// File is the durable, hash-chained control-plane event log, stored as SQLite
// in WAL mode.
//
// The hash chain is retained on top of the database rather than replaced by it:
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
// Replaying and verifying the chain is part of the normal startup path rather
// than a special case, which is what keeps recovery from rotting.
func Open(path string) (*File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("event log path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create event log directory: %w", err)
	}

	// An existing file that is not a database is refused before the driver sees
	// it, so a wrong --event-log path fails with a clear message rather than a
	// SQLite parse error.
	if err := checkDatabaseFile(path); err != nil {
		return nil, err
	}

	db, err := openDatabase(path)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	store := &File{path: path, db: db}
	if err := store.loadHead(); err != nil {
		db.Close()
		return nil, err
	}
	// The chain is verified at open, not on first read. loadHead only checks that
	// the head and the row count agree, which a store holding undecodable event
	// blobs still satisfies: it opens cleanly and then fails later, in whichever
	// caller happens to read records first. Recovery is the normal startup path,
	// so a log that cannot be replayed must refuse to open.
	if err := store.Verify(); err != nil {
		db.Close()
		return nil, fmt.Errorf("event log failed verification: %w", err)
	}
	return store, nil
}

// checkDatabaseFile rejects a non-empty file that is not a SQLite database.
func checkDatabaseFile(path string) error {
	handle, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer handle.Close()

	header := make([]byte, 16)
	read, err := handle.Read(header)
	if err != nil && read == 0 {
		// An empty file is a fresh start.
		return nil
	}
	// Every SQLite database begins with this string.
	if !bytes.HasPrefix(header[:read], []byte("SQLite format 3")) {
		return fmt.Errorf("%s is not a SQLite event log", path)
	}
	return nil
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

// ErrDiverged marks two logs that were never the same history.
//
// It exists so a follower can tell this failure from a transport one. An
// unreachable primary is the case retrying exists for; a divergence is
// permanent, because no amount of pulling turns a different history into the
// primary's, and a loop that retries it forever is hiding the one failure its
// operator most needs to hear about.
var ErrDiverged = errors.New("histories have diverged")

// Ingest appends records from a primary log, re-deriving every hash locally.
//
// It deliberately does not copy the primary's hashes. Each event is appended
// through the ordinary path, which computes the hash from this store's own chain
// head, and the result is then compared with what the primary recorded. Agreement
// means the follower independently derived the same history; a copy would only
// mean it faithfully reproduced whatever it was sent, which is not the same
// claim and is exactly the one that matters when deciding whether a standby may
// take over.
//
// Records at or below the current head are skipped, so ingesting an overlapping
// batch is idempotent and a follower can be fed the whole log repeatedly.
// Returns how many records were appended.
func (f *File) Ingest(records []Record) (int, error) {
	appended := 0
	for _, record := range records {
		if record.Sequence <= f.NextSequence()-1 {
			// Already held. Verify it agrees rather than silently skipping,
			// because a divergence in history the follower already has is the
			// most important kind to notice.
			if err := f.confirmRecord(record); err != nil {
				return appended, err
			}
			continue
		}
		if err := f.Append(record.Event); err != nil {
			return appended, fmt.Errorf("ingest record %d: %w", record.Sequence, err)
		}
		if head := f.Head(); head.Hash != record.Hash {
			return appended, fmt.Errorf(
				"ingested record %d derives hash %s but the primary recorded %s: %w",
				record.Sequence, head.Hash, record.Hash, ErrDiverged)
		}
		appended++
	}
	return appended, nil
}

// confirmRecord checks that a record the follower already holds matches what the
// primary sent. One row answers it: only the hash at that sequence is in
// question, and the chain already commits it to everything before it.
func (f *File) confirmRecord(record Record) error {
	if record.Sequence == 0 {
		return nil
	}
	hash, err := f.HashAt(record.Sequence)
	if err != nil {
		return err
	}
	if hash == "" {
		// Beyond what this store holds; Append will derive it when it arrives.
		return nil
	}
	if hash != record.Hash {
		return fmt.Errorf(
			"record %d differs from the primary: %w", record.Sequence, ErrDiverged)
	}
	return nil
}

// HashAt returns the chain hash at a sequence, or empty when no record holds
// it. The anchor check and ingest confirmation need exactly this: one hash,
// answered by the sequence index rather than by decoding the whole log.
func (f *File) HashAt(sequence uint64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return "", fmt.Errorf("event log is closed")
	}
	var hash string
	err := f.db.QueryRow(
		`SELECT hash FROM records WHERE sequence = ?`, sequence).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read record %d: %w", sequence, err)
	}
	return hash, nil
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

// RecordsAfter returns records with a sequence greater than after, in order,
// at most limit of them. A non-positive limit means no bound.
//
// This is the replication read. A follower polls with its own head as after,
// and the common case is a follower that is already caught up, so the answer
// has to come from the sequence index rather than from decoding the whole log
// the way Records does; a poll every few seconds must not cost the primary its
// entire history each time.
func (f *File) RecordsAfter(after uint64, limit int) []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return nil
	}
	if limit <= 0 {
		// SQLite reads LIMIT -1 as unbounded.
		limit = -1
	}
	rows, err := f.db.Query(
		`SELECT sequence, previous_hash, hash, event FROM records
		 WHERE sequence > ? ORDER BY sequence LIMIT ?`, after, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	records, err := scanRecords(rows)
	if err != nil {
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
	return scanRecords(rows)
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	var records []Record
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
	if err := verifyChain(records); err != nil {
		return err
	}
	// The chain proves each record follows the one before it, but nothing in
	// the records says where the chain ends. The chain_head row does, and it is
	// what an anchor compares against. Without tying it to the last record, an
	// edit that re-derives every hash after the changed record while leaving
	// the head row alone would verify here and match the anchor too.
	tip := Record{}
	if len(records) > 0 {
		tip = records[len(records)-1]
	}
	if f.head.Sequence != tip.Sequence || f.head.Hash != tip.Hash {
		return fmt.Errorf(
			"chain head (%d, %.12s) does not match the last record (%d, %.12s): history was modified outside a4s",
			f.head.Sequence, f.head.Hash, tip.Sequence, tip.Hash)
	}
	return nil
}

// Head reports the current chain tip. The zero value means an empty log.
func (f *File) Head() Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head
}

// Path reports where this store lives.
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
