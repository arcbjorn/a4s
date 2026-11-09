package node

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/arcbjorn/a4s/internal/fsutil"
)

type ledgerRecord struct {
	Key    string         `json:"key"`
	Result DispatchResult `json:"result"`
}

// FileLedger is a durable, append-only idempotency ledger. Runtime operations
// must also be idempotent because a process can still fail between mutation and
// recording the result.
type FileLedger struct {
	mu      sync.Mutex
	file    *os.File
	results map[string]DispatchResult
	// size is the length of the acknowledged prefix: every byte before it
	// belongs to a record whose Put returned success.
	size int64
	// broken holds the error that left the file in an unknown state, after
	// which no further append is attempted.
	broken error
}

// maxLedgerLine bounds one record, so a corrupt file cannot make replay read
// an unbounded line into memory.
const maxLedgerLine = 1 << 20

func OpenFileLedger(path string) (*FileLedger, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("ledger path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create ledger directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure ledger permissions: %w", err)
	}
	// The directory entry is synced so a ledger created just before a crash
	// still exists afterward.
	if err := fsutil.SyncDir(filepath.Dir(path)); err != nil {
		file.Close()
		return nil, err
	}
	ledger := &FileLedger{file: file, results: make(map[string]DispatchResult)}
	if err := ledger.replay(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return ledger, nil
}

func (l *FileLedger) replay() error {
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek ledger: %w", err)
	}
	reader := bufio.NewReader(l.file)
	var offset int64
	for line := 1; ; line++ {
		raw, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(raw) > 0 {
				// A final line with no newline is a record whose append was
				// interrupted. Put acknowledges only after the newline is
				// written and synced, so this record was never reported as
				// stored and nothing relies on it. Refusing to open over it
				// would leave the node unable to start after any crash during
				// an append, so it is cut off instead.
				if err := l.truncate(offset); err != nil {
					return fmt.Errorf("drop torn ledger tail at line %d: %w", line, err)
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read ledger: %w", err)
		}
		if len(raw) > maxLedgerLine {
			return fmt.Errorf("ledger line %d exceeds %d bytes", line, maxLedgerLine)
		}
		var record ledgerRecord
		if err := json.Unmarshal(raw[:len(raw)-1], &record); err != nil {
			// A complete line that does not decode is corruption, not an
			// interrupted append, and the node must not guess past it.
			return fmt.Errorf("decode ledger line %d: %w", line, err)
		}
		if record.Key == "" || record.Result.EnvelopeDigest == "" {
			return fmt.Errorf("ledger line %d is incomplete", line)
		}
		offset += int64(len(raw))
		if previous, exists := l.results[record.Key]; exists {
			if previous.EnvelopeDigest != record.Result.EnvelopeDigest {
				return fmt.Errorf("ledger line %d conflicts with idempotency key %q", line, record.Key)
			}
			continue
		}
		l.results[record.Key] = record.Result
	}
	l.size = offset
	_, err := l.file.Seek(0, io.SeekEnd)
	return err
}

// truncate cuts the file back to an acknowledged length and makes that durable.
func (l *FileLedger) truncate(size int64) error {
	if err := l.file.Truncate(size); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *FileLedger) Get(key string) (DispatchResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	result, ok := l.results[key]
	return result, ok
}

func (l *FileLedger) Put(key string, result DispatchResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.results[key]; exists {
		return fmt.Errorf("idempotency key %q already stored", key)
	}
	record, err := json.Marshal(ledgerRecord{Key: key, Result: result})
	if err != nil {
		return fmt.Errorf("encode ledger record: %w", err)
	}
	record = append(record, '\n')
	if l.broken != nil {
		return fmt.Errorf("ledger is unusable after an earlier failure: %w", l.broken)
	}
	if _, err := l.file.Write(record); err != nil {
		l.rollback(err)
		return fmt.Errorf("append ledger record: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		l.rollback(err)
		return fmt.Errorf("sync ledger record: %w", err)
	}
	l.size += int64(len(record))
	l.results[key] = result
	return nil
}

// rollback cuts a partially written record off the end of the file.
//
// A write that failed halfway (a full disk, say) leaves a fragment, and the
// next successful append would follow it on the same line, turning a torn
// tail into corruption in the middle of the file that no restart could get
// past. If the fragment cannot be removed the ledger stops accepting appends
// rather than building on it.
func (l *FileLedger) rollback(cause error) {
	if err := l.truncate(l.size); err != nil {
		l.broken = fmt.Errorf("%v; and the partial record could not be removed: %w", cause, err)
	}
}

func (l *FileLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
