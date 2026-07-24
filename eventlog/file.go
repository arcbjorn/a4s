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

func (f *File) replay() error {
	if _, err := f.handle.Seek(0, 0); err != nil {
		return fmt.Errorf("seek event log: %w", err)
	}
	scanner := bufio.NewScanner(f.handle)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	previous := ""
	line := 0
	for scanner.Scan() {
		line++
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
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
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read event log: %w", err)
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
