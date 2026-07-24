// Package eventlog persists the append-only control-plane audit trail.
package eventlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

type Record struct {
	Sequence     uint64        `json:"sequence"`
	PreviousHash string        `json:"previous_hash,omitempty"`
	Hash         string        `json:"hash"`
	Event        control.Event `json:"event"`
}

// File is a newline-delimited, hash-chained event store. It is intentionally
// simpler than a database for the first node slice: append, fsync, replay, and
// detect corruption. Materialized views remain rebuildable from this log.
type File struct {
	mu      sync.Mutex
	path    string
	handle  *os.File
	records []Record
	// truncated records the line number of an incomplete trailing record
	// removed at open time. Zero means the log was intact.
	truncated int
}

func Open(path string) (*File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("event log path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create event log directory: %w", err)
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	if err := handle.Chmod(0o600); err != nil {
		handle.Close()
		return nil, fmt.Errorf("secure event log permissions: %w", err)
	}
	store := &File{path: path, handle: handle}
	if err := store.replay(); err != nil {
		handle.Close()
		return nil, err
	}
	return store, nil
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handle == nil {
		return nil
	}
	err := f.handle.Close()
	f.handle = nil
	return err
}

func (f *File) Append(event control.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handle == nil {
		return fmt.Errorf("event log is closed")
	}
	expected := uint64(len(f.records) + 1)
	if event.Sequence != expected {
		return fmt.Errorf("event sequence %d, want %d", event.Sequence, expected)
	}
	previous := ""
	if len(f.records) > 0 {
		previous = f.records[len(f.records)-1].Hash
	}
	hash, err := eventHash(previous, event)
	if err != nil {
		return err
	}
	record := Record{Sequence: expected, PreviousHash: previous, Hash: hash, Event: event}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode event record: %w", err)
	}
	line = append(line, '\n')
	if _, err := f.handle.Write(line); err != nil {
		return fmt.Errorf("append event record: %w", err)
	}
	if err := f.handle.Sync(); err != nil {
		return fmt.Errorf("sync event record: %w", err)
	}
	f.records = append(f.records, record)
	return nil
}

func (f *File) Records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.records...)
}

func (f *File) NextSequence() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.records) + 1)
}

// ReplayEvents returns every recorded event in sequence order, for read-only
// analysis such as explanation and diagnosis.
func (f *File) ReplayEvents() ([]control.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events := make([]control.Event, 0, len(f.records))
	for _, record := range f.records {
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
	evidence := make([]control.Evidence, 0, len(f.records))
	for _, record := range f.records {
		if record.Event.Evidence == nil {
			continue
		}
		evidence = append(evidence, *record.Event.Evidence)
	}
	return evidence, nil
}

// replay rebuilds the in-memory records from the file, distinguishing a crash
// artifact from corruption.
//
// A record is appended and fsynced as one write, but a machine that loses power
// mid-write can still leave a partial final line. That line is not evidence of
// tampering: it is the write that never completed. Refusing to open the log
// because of it would mean a single badly-timed power loss prevents the control
// plane from starting at all, with every prior record intact and verifiable.
//
// So an unreadable *trailing* line is truncated away, while anything wrong
// earlier in the file stays fatal. The distinction matters: a corrupt record
// with valid records after it cannot be explained by an interrupted append, and
// silently dropping it would discard history the chain says exists.
// truncateTrailing removes an undecodable final line, reporting whether it did.
//
// It only acts when the bad line is genuinely last. A line that fails to decode
// with more data after it was not interrupted by a crash — something rewrote
// the middle of an append-only file — and that must surface as corruption
// rather than be quietly discarded along with everything after it.
func (f *File) truncateTrailing(scanner *bufio.Scanner, validBytes int64, line int) (bool, error) {
	if scanner.Scan() {
		return false, nil
	}
	if err := scanner.Err(); err != nil {
		return false, nil
	}
	if err := f.handle.Truncate(validBytes); err != nil {
		return true, fmt.Errorf("truncate incomplete event log record: %w", err)
	}
	if err := f.handle.Sync(); err != nil {
		return true, fmt.Errorf("sync truncated event log: %w", err)
	}
	f.truncated = line
	return true, nil
}

// dropTrailingBytes removes bytes after the last complete record.
//
// This catches the shapes the scanner treats as a clean end of file: a trailing
// blank line, or a final line with no newline that happened to be empty. They
// come from the same interrupted-write cause as a partial record.
func (f *File) dropTrailingBytes(validBytes int64) error {
	info, err := f.handle.Stat()
	if err != nil {
		return fmt.Errorf("stat event log: %w", err)
	}
	if info.Size() == validBytes {
		return nil
	}
	if err := f.handle.Truncate(validBytes); err != nil {
		return fmt.Errorf("truncate incomplete event log record: %w", err)
	}
	if err := f.handle.Sync(); err != nil {
		return fmt.Errorf("sync truncated event log: %w", err)
	}
	if f.truncated == 0 {
		f.truncated = len(f.records) + 1
	}
	return nil
}

// Truncated reports the line number of an incomplete trailing record removed at
// open time, or zero if the log was intact.
//
// An operator needs to know this happened: the control plane recovered, but one
// action may have been dispatched without its outcome ever being recorded.
func (f *File) Truncated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.truncated
}

func (f *File) replay() error {
	if _, err := f.handle.Seek(0, 0); err != nil {
		return fmt.Errorf("seek event log: %w", err)
	}
	scanner := bufio.NewScanner(f.handle)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	previous := ""
	line := 0
	// validBytes is where the last complete, verified record ends. A trailing
	// partial write is truncated back to here.
	validBytes := int64(0)
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		// The scanner strips the newline, so account for it when measuring how
		// much of the file is known good.
		lineBytes := int64(len(raw)) + 1

		var record Record
		if err := json.Unmarshal(raw, &record); err != nil {
			if partial, truncErr := f.truncateTrailing(scanner, validBytes, line); partial {
				return truncErr
			}
			return fmt.Errorf("decode event log line %d: %w", line, err)
		}
		if record.Sequence != uint64(line) || record.Event.Sequence != record.Sequence {
			return fmt.Errorf("event log line %d has invalid sequence", line)
		}
		if record.PreviousHash != previous {
			return fmt.Errorf("event log line %d breaks hash chain", line)
		}
		hash, err := eventHash(previous, record.Event)
		if err != nil {
			return err
		}
		if hash != record.Hash {
			return fmt.Errorf("event log line %d has invalid hash", line)
		}
		f.records = append(f.records, record)
		previous = record.Hash
		validBytes += lineBytes
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	// A trailing newline with nothing after it leaves the scanner having read a
	// complete final record, so validBytes already covers the whole file. A
	// mismatch here means trailing bytes the loop never turned into a record.
	if err := f.dropTrailingBytes(validBytes); err != nil {
		return err
	}
	_, err := f.handle.Seek(0, 2)
	return err
}

func eventHash(previous string, event control.Event) (string, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("encode event hash payload: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(previous))
	hash.Write([]byte{0})
	hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil)), nil
}
