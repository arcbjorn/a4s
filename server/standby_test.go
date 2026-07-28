package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

// standbyPair builds a primary holding history and an empty follower.
//
// Both use one anchor, because that is the only arrangement in which the anchor
// answers the question promotion turns on. A replica-local anchor would witness
// whatever the replica last ingested and agree with itself.
func standbyPair(t *testing.T) (*Server, ed25519.PrivateKey, *Standby, Config) {
	t.Helper()
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.anchor")

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"operator-arc": public}
	primary, err := Open(Config{
		EventLog: filepath.Join(dir, "primary.db"), Anchor: shared,
		Base: baseWorld(), OperatorKeys: keys,
	}, control.PlacementAgent{}, control.NetworkAgent{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { primary.Close() })

	if err := primary.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Approve(signedGrant(t, private, "public-route")); err != nil {
		t.Fatal(err)
	}

	// Base and operator keys must match the primary. The log carries neither,
	// so a follower configured without them promotes into a server missing node
	// inventory and any approval granted outside recorded history.
	config := Config{
		EventLog: filepath.Join(dir, "standby.db"), Anchor: shared,
		Base: baseWorld(), OperatorKeys: keys,
	}
	standby, err := OpenStandby(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { standby.Close() })
	return primary, private, standby, config
}

// A follower needs hashes, not just events. Re-deriving them against its own
// chain is what makes agreement mean it computed the same history.
func TestRecordsCarryHashesAndPaginate(t *testing.T) {
	primary, _, _, _ := standbyPair(t)
	all := primary.Records(0, 0)
	if len(all) == 0 {
		t.Fatal("the primary served no records")
	}
	for _, record := range all {
		if record.Hash == "" {
			t.Fatalf("record %d carried no hash", record.Sequence)
		}
	}

	// Batching, so a long log is a loop rather than one transfer the follower's
	// bounded reader would truncate mid-record.
	first := primary.Records(0, 1)
	if len(first) != 1 || first[0].Sequence != 1 {
		t.Fatalf("limited batch = %v", first)
	}
	rest := primary.Records(1, 0)
	if len(rest) != len(all)-1 {
		t.Fatalf("after=1 returned %d records, want %d", len(rest), len(all)-1)
	}
	if got := primary.Records(uint64(len(all)), 0); len(got) != 0 {
		t.Fatalf("a caught-up follower was sent %d records", len(got))
	}
	// An oversized limit is clamped rather than honoured, so one request cannot
	// produce a response the follower will not read whole.
	if got := primary.Records(0, MaxRecordBatch*10); len(got) != len(all) {
		t.Fatalf("clamped batch = %d, want %d", len(got), len(all))
	}
}

// Replication over the operator API is the transport a standby actually uses,
// and it is authenticated for a stronger reason than most reads: this is the
// entire history of the cluster.
func TestAPIServesRecordsToAFollower(t *testing.T) {
	api, key := operatorAPI(t)
	if err := api.server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	if _, err := api.server.Approve(signedGrant(t, key, "public-route")); err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRequest(http.MethodGet, "/v1/records", nil)
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, unauthenticated)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated records = %d, want 401", recorder.Code)
	}

	answer := signedGet(t, api, key, "/v1/records", "after=0")
	if answer.Code != http.StatusOK {
		t.Fatalf("records = %d, want 200: %s", answer.Code, answer.Body)
	}
	var records []eventlog.Record
	if err := json.Unmarshal(answer.Body.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 || records[0].Hash == "" {
		t.Fatalf("the API served %d records, first hash %q", len(records), records[0].Hash)
	}

	// A follower fed straight from the endpoint must derive the same chain.
	follower, err := OpenStandby(Config{
		EventLog: filepath.Join(t.TempDir(), "replica.db"),
		Anchor:   filepath.Join(t.TempDir(), "replica.anchor"),
		Base:     baseWorld(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	if _, err := follower.Ingest(records); err != nil {
		t.Fatalf("records from the API did not derive: %v", err)
	}
	if follower.Head().Hash != records[len(records)-1].Hash {
		t.Fatal("the follower derived a different head from the served records")
	}

	if bad := signedGet(t, api, key, "/v1/records", "after=nonsense"); bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed after = %d, want 400", bad.Code)
	}
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

	// The follower has replicated nothing, so the primary's witness is ahead of
	// it. That is a perfectly valid chain which simply stops too early, and
	// nothing inside the replica can tell.
	if standby.Head().Sequence >= uint64(len(records)) {
		t.Fatal("the follower was already caught up; the test proves nothing")
	}
	if _, err := standby.Promote(); err == nil {
		t.Fatal("a follower behind the witnessed head was promoted")
	} else if !strings.Contains(err.Error(), "behind the primary") {
		t.Fatalf("expected a behind-the-primary refusal, got %v", err)
	}

	// Partial replication is still behind, which is the case a promotion during
	// catch-up actually looks like.
	partial, err := OpenStandby(Config{
		EventLog: filepath.Join(t.TempDir(), "partial.db"),
		Anchor:   standby.config.Anchor, Base: baseWorld(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	if _, err := partial.Ingest(records[:len(records)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := partial.Promote(); err == nil {
		t.Fatal("a partially replicated follower was promoted")
	}
}

// A replica-local anchor witnesses whatever the replica last ingested, so it
// agrees with itself and protects nothing. The shared witness is what makes the
// refusal above possible, and this pins why.
func TestReplicaLocalAnchorWouldNotDetectStaleness(t *testing.T) {
	primary, _, _, _ := standbyPair(t)
	records := primary.History()

	own, err := OpenStandby(Config{
		EventLog: filepath.Join(t.TempDir(), "own.db"),
		Anchor:   filepath.Join(t.TempDir(), "own.anchor"),
		Base:     baseWorld(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if _, err := own.Ingest(records[:1]); err != nil {
		t.Fatal(err)
	}
	// Ingesting must not write the anchor. If it did, this replica would now
	// witness its own partial head and consider itself promotable.
	if witnessed := own.anchor.Last(); witnessed.Sequence != 0 {
		t.Fatalf("the follower wrote its own anchor at sequence %d", witnessed.Sequence)
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
