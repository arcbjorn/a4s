package server

import (
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

// standbyPair builds a primary holding history and an empty follower.
func standbyPair(t *testing.T) (*Server, ed25519.PrivateKey, *Standby, Config) {
	t.Helper()
	dir := t.TempDir()
	primary, _, key := operatorServer(t)
	if err := primary.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Approve(signedGrant(t, key, "public-route")); err != nil {
		t.Fatal(err)
	}

	// Base and operator keys must match the primary. The log carries neither,
	// so a follower configured without them promotes into a server missing node
	// inventory and any approval granted outside recorded history.
	config := Config{
		EventLog:     filepath.Join(dir, "standby.db"),
		Anchor:       filepath.Join(dir, "standby.anchor"),
		Base:         baseWorld(),
		OperatorKeys: primary.OperatorKeys(),
	}
	standby, err := OpenStandby(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { standby.Close() })
	return primary, key, standby, config
}

// An anchor is mandatory: without an outside witness a follower cannot tell
// "caught up" from "missing the last hour".
func TestStandbyRequiresAnAnchor(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenStandby(Config{EventLog: filepath.Join(dir, "log.db")})
	if err == nil || !strings.Contains(err.Error(), "anchor") {
		t.Fatalf("expected an anchor requirement, got %v", err)
	}
}

// The follower derives every hash itself, so agreement means it computed the
// same history rather than faithfully copying whatever it was sent.
func TestStandbyDerivesTheSameChain(t *testing.T) {
	primary, _, standby, _ := standbyPair(t)
	records := primary.History()
	if len(records) == 0 {
		t.Fatal("the primary recorded no history")
	}

	appended, err := standby.Ingest(records)
	if err != nil {
		t.Fatal(err)
	}
	if appended != len(records) {
		t.Fatalf("ingested %d of %d records", appended, len(records))
	}
	if standby.Head().Hash != primary.History()[len(records)-1].Hash {
		t.Fatal("the follower derived a different chain head")
	}
	if behind := standby.Behind(primary.History()[len(records)-1]); behind != 0 {
		t.Fatalf("a caught-up follower reported %d behind", behind)
	}
}

// Re-shipping an overlapping batch must be safe, since a transport that cannot
// be replayed is a transport that cannot recover from a dropped connection.
func TestStandbyIngestIsIdempotent(t *testing.T) {
	primary, _, standby, _ := standbyPair(t)
	records := primary.History()

	if _, err := standby.Ingest(records); err != nil {
		t.Fatal(err)
	}
	appended, err := standby.Ingest(records)
	if err != nil {
		t.Fatalf("re-ingesting the same records failed: %v", err)
	}
	if appended != 0 {
		t.Fatalf("re-ingesting appended %d records", appended)
	}
}

// A record that does not derive the hash the primary recorded means the two
// histories are not the same history, which must be refused rather than stored.
func TestStandbyRefusesDivergentHistory(t *testing.T) {
	primary, _, standby, _ := standbyPair(t)
	records := primary.History()

	forged := make([]eventlog.Record, len(records))
	copy(forged, records)
	forged[0].Event.Message = "something else entirely"

	_, err := standby.Ingest(forged)
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("expected a divergence refusal, got %v", err)
	}
}

// The check that matters: a follower promoted mid-replication would silently
// roll history backwards, resurrecting revoked approvals.
func TestStandbyRefusesPromotionWhenBehind(t *testing.T) {
	primary, _, standby, _ := standbyPair(t)
	records := primary.History()
	if len(records) < 1 {
		t.Fatal("the primary recorded no history")
	}

	// The follower catches up, so its anchor now witnesses the primary's head.
	if _, err := standby.Ingest(records); err != nil {
		t.Fatal(err)
	}
	anchorPath := standby.config.Anchor

	// A second follower with an empty log against that same witness is what a
	// replica restored from an older copy looks like: a perfectly valid chain
	// that simply stops too early. Only the outside witness can tell.
	behind, err := OpenStandby(Config{
		EventLog: filepath.Join(t.TempDir(), "behind.db"),
		Anchor:   anchorPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer behind.Close()

	if _, err := behind.Promote(); err == nil {
		t.Fatal("a follower behind the witnessed head was promoted")
	} else if !strings.Contains(err.Error(), "behind the primary") {
		t.Fatalf("expected a behind-the-primary refusal, got %v", err)
	}
}

// A caught-up follower promotes into an ordinary server that has recovered the
// primary's state, which is the whole point of keeping one.
func TestStandbyPromotesWhenCaughtUp(t *testing.T) {
	primary, _, standby, _ := standbyPair(t)
	if _, err := standby.Ingest(primary.History()); err != nil {
		t.Fatal(err)
	}

	promoted, err := standby.Promote(control.PlacementAgent{})
	if err != nil {
		t.Fatalf("a caught-up follower refused to promote: %v", err)
	}
	defer promoted.Close()

	if got, want := promoted.Status().Events, primary.Status().Events; got != want {
		t.Fatalf("promoted server holds %d events, primary held %d", got, want)
	}
	// The approval the primary recorded must still authorize after promotion.
	if len(promoted.Approvals()) != len(primary.Approvals()) {
		t.Fatalf("promoted server holds %d approvals, primary held %d",
			len(promoted.Approvals()), len(primary.Approvals()))
	}
	// Promotion consumes the standby, so a second one must be refused rather
	// than opening a second writer on one log.
	if _, err := standby.Promote(); err == nil {
		t.Fatal("a consumed standby promoted twice")
	}
}
