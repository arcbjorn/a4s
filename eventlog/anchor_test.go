package eventlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anchoredStore builds a store with a witnessed head, returning both.
func anchoredStore(t *testing.T, count int) (*File, *Anchor, string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.db")
	anchorPath := filepath.Join(dir, "anchor.jsonl")

	store, err := Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= count; index++ {
		if err := store.Append(testEvent(store.NextSequence(),
			fmt.Sprintf("event %d", index))); err != nil {
			t.Fatal(err)
		}
	}
	anchor, err := OpenAnchor(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	head := store.Head()
	if err := anchor.Witness(head.Sequence, head.Hash); err != nil {
		t.Fatal(err)
	}
	return store, anchor, logPath, anchorPath
}

func TestAnchorAcceptsTheStoreItWitnessed(t *testing.T) {
	store, anchor, _, _ := anchoredStore(t, 3)
	defer store.Close()

	if err := anchor.Check(store); err != nil {
		t.Fatalf("the witnessed store was refused: %v", err)
	}
}

// A store that keeps appending after the last witness is normal, not an attack.
func TestAnchorAcceptsAStoreThatGrew(t *testing.T) {
	store, anchor, _, _ := anchoredStore(t, 3)
	defer store.Close()

	for index := 4; index <= 6; index++ {
		if err := store.Append(testEvent(store.NextSequence(),
			fmt.Sprintf("event %d", index))); err != nil {
			t.Fatal(err)
		}
	}
	if err := anchor.Check(store); err != nil {
		t.Fatalf("a store that grew past its witness was refused: %v", err)
	}
}

// The attack the anchor exists for: a forged store whose own chain verifies
// perfectly, because the attacker built it from scratch.
func TestAnchorDetectsWholesaleReplacement(t *testing.T) {
	store, _, logPath, anchorPath := anchoredStore(t, 3)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Replace the store with a different, internally consistent one.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	forged, err := Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer forged.Close()
	for index := 1; index <= 3; index++ {
		if err := forged.Append(testEvent(forged.NextSequence(),
			fmt.Sprintf("forged %d", index))); err != nil {
			t.Fatal(err)
		}
	}
	// The forgery passes its own integrity check, which is exactly the problem.
	if err := forged.Verify(); err != nil {
		t.Fatalf("the forged chain should verify against itself: %v", err)
	}

	reopened, err := OpenAnchor(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	err = reopened.Check(forged)
	if err == nil {
		t.Fatal("a replaced event log passed the anchor check")
	}
	if !strings.Contains(err.Error(), "rewritten") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A truncated or rolled-back store is behind its witness.
func TestAnchorDetectsTruncation(t *testing.T) {
	store, _, logPath, anchorPath := anchoredStore(t, 5)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	shorter, err := Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer shorter.Close()
	for index := 1; index <= 2; index++ {
		if err := shorter.Append(testEvent(shorter.NextSequence(),
			fmt.Sprintf("event %d", index))); err != nil {
			t.Fatal(err)
		}
	}

	anchor, err := OpenAnchor(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	err = anchor.Check(shorter)
	if err == nil {
		t.Fatal("a truncated event log passed the anchor check")
	}
	if !strings.Contains(err.Error(), "truncated or replaced") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The anchor must not be talked backwards, or a rolled-back store could
// re-witness itself into looking current.
func TestAnchorRefusesGoingBackwards(t *testing.T) {
	store, anchor, _, _ := anchoredStore(t, 5)
	defer store.Close()

	if err := anchor.Witness(2, "aaaa"); err == nil {
		t.Fatal("the anchor accepted a lower sequence")
	}
	// Re-witnessing the same head is a no-op rather than an error, so a restart
	// that anchors again does not fail.
	head := store.Head()
	if err := anchor.Witness(head.Sequence, head.Hash); err != nil {
		t.Fatalf("re-witnessing the same head failed: %v", err)
	}
	// The same sequence with a different hash is a contradiction.
	if err := anchor.Witness(head.Sequence, "bbbb"); err == nil {
		t.Fatal("the anchor accepted a conflicting hash at a witnessed sequence")
	}
}

func TestAnchorSurvivesReopen(t *testing.T) {
	store, _, _, anchorPath := anchoredStore(t, 4)
	defer store.Close()

	reopened, err := OpenAnchor(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reopened.Last().Sequence, store.Head().Sequence; got != want {
		t.Fatalf("reopened anchor is at %d, want %d", got, want)
	}
	if reopened.Last().Hash != store.Head().Hash {
		t.Fatal("reopened anchor lost the witnessed hash")
	}
	if err := reopened.Check(store); err != nil {
		t.Fatalf("reopened anchor refused its own store: %v", err)
	}
}

// An unreadable anchor is fatal rather than treated as empty, since silently
// ignoring it would discard the protection without telling anyone.
func TestMalformedAnchorIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchor.jsonl")
	if err := os.WriteFile(path, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAnchor(path); err == nil {
		t.Fatal("a malformed anchor opened cleanly")
	}
}

func TestAnchorWithoutWitnessAcceptsAnyStore(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append(testEvent(store.NextSequence(), "first")); err != nil {
		t.Fatal(err)
	}

	anchor, err := OpenAnchor(filepath.Join(dir, "anchor.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing witnessed yet, so there is nothing to contradict.
	if err := anchor.Check(store); err != nil {
		t.Fatalf("an unwitnessed anchor refused a store: %v", err)
	}
}

func TestAnchorRequiresAbsolutePath(t *testing.T) {
	if _, err := OpenAnchor("relative/anchor.jsonl"); err == nil {
		t.Fatal("a relative anchor path was accepted")
	}
}
