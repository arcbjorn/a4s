package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// The planned-maintenance path: nothing is wrong with the node yet, so nothing
// will observe a reason to stop scheduling onto it.
func TestOperatorCordonsANode(t *testing.T) {
	server, _, _ := operatorServer(t)
	if err := server.Cordon("base", "replacing a disk", "arc"); err != nil {
		t.Fatal(err)
	}
	node := server.World().Nodes["base"]
	if !node.Cordoned {
		t.Fatal("the node was not cordoned")
	}
	if node.CordonReason != "replacing a disk" {
		t.Fatalf("cordon reason = %q, want the operator's words", node.CordonReason)
	}
	if node.Schedulable() {
		t.Fatal("a cordoned node is still schedulable")
	}

	if err := server.Uncordon("base", "arc"); err != nil {
		t.Fatal(err)
	}
	if server.World().Nodes["base"].Cordoned {
		t.Fatal("uncordon did not return the node to service")
	}
}

// A cordon that vanished on restart would silently return a machine an operator
// is working on to the scheduler.
func TestCordonSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/events.log"
	first := openServer(t, path)
	if err := first.Cordon("base", "maintenance", "arc"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openServer(t, path)
	defer restarted.Close()
	node := restarted.World().Nodes["base"]
	if !node.Cordoned || node.CordonReason != "maintenance" {
		t.Fatalf("the cordon did not survive a restart: %+v", node)
	}
}

// A typo must be a refusal, not a cordon on nothing that leaves an operator
// believing a machine is out of service.
func TestCordonRefusesAnUnknownNode(t *testing.T) {
	server, _, _ := operatorServer(t)
	if err := server.Cordon("nonexistent", "oops", "arc"); err == nil {
		t.Fatal("cordoning an unknown node was accepted")
	}
	if err := server.Cordon("", "", "arc"); err == nil {
		t.Fatal("cordoning an unnamed node was accepted")
	}
	if err := server.Cordon("base", "", ""); err == nil {
		t.Fatal("an unattributed cordon was accepted")
	}
}

// The decision belongs in durable history, attributed to whoever made it.
func TestCordonIsRecordedAgainstTheOperator(t *testing.T) {
	server, _, _ := operatorServer(t)
	if err := server.Cordon("base", "replacing a disk", "arc"); err != nil {
		t.Fatal(err)
	}
	events := server.Query(HistoryQuery{Kind: control.EvidenceNodeCordoned})
	if len(events) != 1 {
		t.Fatalf("expected one cordon event, got %d", len(events))
	}
	if events[0].Actor != "operator:arc" {
		t.Fatalf("cordon attributed to %q, want operator:arc", events[0].Actor)
	}
	if !strings.Contains(events[0].Message, "replacing a disk") {
		t.Fatalf("the reason did not reach history: %q", events[0].Message)
	}
}

// Evacuation answers the question an operator actually asks next.
func TestEvacuationReportsWhatMustMove(t *testing.T) {
	server, _, _ := operatorServer(t)
	if got := server.Evacuation("base"); !got.Empty() {
		t.Fatalf("an idle node reported work: %v", got.Allocations)
	}
}

// Over the API the operator comes from the verified envelope, so a caller
// cannot attribute the decision to somebody else.
func TestAPICordonAttributesToTheSigner(t *testing.T) {
	api, key := operatorAPI(t)
	recorder := call(t, api, key, http.MethodPost, "/v1/nodes/base/cordon",
		map[string]string{"reason": "disk"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("cordon = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var evacuation control.NodeEvacuation
	if err := json.Unmarshal(recorder.Body.Bytes(), &evacuation); err != nil {
		t.Fatal(err)
	}
	if evacuation.Node != "base" {
		t.Fatalf("evacuation named %q, want base", evacuation.Node)
	}
	if !api.server.World().Nodes["base"].Cordoned {
		t.Fatal("the API cordon did not take effect")
	}
	events := api.server.Query(HistoryQuery{Kind: control.EvidenceNodeCordoned})
	if len(events) != 1 || events[0].Actor != "operator:arc" {
		t.Fatalf("cordon was not attributed to the signer: %+v", events)
	}

	uncordon := call(t, api, key, http.MethodPost, "/v1/nodes/base/uncordon", nil)
	if uncordon.Code != http.StatusOK {
		t.Fatalf("uncordon = %d, want 200: %s", uncordon.Code, uncordon.Body)
	}
	if api.server.World().Nodes["base"].Cordoned {
		t.Fatal("the API uncordon did not take effect")
	}
}

// Taking a node out of service is not something an unauthenticated caller may
// do, and a malformed body is refused rather than cordoning with no reason.
func TestAPICordonRequiresAuthenticationAndAWellFormedBody(t *testing.T) {
	api, key := operatorAPI(t)
	for _, path := range []string{"/v1/nodes/base/cordon", "/v1/nodes/base/uncordon"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		recorder := httptest.NewRecorder()
		api.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated = %d, want 401", path, recorder.Code)
		}
	}
	bad := call(t, api, key, http.MethodPost, "/v1/nodes/base/cordon",
		map[string]any{"unexpected": true})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field = %d, want 400", bad.Code)
	}
	missing := call(t, api, key, http.MethodPost, "/v1/nodes/ghost/cordon", nil)
	if missing.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown node = %d, want 422", missing.Code)
	}
}
