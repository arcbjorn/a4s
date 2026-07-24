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

// appendedLog writes a small log and returns its path and record count.
func appendedLog(t *testing.T, events int) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.log")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= events; i++ {
		if err := store.Append(control.Event{
			Sequence: uint64(i), Type: control.EventGoalAccepted,
			GoalID: "web-public", Message: "accepted", At: time.Unix(int64(i), 0).UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, events
}

// A record is appended and fsynced as one write, but a machine that loses power
// mid-write leaves a partial final line. Refusing to open the log because of it
// would mean one badly-timed power loss stops the control plane from starting,
// with every prior record intact.
func TestTornFinalWriteRecovers(t *testing.T) {
	path, count := appendedLog(t, 4)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the file mid-record, exactly as an interrupted append would.
	if err := os.WriteFile(path, raw[:len(raw)-30], 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("a torn final write made the log unopenable: %v", err)
	}
	defer store.Close()

	if got := len(store.Records()); got != count-1 {
		t.Fatalf("expected the %d complete records to survive, got %d", count-1, got)
	}
	if store.Truncated() != count {
		t.Fatalf("expected the truncation to be reported, got line %d", store.Truncated())
	}
}

// Recovery must leave the log writable, or a node recovers from a crash and
// then cannot record anything that happens next.
func TestRecoveredLogAcceptsNewRecords(t *testing.T) {
	path, count := appendedLog(t, 3)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-20], 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(control.Event{
		Sequence: uint64(count), Type: control.EventGoalAchieved,
		GoalID: "web-public", Message: "achieved",
	}); err != nil {
		t.Fatalf("recovered log refused a new record: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The chain must still verify from disk after recovery plus a fresh append.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("log written after recovery does not replay: %v", err)
	}
	defer reopened.Close()
	if got := len(reopened.Records()); got != count {
		t.Fatalf("expected %d records after recovery and append, got %d", count, got)
	}
	if reopened.Truncated() != 0 {
		t.Fatal("expected a clean log to report no truncation")
	}
}

// A trailing blank line comes from the same interrupted-write cause and must
// not brick the log either.
func TestTrailingBlankLineRecovers(t *testing.T) {
	path, count := appendedLog(t, 3)
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("a trailing blank line made the log unopenable: %v", err)
	}
	defer store.Close()
	if got := len(store.Records()); got != count {
		t.Fatalf("expected all %d records to survive, got %d", count, got)
	}
}

// This is the property that keeps recovery from becoming a way to erase
// history: a corrupt record with valid records after it cannot be explained by
// an interrupted append, so it must stay fatal.
func TestMidFileCorruptionStaysFatal(t *testing.T) {
	path, _ := appendedLog(t, 4)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	for _, damage := range []struct {
		name  string
		apply func([]string) []string
	}{
		{"unparseable middle record", func(l []string) []string {
			l[1] = "{not json"
			return l
		}},
		{"tampered middle record", func(l []string) []string {
			l[1] = strings.Replace(l[1], "accepted", "tampered", 1)
			return l
		}},
		{"deleted middle record", func(l []string) []string {
			return append(append([]string{}, l[:1]...), l[2:]...)
		}},
	} {
		t.Run(damage.name, func(t *testing.T) {
			damaged := damage.apply(append([]string{}, lines...))
			broken := filepath.Join(t.TempDir(), "events.log")
			if err := os.WriteFile(broken,
				[]byte(strings.Join(damaged, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(broken); err == nil {
				store.Close()
				t.Fatalf("%s was silently accepted", damage.name)
			}
		})
	}
}

// An empty log is the normal first-start case, not a crash artifact.
func TestEmptyLogOpensCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("an empty log should open: %v", err)
	}
	defer store.Close()
	if len(store.Records()) != 0 || store.Truncated() != 0 {
		t.Fatal("expected an empty log to report no records and no truncation")
	}
}

// FuzzReplay checks that opening an arbitrary file either fails or yields a log
// whose chain verifies.
//
// The log is the only authoritative state in a4s. A file that opens but whose
// records do not chain would let a tampered history look authoritative, which
// is worse than refusing to open: the control plane would act on it.
//
// This target writes a real file per iteration, so it runs orders of magnitude
// slower than the in-memory targets in the control package. That is inherent to
// what it exercises — replay reads from disk — and it is kept because the
// property is worth checking, not because the throughput is good. Give it a
// longer -fuzztime than the kernel targets.
func FuzzReplay(f *testing.F) {
	path, _ := appendedLog(&testing.T{}, 3)
	if genuine, err := os.ReadFile(path); err == nil {
		f.Add(genuine)
	}
	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Add([]byte("{not json"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		target := filepath.Join(t.TempDir(), "events.log")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Skip()
		}
		store, err := Open(target)
		if err != nil {
			return
		}
		defer store.Close()

		// Whatever survived must be a verifiable chain from the start.
		previous := ""
		for i, record := range store.Records() {
			if record.Sequence != uint64(i+1) {
				t.Fatalf("record %d has sequence %d", i, record.Sequence)
			}
			if record.PreviousHash != previous {
				t.Fatalf("record %d breaks the chain", i)
			}
			hash, hashErr := eventHash(previous, record.Event)
			if hashErr != nil {
				t.Fatalf("record %d cannot be hashed: %v", i, hashErr)
			}
			if hash != record.Hash {
				t.Fatalf("record %d has an invalid hash", i)
			}
			previous = record.Hash
		}
	})
}
