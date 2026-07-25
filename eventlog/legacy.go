package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// sqliteSuffix names the database created beside a migrated legacy log.
//
// The original file keeps its name so a rollback to a previous build still
// finds the history it expects. That matters more than a tidy filename: an
// upgrade that cannot be undone is not an upgrade an operator can risk.
const sqliteSuffix = ".db"

// legacyLog is a newline-delimited log awaiting import.
type legacyLog struct {
	path    string
	records []Record
	// truncated is the line number of an incomplete trailing record dropped
	// during the read, or zero when the file was intact.
	truncated int
}

// legacyLogAt reports whether path holds a newline-delimited event log.
//
// Detection reads the first line rather than trusting the extension. An
// operator who named their SQLite database `events.jsonl` should still get a
// database, and one who named their jsonl log `events.db` should still get a
// migration.
func legacyLogAt(path string) (*legacyLog, error) {
	handle, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	defer handle.Close()

	header := make([]byte, 16)
	read, err := handle.Read(header)
	if err != nil && read == 0 {
		// An empty file is a fresh start, not a legacy log.
		return nil, nil
	}
	// Every SQLite database begins with this string.
	if bytes.HasPrefix(header[:read], []byte("SQLite format 3")) {
		return nil, nil
	}
	if bytes.TrimSpace(header[:read]) == nil || len(bytes.TrimSpace(header[:read])) == 0 {
		return nil, nil
	}
	if header[0] != '{' {
		return nil, fmt.Errorf(
			"%s is neither a SQLite database nor a newline-delimited event log", path)
	}

	if _, err := handle.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	log := &legacyLog{path: path}
	if err := log.read(handle); err != nil {
		return nil, err
	}
	return log, nil
}

// read parses and verifies the legacy chain.
//
// The same rule the file store applied is kept: an undecodable *trailing* line
// is an interrupted append and is dropped, while anything wrong earlier is
// corruption and is fatal. Importing a log without that distinction would
// either refuse to migrate a deployment that lost power once, or silently
// discard history the chain says exists.
func (l *legacyLog) read(handle *os.File) error {
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)

	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(raw, &record); err != nil {
			// Only a trailing partial line is forgivable.
			if scanner.Scan() {
				return fmt.Errorf("decode event log line %d: %w", line, err)
			}
			if scanErr := scanner.Err(); scanErr != nil {
				return fmt.Errorf("decode event log line %d: %w", line, err)
			}
			l.truncated = line
			break
		}
		l.records = append(l.records, record)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read event log: %w", err)
	}

	// The chain is verified before anything is imported. Migrating a log that
	// does not verify would launder a broken history into a new store and lose
	// the evidence that it was broken.
	if err := verifyChain(l.records); err != nil {
		return fmt.Errorf("refusing to migrate a log that does not verify: %w", err)
	}
	return nil
}

// importLegacy copies a verified legacy log into the database.
//
// The whole import is one transaction: a partial migration would leave a store
// that looks like a shorter history rather than an interrupted copy, and
// nothing downstream could tell the difference.
func (f *File) importLegacy(log *legacyLog) error {
	if f.count > 0 {
		// The database already holds history. This happens when a migrated
		// deployment restarts: the legacy file is still on disk, and re-reading
		// it would duplicate everything.
		if f.count < uint64(len(log.records)) {
			return fmt.Errorf(
				"event log database holds %d records but the legacy log at %s holds %d: "+
					"refusing to guess which is authoritative",
				f.count, log.path, len(log.records))
		}
		return nil
	}
	if len(log.records) == 0 {
		return nil
	}

	transaction, err := f.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer transaction.Rollback()

	insert, err := transaction.Prepare(
		`INSERT INTO records (sequence, previous_hash, hash, recorded_at, event)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare migration insert: %w", err)
	}
	defer insert.Close()

	for _, record := range log.records {
		payload, err := json.Marshal(record.Event)
		if err != nil {
			return fmt.Errorf("encode event %d: %w", record.Sequence, err)
		}
		if _, err := insert.Exec(record.Sequence, record.PreviousHash, record.Hash,
			record.Event.At.UTC().Format(time.RFC3339Nano), string(payload)); err != nil {
			return fmt.Errorf("import event %d: %w", record.Sequence, err)
		}
	}

	head := log.records[len(log.records)-1]
	if _, err := transaction.Exec(
		`UPDATE chain_head SET sequence = ?, hash = ? WHERE id = 1`,
		head.Sequence, head.Hash); err != nil {
		return fmt.Errorf("set migrated chain head: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}

	f.count = head.Sequence
	f.head = head
	f.migrated = len(log.records)
	f.truncated = log.truncated
	return nil
}
