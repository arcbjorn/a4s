package eventlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/arcbjorn/a4s/control"
)

// eventHash links one event to the record before it.
//
// The previous hash and the event payload are separated by a zero byte so two
// different splits of the same bytes cannot produce the same digest. The
// payload is the canonical JSON encoding of the event, which is also what is
// stored, so a hash can always be re-derived from what the database holds.
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

// verifyChain re-derives every hash and checks the links between records.
//
// This is the only check that distinguishes "these are the rows SQLite
// committed" from "these are the rows a4s wrote". The database guarantees the
// former; only the chain establishes the latter.
func verifyChain(records []Record) error {
	previous := ""
	for index, record := range records {
		sequence := uint64(index + 1)
		if record.Sequence != sequence || record.Event.Sequence != record.Sequence {
			return fmt.Errorf("record %d has invalid sequence", sequence)
		}
		if record.PreviousHash != previous {
			return fmt.Errorf("record %d breaks the hash chain", sequence)
		}
		hash, err := eventHash(previous, record.Event)
		if err != nil {
			return err
		}
		if hash != record.Hash {
			return fmt.Errorf("record %d has invalid hash", sequence)
		}
		previous = record.Hash
	}
	return nil
}
