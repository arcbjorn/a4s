package eventlog

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// rewriteFrom edits the record at sequence `from` and re-derives every hash
// after it, the way an attacker with sqlite3 access would forge history that
// still forms a valid chain. When moveHead is false the chain_head row is left
// at its original value.
func rewriteFrom(t *testing.T, path string, from uint64, moveHead bool) {
	t.Helper()
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT sequence, previous_hash, hash, event FROM records ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	records, err := scanRecords(rows)
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	records[from-1].Event.Message = "forged"
	previous := records[from-1].PreviousHash
	for index := int(from - 1); index < len(records); index++ {
		hash, err := eventHash(previous, records[index].Event)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(records[index].Event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE records SET previous_hash = ?, hash = ?, event = ? WHERE sequence = ?`,
			previous, hash, string(payload), records[index].Sequence); err != nil {
			t.Fatal(err)
		}
		previous = hash
	}
	if moveHead {
		if _, err := db.Exec(`UPDATE chain_head SET hash = ? WHERE id = 1`, previous); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRefusesRewrittenHistoryUnderUnchangedHead(t *testing.T) {
	store, _, logPath, _ := anchoredStore(t, 3)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Every record still links to its predecessor, so the chain alone verifies.
	// Only the head row, which still names the original tip, gives it away.
	rewriteFrom(t, logPath, 2, false)

	reopened, err := Open(logPath)
	if err == nil {
		reopened.Close()
		t.Fatal("a log whose chain head no longer matches its last record must refuse to open")
	}
	if !strings.Contains(err.Error(), "modified outside a4s") {
		t.Fatalf("want a modified-history error, got %v", err)
	}
}

func TestAnchorRefusesRewriteThatAlsoMovedTheHead(t *testing.T) {
	store, _, logPath, anchorPath := anchoredStore(t, 3)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// A thorough forger also moves the head row. The log is then internally
	// consistent and opens; only the outside witness can tell.
	rewriteFrom(t, logPath, 2, true)

	reopened, err := Open(logPath)
	if err != nil {
		t.Fatalf("an internally consistent log should open: %v", err)
	}
	defer reopened.Close()
	anchor, err := OpenAnchor(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Check(reopened); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Fatalf("the anchor must catch rewritten witnessed history, got %v", err)
	}
}

func TestVerifyAcceptsEmptyAndGrowingLogs(t *testing.T) {
	store, _ := appendedStore(t, 0)
	defer store.Close()
	if err := store.Verify(); err != nil {
		t.Fatalf("an empty log must verify: %v", err)
	}
	for index := 0; index < 3; index++ {
		if err := store.Append(testEvent(store.NextSequence(), "event")); err != nil {
			t.Fatal(err)
		}
		if err := store.Verify(); err != nil {
			t.Fatalf("verify after append %d: %v", index+1, err)
		}
	}
}
