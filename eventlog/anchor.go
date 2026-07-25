package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Anchor records chain heads outside the event store.
//
// The hash chain detects an edit or a reordering, because every record commits to
// its predecessor. What it cannot detect is replacement of the whole store: a
// forged database with an internally consistent chain verifies perfectly, since
// the only copy of the real head lived inside the file that was swapped. The
// anchor is the outside witness that closes that gap.
//
// It is a separate append-only file rather than a row in the database, and that
// separation is the entire point. Storing it alongside the records would put the
// witness inside the thing it witnesses.
type Anchor struct {
	mu   sync.Mutex
	path string
	last AnchorRecord
}

// AnchorRecord is one witnessed chain head.
type AnchorRecord struct {
	Sequence    uint64    `json:"sequence"`
	Hash        string    `json:"hash"`
	WitnessedAt time.Time `json:"witnessed_at"`
}

// OpenAnchor opens or creates an anchor file.
//
// The file is read fully so the highest witnessed head is known before the store
// is checked against it. A malformed line is fatal: an anchor that cannot be
// read is not evidence of anything, and treating it as empty would silently
// discard the protection it exists to provide.
func OpenAnchor(path string) (*Anchor, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("anchor path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create anchor directory: %w", err)
	}
	anchor := &Anchor{path: path}
	handle, err := os.Open(path)
	if os.IsNotExist(err) {
		return anchor, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open anchor: %w", err)
	}
	defer handle.Close()

	var records []AnchorRecord
	scanner := bufio.NewScanner(handle)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var record AnchorRecord
		if err := json.Unmarshal([]byte(text), &record); err != nil {
			return nil, fmt.Errorf("anchor line %d is malformed: %w", line, err)
		}
		if record.Hash == "" {
			return nil, fmt.Errorf("anchor line %d has no hash", line)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read anchor: %w", err)
	}
	// The highest sequence is the witness that matters, whatever order the file
	// happens to be in.
	sort.Slice(records, func(i, j int) bool { return records[i].Sequence < records[j].Sequence })
	if len(records) > 0 {
		anchor.last = records[len(records)-1]
	}
	return anchor, nil
}

// Last reports the highest witnessed head, or a zero record when none exists.
func (a *Anchor) Last() AnchorRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// Witness appends a chain head to the anchor.
//
// Going backwards is refused. An anchor that accepted a lower sequence would let
// a rolled-back or truncated store re-witness itself into looking current, which
// is one of the attacks this is here to catch.
func (a *Anchor) Witness(sequence uint64, hash string) error {
	if hash == "" {
		return fmt.Errorf("anchor requires a chain hash")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sequence < a.last.Sequence {
		return fmt.Errorf("anchor already witnessed sequence %d, refusing to record %d",
			a.last.Sequence, sequence)
	}
	if sequence == a.last.Sequence && hash != a.last.Hash {
		return fmt.Errorf("anchor witnessed a different hash at sequence %d", sequence)
	}
	if sequence == a.last.Sequence {
		// Already witnessed exactly this head; nothing to record.
		return nil
	}

	record := AnchorRecord{Sequence: sequence, Hash: hash, WitnessedAt: time.Now().UTC()}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode anchor record: %w", err)
	}
	handle, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open anchor for append: %w", err)
	}
	defer handle.Close()
	if _, err := handle.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("append anchor record: %w", err)
	}
	// Synced before returning: an anchor that is still in the page cache when the
	// machine dies witnesses nothing.
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync anchor: %w", err)
	}
	a.last = record
	return nil
}

// Check verifies a store's head against the anchor.
//
// Three outcomes are distinguished because they mean different things:
//
//   - The store is at or ahead of the witnessed head with a matching hash at that
//     point: normal, including a store that has appended since the last witness.
//   - The store is behind the witnessed head: it was truncated, rolled back, or
//     replaced with an older copy.
//   - The store's hash at the witnessed sequence differs: history was rewritten
//     or the whole store was substituted.
func (a *Anchor) Check(store *File) error {
	a.mu.Lock()
	witnessed := a.last
	a.mu.Unlock()
	if witnessed.Hash == "" {
		// Nothing witnessed yet, so there is nothing to contradict.
		return nil
	}
	head := store.Head()
	if head.Sequence < witnessed.Sequence {
		return fmt.Errorf(
			"event log is at sequence %d but the anchor witnessed %d: the log was truncated or replaced",
			head.Sequence, witnessed.Sequence)
	}
	if head.Sequence == witnessed.Sequence {
		if head.Hash != witnessed.Hash {
			return fmt.Errorf(
				"event log hash at sequence %d does not match the anchor: history was rewritten",
				witnessed.Sequence)
		}
		return nil
	}

	// The store has grown since the last witness, so check that the witnessed
	// record is still the one at that sequence.
	records, err := store.readRecords()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Sequence != witnessed.Sequence {
			continue
		}
		if record.Hash != witnessed.Hash {
			return fmt.Errorf(
				"event log hash at sequence %d does not match the anchor: history was rewritten",
				witnessed.Sequence)
		}
		return nil
	}
	return fmt.Errorf("event log has no record at anchored sequence %d", witnessed.Sequence)
}
