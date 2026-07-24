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
