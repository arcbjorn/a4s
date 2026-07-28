package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func at(spec string) time.Time {
	parsed, err := time.Parse(time.RFC3339, spec)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}

func TestParseCronAcceptsSupportedForms(t *testing.T) {
	for _, expression := range []string{
		"* * * * *",
		"0 3 * * *",
		"*/15 * * * *",
		"0 0 1 1 *",
		"30 2 * * 0",
		"0 9-17 * * 1-5",
		"0,30 * * * *",
		"0 */6 * * *",
	} {
		if _, err := ParseCron(expression); err != nil {
			t.Fatalf("%q was refused: %v", expression, err)
		}
	}
}

func TestParseCronRejectsBadExpressions(t *testing.T) {
	for _, expression := range []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 7",
		"*/0 * * * *",
		"5-1 * * * *",
		"JAN * * * *",
		"abc * * * *",
	} {
		if _, err := ParseCron(expression); err == nil {
			t.Fatalf("%q was accepted", expression)
		}
	}
}

func TestCronMatches(t *testing.T) {
	daily, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !daily.Matches(at("2026-07-25T03:00:00Z")) {
		t.Fatal("3am did not match a daily 3am schedule")
	}
	if daily.Matches(at("2026-07-25T03:01:00Z")) {
		t.Fatal("3:01 matched a schedule for 3:00")
	}
	if daily.Matches(at("2026-07-25T04:00:00Z")) {
		t.Fatal("4am matched a 3am schedule")
	}
}

// Schedules are evaluated in UTC so a daylight-saving transition cannot make a
// job run twice or not at all.
func TestCronIsEvaluatedInUTC(t *testing.T) {
	daily, err := ParseCron("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("test", 5*3600)
	// 01:30 UTC expressed in a +5 zone is 06:30 local; the schedule must follow
	// the instant, not the local clock reading.
	local := at("2026-07-25T01:30:00Z").In(zone)
	if !daily.Matches(local) {
		t.Fatalf("a non-UTC time at the scheduled instant did not match: %s", local)
	}
	if daily.Matches(at("2026-07-25T06:30:00Z")) {
		t.Fatal("the local clock reading matched instead of the instant")
	}
}

func TestCronNext(t *testing.T) {
	daily, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next, ok := daily.Next(at("2026-07-25T03:00:00Z"))
	if !ok {
		t.Fatal("no next occurrence")
	}
	// Strictly after, so the same instant does not resolve to itself.
	if !next.Equal(at("2026-07-26T03:00:00Z")) {
		t.Fatalf("next occurrence is %s", next)
	}
}

// An impossible schedule terminates instead of looping forever.
func TestCronNextTerminatesOnImpossibleSchedule(t *testing.T) {
	impossible, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := impossible.Next(at("2026-07-25T00:00:00Z")); ok {
		t.Fatal("February 30th resolved to a real time")
	}
}

// Skipping a non-matching day whole must not skip past a match inside it, which
// is the way a date-level shortcut goes wrong.
func TestCronNextFindsAMatchLateInTheDay(t *testing.T) {
	schedule, err := ParseCron("59 23 1 * *")
	if err != nil {
		t.Fatal(err)
	}
	next, ok := schedule.Next(at("2026-07-25T00:00:00Z"))
	if !ok {
		t.Fatal("no next occurrence")
	}
	if !next.Equal(at("2026-08-01T23:59:00Z")) {
		t.Fatalf("next occurrence is %s, want 2026-08-01T23:59:00Z", next)
	}

	// A match later on the same day the search starts must still be found.
	sameDay, ok := schedule.Next(at("2026-08-01T00:00:00Z"))
	if !ok || !sameDay.Equal(at("2026-08-01T23:59:00Z")) {
		t.Fatalf("same-day occurrence is %s, want 2026-08-01T23:59:00Z", sameDay)
	}
}

// An impossible schedule is re-evaluated on every reconciliation, so the search
// has to reject a non-matching date without walking its minutes.
func TestCronNextRejectsImpossibleDatesCheaply(t *testing.T) {
	impossible, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, ok := impossible.Next(at("2026-07-25T00:00:00Z")); ok {
		t.Fatal("February 30th resolved to a real time")
	}
	// Four years of minute-stepping takes tens of milliseconds; four years of
	// day-stepping takes well under one. The bound is loose enough not to be
	// flaky and tight enough to catch a regression to the minute-by-minute scan.
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("impossible schedule took %s to reject, want under 5ms", elapsed)
	}
}

