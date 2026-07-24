package eventlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func TestFilePersistsAndReplaysHashChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		event := control.Event{
			Sequence: uint64(i), At: time.Unix(int64(i), 0).UTC(),
			Type: control.EventGoalAccepted, Actor: "test", GoalID: "goal", Message: "event",
		}
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	records := reopened.Records()
	if len(records) != 2 || records[1].PreviousHash != records[0].Hash {
		t.Fatalf("invalid replayed chain: %+v", records)
	}
}

func TestFileRejectsTamperedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	event := control.Event{
		Sequence: 1, At: time.Unix(1, 0).UTC(), Type: control.EventGoalAccepted,
		Actor: "test", GoalID: "goal", Message: "before",
	}
	if err := store.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "before", "after", 1))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "invalid hash") {
		t.Fatalf("expected tamper error, got %v", err)
	}
}
