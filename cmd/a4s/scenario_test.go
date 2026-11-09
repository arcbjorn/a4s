package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func TestValidateAcceptsEveryExample(t *testing.T) {
	examples, err := filepath.Glob(example("*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Fatal("no example scenarios found")
	}
	for _, path := range examples {
		t.Run(filepath.Base(path), func(t *testing.T) {
			stdout := mustRun(t, "validate", "--file", path)
			if !strings.HasPrefix(stdout, "valid goal ") {
				t.Fatalf("unexpected validate output %q", stdout)
			}
		})
	}
}

func TestValidateRejectsUnknownField(t *testing.T) {
	raw, err := os.ReadFile(example("web-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A misspelled field silently ignored is how a constraint an operator
	// thought they set goes missing, so decoding must be strict.
	tampered := strings.Replace(string(raw), `"goal": {`, `"goal": {"replicaz": 3,`, 1)
	path := writeFile(t, filepath.Join(t.TempDir(), "scenario.json"), tampered)
	_, _, err = runCLI(t, "validate", "--file", path)
	if err == nil || !strings.Contains(err.Error(), "decode scenario") {
		t.Fatalf("want a strict-decode error, got %v", err)
	}
}

func TestValidateRejectsInvalidScenario(t *testing.T) {
	raw, err := os.ReadFile(example("web-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	// An unpinned image must never reach the kernel.
	tampered := strings.Replace(string(raw),
		"@sha256:0000000000000000000000000000000000000000000000000000000000000000", ":latest", 1)
	path := writeFile(t, filepath.Join(t.TempDir(), "scenario.json"), tampered)
	_, _, err = runCLI(t, "validate", "--file", path)
	if err == nil || !strings.Contains(err.Error(), "validate scenario") {
		t.Fatalf("want a validation error, got %v", err)
	}
}

func TestValidateReportsMissingFile(t *testing.T) {
	_, _, err := runCLI(t, "validate", "--file", filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || !strings.Contains(err.Error(), "read scenario") {
		t.Fatalf("want a read error, got %v", err)
	}
}

func TestSimulateConvergesWebService(t *testing.T) {
	stdout := mustRun(t, "simulate", "--file", example("web-service.json"))
	for _, want := range []string{"goal.accepted", "proposal.approved", "publish_route", "goal.achieved",
		"world revision"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("simulation output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestSimulateJSONEmitsOneEventPerLine(t *testing.T) {
	stdout := mustRun(t, "simulate", "--json", "--file", example("web-service.json"))
	var events []control.Event
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var event control.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("line is not an event: %v\n%s", err, scanner.Text())
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			t.Fatalf("event %d has sequence %d; sequences must be dense", i, event.Sequence)
		}
	}
	if last := events[len(events)-1]; last.Type != control.EventGoalAchieved {
		t.Fatalf("last event is %s, want %s", last.Type, control.EventGoalAchieved)
	}
}

func TestSimulateFailsWhenRoundsRunOut(t *testing.T) {
	_, _, err := runCLI(t, "simulate", "--max-rounds", "1", "--file", example("web-service.json"))
	if err == nil {
		t.Fatal("a goal that cannot converge within the round budget must fail")
	}
}

// recordedLog runs the web-service simulation into a durable event log and
// returns its path, so read-only commands have real history to inspect.
func recordedLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.db")
	mustRun(t, "simulate", "--file", example("web-service.json"), "--event-log", path)
	return path
}

func TestHistoryReadsRecordedLog(t *testing.T) {
	log := recordedLog(t)

	stdout := mustRun(t, "history", "--event-log", log, "--json")
	var events []control.Event
	if err := json.Unmarshal([]byte(stdout), &events); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(events) == 0 || events[len(events)-1].Type != control.EventGoalAchieved {
		t.Fatalf("history should end with the goal achieved, got %d events", len(events))
	}

	limited := mustRun(t, "history", "--event-log", log, "--limit", "2")
	if lines := strings.Count(limited, "\n"); lines != 2 {
		t.Fatalf("limit 2 printed %d lines:\n%s", lines, limited)
	}

	none := mustRun(t, "history", "--event-log", log, "--goal", "no-such-goal")
	if strings.TrimSpace(none) != "no matching events" {
		t.Fatalf("an empty filter should say so, got %q", none)
	}
}

func TestHistoryRequiresEventLog(t *testing.T) {
	_, _, err := runCLI(t, "history")
	if err == nil || !strings.Contains(err.Error(), "event-log is required") {
		t.Fatalf("want a missing-flag error, got %v", err)
	}
}

func TestExplainRoute(t *testing.T) {
	log := recordedLog(t)
	stdout := mustRun(t, "explain", "--event-log", log, "--target", "homepage.example.com")
	if !strings.Contains(stdout, "homepage.example.com") {
		t.Fatalf("explanation should name its target:\n%s", stdout)
	}

	jsonOut := mustRun(t, "explain", "--event-log", log, "--target", "homepage.example.com", "--json")
	var explanation control.Explanation
	if err := json.Unmarshal([]byte(jsonOut), &explanation); err != nil {
		t.Fatalf("decode explanation: %v", err)
	}
	if !explanation.Found {
		t.Fatal("a published route should have history")
	}
}

func TestExplainUnknownTargetFails(t *testing.T) {
	log := recordedLog(t)
	_, _, err := runCLI(t, "explain", "--event-log", log, "--target", "nowhere.example.com")
	if err == nil || !strings.Contains(err.Error(), "no history") {
		t.Fatalf("want a no-history error, got %v", err)
	}
}

func TestExplainRequiresFlags(t *testing.T) {
	_, _, err := runCLI(t, "explain", "--target", "x")
	if err == nil || !strings.Contains(err.Error(), "event-log and target are required") {
		t.Fatalf("want a missing-flag error, got %v", err)
	}
}

func TestPlanFromScenario(t *testing.T) {
	stdout := mustRun(t, "plan", "--file", example("web-service.json"), "--json")
	var plan control.Plan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, stdout)
	}
	text := mustRun(t, "plan", "--file", example("web-service.json"))
	if strings.TrimSpace(text) == "" {
		t.Fatal("plan printed nothing")
	}
}

func TestPlanAgainstRecordedHistory(t *testing.T) {
	log := recordedLog(t)
	mustRun(t, "plan", "--file", example("web-service.json"), "--event-log", log)
}

func TestDiagnoseDeterministic(t *testing.T) {
	log := recordedLog(t)
	stdout := mustRun(t, "diagnose", "--event-log", log, "--goal", "homepage-public",
		"--file", example("web-service.json"), "--deterministic")
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("diagnosis printed nothing")
	}
	jsonOut := mustRun(t, "diagnose", "--event-log", log, "--goal", "homepage-public",
		"--file", example("web-service.json"), "--deterministic", "--json")
	if !json.Valid([]byte(jsonOut)) {
		t.Fatalf("diagnose --json is not JSON:\n%s", jsonOut)
	}
}

func TestDiagnoseRequiresFlags(t *testing.T) {
	_, _, err := runCLI(t, "diagnose", "--goal", "g")
	if err == nil || !strings.Contains(err.Error(), "event-log and goal are required") {
		t.Fatalf("want a missing-flag error, got %v", err)
	}
}