// Standard cron semantics: with both day-of-month and day-of-week restricted,
// either match is enough.
func TestCronDayAndWeekdayAreUnioned(t *testing.T) {
	schedule, err := ParseCron("0 0 1 * 0")
	if err != nil {
		t.Fatal(err)
	}
	// The 1st of the month, not a Sunday.
	if !schedule.Matches(at("2026-07-01T00:00:00Z")) {
		t.Fatal("the first of the month did not match")
	}
	// A Sunday that is not the 1st.
	sunday := at("2026-07-26T00:00:00Z")
	if sunday.Weekday() != time.Sunday {
		t.Fatalf("fixture is not a Sunday: %s", sunday.Weekday())
	}
	if !schedule.Matches(sunday) {
		t.Fatal("a Sunday did not match")
	}
	// Neither.
	if schedule.Matches(at("2026-07-15T00:00:00Z")) {
		t.Fatal("a day matching neither field matched")
	}
}

// A newly submitted job is not retroactively due for every missed slot.
func TestDueDoesNotBackfillANewSchedule(t *testing.T) {
	nightly, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// Submitted at noon, having never run: not due until 3am.
	if nightly.Due(at("2026-07-25T12:00:00Z"), time.Time{}) {
		t.Fatal("a never-run nightly job was due at noon")
	}
	if !nightly.Due(at("2026-07-25T03:00:00Z"), time.Time{}) {
		t.Fatal("a never-run nightly job was not due at 3am")
	}
}

func TestDueAfterAPreviousRun(t *testing.T) {
	nightly, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	lastRun := at("2026-07-25T03:00:00Z")
	// Later the same day: the next slot has not arrived.
	if nightly.Due(at("2026-07-25T20:00:00Z"), lastRun) {
		t.Fatal("due before the next scheduled slot")
	}
	// The next day's slot has arrived.
	if !nightly.Due(at("2026-07-26T03:00:00Z"), lastRun) {
		t.Fatal("not due at the next scheduled slot")
	}
	// A missed slot is still due once observed late, so an outage does not skip
	// the run entirely.
	if !nightly.Due(at("2026-07-26T09:00:00Z"), lastRun) {
		t.Fatal("a missed slot was skipped instead of running late")
	}
}

func TestScheduleValidation(t *testing.T) {
	valid := &Schedule{Cron: "0 3 * * *"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid schedule was refused: %v", err)
	}
	if err := (&Schedule{Cron: "nonsense"}).Validate(); err == nil {
		t.Fatal("an invalid cron expression was accepted")
	}
	if err := (&Schedule{Cron: "* * * * *", Concurrency: "sometimes"}).Validate(); err == nil {
		t.Fatal("an unknown concurrency policy was accepted")
	}
	if err := (&Schedule{Cron: "* * * * *", Retries: -1}).Validate(); err == nil {
		t.Fatal("negative retries were accepted")
	}
	if err := (&Schedule{Cron: "* * * * *", Completions: -1}).Validate(); err == nil {
		t.Fatal("negative completions were accepted")
	}
	// A nil schedule is valid: it means the workload is a service.
	var absent *Schedule
	if err := absent.Validate(); err != nil {
		t.Fatalf("an absent schedule was refused: %v", err)
	}
}

func TestScheduleDefaults(t *testing.T) {
	var absent *Schedule
	if absent.RunDeadline() != DefaultRunDeadline {
		t.Fatal("an absent schedule did not report the default deadline")
	}
	if absent.RequiredCompletions() != 1 {
		t.Fatal("an absent schedule did not require one completion")
	}
	if absent.ConcurrencyPolicyOrDefault() != ConcurrencyForbid {
		t.Fatal("the default concurrency policy is not forbid")
	}
	// Forbid is the default because a job overlapping itself is usually a bug.
	explicit := &Schedule{Cron: "* * * * *", Concurrency: ConcurrencyAllow}
	if explicit.ConcurrencyPolicyOrDefault() != ConcurrencyAllow {
		t.Fatal("an explicit policy was overridden")
	}
}

// Durations are readable in a hand-edited goal document, and a bare number of
// seconds is accepted for whoever has not read the schema.
func TestDurationJSON(t *testing.T) {
	var schedule Schedule
	if err := json.Unmarshal([]byte(`{"cron":"* * * * *","deadline":"90s"}`), &schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.Deadline.Duration() != 90*time.Second {
		t.Fatalf("parsed %s", schedule.Deadline.Duration())
	}

	if err := json.Unmarshal([]byte(`{"cron":"* * * * *","deadline":45}`), &schedule); err != nil {
		t.Fatalf("a bare number was refused: %v", err)
	}
	if schedule.Deadline.Duration() != 45*time.Second {
		t.Fatalf("bare number parsed to %s", schedule.Deadline.Duration())
	}

	encoded, err := json.Marshal(Schedule{Cron: "* * * * *", Deadline: Duration(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"deadline":"1m0s"`; !strings.Contains(string(encoded), want) {
		t.Fatalf("encoded as %s, want %s", encoded, want)
	}

	if err := json.Unmarshal([]byte(`{"cron":"* * * * *","deadline":"forever"}`), &schedule); err == nil {
		t.Fatal("an unparseable duration was accepted")
	}
}
