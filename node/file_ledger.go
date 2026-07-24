package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
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
}

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
	scanner := bufio.NewScanner(l.file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var record ledgerRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("decode ledger line %d: %w", line, err)
		}
		if record.Key == "" || record.Result.EnvelopeDigest == "" {
			return fmt.Errorf("ledger line %d is incomplete", line)
		}
		if previous, exists := l.results[record.Key]; exists {
			if previous.EnvelopeDigest != record.Result.EnvelopeDigest {
				return fmt.Errorf("ledger line %d conflicts with idempotency key %q", line, record.Key)
			}
			continue
		}
		l.results[record.Key] = record.Result
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}
	_, err := l.file.Seek(0, io.SeekEnd)
	return err
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
	if _, err := l.file.Write(record); err != nil {
		return fmt.Errorf("append ledger record: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync ledger record: %w", err)
	}
	l.results[key] = result
	return nil
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
