package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/obs"
)

// operatorAPI builds a server with one enrolled operator key and its API.
func operatorAPI(t *testing.T) (*API, ed25519.PrivateKey) {
	t.Helper()
	server, _, key := operatorServer(t)
	return NewAPI(server, APIConfig{Metrics: obs.NewMetrics()}), key
}

// call signs and issues a request the way the operator CLI would.
func call(t *testing.T, api *API, key ed25519.PrivateKey,
	method, path string, body any) *httptest.ResponseRecorder {

	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	}
	now := time.Now().UTC()
	signed, err := SignRequest(RequestEnvelope{
		Nonce: fmt.Sprintf("nonce-%d", time.Now().UnixNano()), Method: method, Path: path,
		BodyDigest: BodyDigest(payload), IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	return issue(t, api, signed, method, path, payload)
}

func issue(t *testing.T, api *API, signed SignedRequest,
	method, path string, payload []byte) *httptest.ResponseRecorder {

	t.Helper()
	header, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set(SignatureHeader, string(header))
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestAPIRejectsUnsignedRequest(t *testing.T) {
	api, _ := operatorAPI(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned request = %d, want 401", recorder.Code)
	}
}

func TestAPIAcceptsSignedRead(t *testing.T) {
	api, key := operatorAPI(t)
	recorder := call(t, api, key, http.MethodGet, "/v1/status", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("signed read = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var status Status
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
}

// A signature authorizes one call. Replaying it against a different endpoint is
// the substitution that binding method and path into the envelope prevents.
func TestAPIRefusesSignatureReusedOnAnotherPath(t *testing.T) {
	api, key := operatorAPI(t)
	now := time.Now().UTC()
	signed, err := SignRequest(RequestEnvelope{
		Nonce: "n1", Method: http.MethodGet, Path: "/v1/status", IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	recorder := issue(t, api, signed, http.MethodGet, "/v1/world", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("cross-path replay = %d, want 401", recorder.Code)
	}
}

// The same request replayed verbatim must be refused the second time, or a
// captured request would remain usable for its whole validity window.
func TestAPIRefusesReplayedNonce(t *testing.T) {
	api, key := operatorAPI(t)
	now := time.Now().UTC()
	signed, err := SignRequest(RequestEnvelope{
		Nonce: "replay-me", Method: http.MethodGet, Path: "/v1/status", IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	if first := issue(t, api, signed, http.MethodGet, "/v1/status", nil); first.Code != http.StatusOK {
		t.Fatalf("first use = %d, want 200", first.Code)
	}
	second := issue(t, api, signed, http.MethodGet, "/v1/status", nil)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("replay = %d, want 401", second.Code)
	}
}

// Swapping the body under a valid signature must be refused, or the digest
// binding would be decorative.
func TestAPIRefusesTamperedBody(t *testing.T) {
	api, key := operatorAPI(t)
	goal := testGoal()
	encoded, err := json.Marshal(goal)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	signed, err := SignRequest(RequestEnvelope{
		Nonce: "n2", Method: http.MethodPost, Path: "/v1/goals",
		BodyDigest: BodyDigest(encoded), IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Replace(encoded, []byte(goal.ID), []byte("other-goal"), 1)
	recorder := issue(t, api, signed, http.MethodPost, "/v1/goals", tampered)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body = %d, want 401", recorder.Code)
	}
}

func TestAPIRefusesUnknownKey(t *testing.T) {
	api, _ := operatorAPI(t)
	_, stranger, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := call(t, api, stranger, http.MethodGet, "/v1/status", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d, want 401", recorder.Code)
	}
}

func TestAPIRefusesExpiredRequest(t *testing.T) {
	api, key := operatorAPI(t)
	past := time.Now().UTC().Add(-10 * time.Minute)
	signed, err := SignRequest(RequestEnvelope{
		Nonce: "old", Method: http.MethodGet, Path: "/v1/status", IssuedBy: "arc",
		IssuedAt: past, ExpiresAt: past.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	recorder := issue(t, api, signed, http.MethodGet, "/v1/status", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired request = %d, want 401", recorder.Code)
	}
}

// A goal submitted through the API must reach the server, which is the claim
// that an authenticated operator can deploy without a scenario file.
func TestAPISubmitsGoal(t *testing.T) {
	api, key := operatorAPI(t)
	recorder := call(t, api, key, http.MethodPost, "/v1/goals", testGoal())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("submit = %d, want 201: %s", recorder.Code, recorder.Body)
	}
	if len(api.server.Goals()) != 1 {
		t.Fatalf("goal was not accepted: %d goals", len(api.server.Goals()))
	}
}

func TestAPIRefusesInvalidGoal(t *testing.T) {
	api, key := operatorAPI(t)
	goal := testGoal()
	goal.ID = "Not A Valid Id"
	recorder := call(t, api, key, http.MethodPost, "/v1/goals", goal)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid goal = %d, want 422", recorder.Code)
	}
}

// An approval arriving over the API carries its own operator signature. The
// request signature proves who called; the grant signature is the durable
// record of the decision.
func TestAPIRecordsApproval(t *testing.T) {
	api, key := operatorAPI(t)
	if err := api.server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	recorder := call(t, api, key, http.MethodPost, "/v1/approvals",
		signedGrant(t, key, "public-route"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("approve = %d, want 201: %s", recorder.Code, recorder.Body)
	}

	var grant control.ApprovalGrant
	if err := json.Unmarshal(recorder.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if !api.server.World().Approvals[grant.ID].Valid(time.Now()) {
		t.Fatal("approval did not authorize in the world")
	}
}

// An approval signed by a key the server does not know must be refused even
// when the request itself is properly signed, because the two signatures answer
// different questions.
func TestAPIRefusesApprovalFromUnknownSigner(t *testing.T) {
	api, key := operatorAPI(t)
	if err := api.server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	_, stranger, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	forged, err := control.SignApproval(control.ApprovalGrant{
		ID: "forged", GoalID: "web-public", Scope: "public-route", IssuedBy: "mallory",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}, "operator-arc", stranger)
	if err != nil {
		t.Fatal(err)
	}
	recorder := call(t, api, key, http.MethodPost, "/v1/approvals", forged)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("forged approval = %d, want 422", recorder.Code)
	}
}

// An oversized body must be refused before it is buffered, or an unauthenticated
// caller could exhaust the control plane's memory.
func TestAPIRefusesOversizedBody(t *testing.T) {
	api, key := operatorAPI(t)
	huge := bytes.Repeat([]byte("a"), MaxRequestBody+1024)
	now := time.Now().UTC()
	signed, err := SignRequest(RequestEnvelope{
		Nonce: "big", Method: http.MethodPost, Path: "/v1/goals",
		BodyDigest: BodyDigest(huge), IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	recorder := issue(t, api, signed, http.MethodPost, "/v1/goals", huge)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", recorder.Code)
	}
}

// Health must answer without a signature so a supervisor can check liveness,
// and must not disclose cluster state.
func TestAPIHealthIsUnauthenticated(t *testing.T) {
	api, _ := operatorAPI(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200", recorder.Code)
	}
	for _, leaked := range []string{"revision", "allocations", "nodes"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("health disclosed %q: %s", leaked, recorder.Body)
		}
	}
}

func TestAPIMetricsRequireAuthentication(t *testing.T) {
	api, _ := operatorAPI(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned metrics = %d, want 401", recorder.Code)
	}
}

func TestAPIMetricsReportWorldShape(t *testing.T) {
	api, key := operatorAPI(t)
	recorder := call(t, api, key, http.MethodGet, "/v1/metrics", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"a4s_world_revision", "a4s_nodes", "a4s_uptime_seconds"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("metrics missing %q: %s", want, recorder.Body)
		}
	}
}

// Reads describe the whole cluster and must not be readable without a key.
func TestAPIReadsRequireAuthentication(t *testing.T) {
	api, _ := operatorAPI(t)
	for _, path := range []string{
		"/v1/status", "/v1/world", "/v1/goals", "/v1/events",
		"/v1/approvals", "/v1/directory", "/v1/routes",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		api.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated = %d, want 401", path, recorder.Code)
		}
	}
}

// The window filters bound both ends, and a malformed timestamp is refused
// rather than silently ignored, which would widen the answer without saying so.
func TestAPIEventsBoundTheWindow(t *testing.T) {
	api, key := operatorAPI(t)
	if err := api.server.Submit(testGoal()); err != nil {
		t.Fatal(err)
	}
	if _, err := api.server.Approve(signedGrant(t, key, "public-route")); err != nil {
		t.Fatal(err)
	}
	all := api.server.Query(HistoryQuery{})
	if len(all) == 0 {
		t.Fatal("expected the recorded approval to appear in history")
	}
	latest := all[len(all)-1].At.UTC()

	recorder := signedGet(t, api, key, "/v1/events",
		"until="+latest.Add(-time.Nanosecond).Format(time.RFC3339Nano))
	if recorder.Code != http.StatusOK {
		t.Fatalf("until = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var bounded []control.Event
	if err := json.Unmarshal(recorder.Body.Bytes(), &bounded); err != nil {
		t.Fatal(err)
	}
	if len(bounded) >= len(all) {
		t.Fatalf("expected the upper bound to narrow history, got %d of %d", len(bounded), len(all))
	}

	for _, bad := range []string{"since=yesterday", "until=yesterday"} {
		recorder := signedGet(t, api, key, "/v1/events", bad)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", bad, recorder.Code)
		}
	}
}

// signedGet issues a read whose signature covers the path only, which is how
// the operator client signs a request that carries query filters.
func signedGet(t *testing.T, api *API, key ed25519.PrivateKey,
	path, query string) *httptest.ResponseRecorder {

	t.Helper()
	now := time.Now().UTC()
	signed, err := SignRequest(RequestEnvelope{
		Nonce:  fmt.Sprintf("nonce-%d", time.Now().UnixNano()),
		Method: http.MethodGet, Path: path, IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}, "operator-arc", key)
	if err != nil {
		t.Fatal(err)
	}
	return issue(t, api, signed, http.MethodGet, path+"?"+query, nil)
}

func TestAPIRefusesUnknownBodyFields(t *testing.T) {
	api, key := operatorAPI(t)
	body := map[string]any{"id": "web", "unexpected_field": true}
	recorder := call(t, api, key, http.MethodPost, "/v1/goals", body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", recorder.Code)
	}
}
