package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func TestFileLedgerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := OpenFileLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	want := DispatchResult{EnvelopeDigest: "digest", Evidence: testEvidence()}
	if err := ledger.Put("action-1", want); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, ok := reopened.Get("action-1")
	if !ok || got.EnvelopeDigest != want.EnvelopeDigest || got.Evidence.Kind != want.Evidence.Kind {
		t.Fatalf("unexpected replayed result: ok=%t result=%+v", ok, got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger permissions: info=%v err=%v", info, err)
	}
}

func TestFileLedgerRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileLedger(path); err == nil {
		t.Fatal("expected corrupt ledger to be rejected")
	}
}

func testEvidence() control.Evidence {
	return control.Evidence{Kind: "allocation.running", Target: "web-0"}
}

// A crash mid-append leaves a final line with no newline. It was never
// acknowledged, so the node must open past it rather than refuse to start.
func TestLedgerRecoversFromTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := OpenFileLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Put("k1", DispatchResult{EnvelopeDigest: "d1", Evidence: testEvidence()}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"key":"k2","result":{"envelope_dig`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileLedger(path)
	if err != nil {
		t.Fatalf("a crash mid-append left the node unable to open its ledger: %v", err)
	}
	if _, ok := reopened.Get("k1"); !ok {
		t.Fatal("the acknowledged record was lost")
	}
	if _, ok := reopened.Get("k2"); ok {
		t.Fatal("the torn record was treated as stored")
	}
	// Appending after recovery must produce a clean file, not one with the
	// fragment glued to the next record.
	if err := reopened.Put("k3", DispatchResult{EnvelopeDigest: "d3", Evidence: testEvidence()}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenFileLedger(path)
	if err != nil {
		t.Fatalf("the ledger did not reopen cleanly after recovery: %v", err)
	}
	defer again.Close()
	for _, key := range []string{"k1", "k3"} {
		if _, ok := again.Get(key); !ok {
			t.Fatalf("record %s missing after recovery and reopen", key)
		}
	}
}

// A complete line that does not decode is corruption, not a torn append, and
// must still refuse to open.
func TestLedgerRefusesCorruptMiddleLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	content := "{not json}\n" + `{"key":"k1","result":{"envelope_digest":"d1"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if ledger, err := OpenFileLedger(path); err == nil {
		ledger.Close()
		t.Fatal("a ledger with a corrupt complete line was opened")
	}
}
