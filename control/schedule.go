package control

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule declares when a workload should run and how a run completes.
//
// A scheduled workload is not a service with replicas held up forever. It starts
// when its schedule says a run is due, and it is done when the container exits
// successfully. Declaring the schedule is what lets the kernel treat it that
// way: a service that exits is a failure, and a batch job that exits is a
// success, and nothing but the goal can distinguish them.
//
// Evaluation is a pure function of the observed world's time, never of a wall
// clock inside an agent. A schedule that consulted time.Now would make placement
// non-deterministic and untestable, and two agents evaluating it a second apart
// could disagree about whether a run had already started.
type Schedule struct {
	// Cron is a five-field expression: minute hour day-of-month month day-of-week.
	// Only the subset a deployment actually needs is supported: numbers, commas,
	// ranges, steps, and a wildcard. Named months and weekdays are deliberately
	// absent, because "JAN" and "1" meaning the same thing is a second parser for
	// no additional expressiveness.
	Cron string `json:"cron"`
	// Deadline bounds how long one run may take before it is considered failed.
	// Zero means DefaultRunDeadline. A batch job with no deadline can wedge a
	// schedule forever by never finishing.
	Deadline Duration `json:"deadline,omitempty"`
	// Concurrency decides what happens when a run is due while one is active.
	Concurrency ConcurrencyPolicy `json:"concurrency,omitempty"`
	// Completions is how many successful exits make one run complete. Zero means
	// one. A parallel batch job declares more.
	Completions int `json:"completions,omitempty"`
	// Retries is how many times a failed run may be restarted before the goal is
	// blocked. Zero means a failure blocks immediately, which is the safe default:
	// a job that failed for a real reason should not be retried silently.
	Retries int `json:"retries,omitempty"`
}

// ConcurrencyPolicy decides what a due run does about a run already going.
type ConcurrencyPolicy string

const (
	// ConcurrencyForbid skips the due run. The default, because a job that
	// overlaps itself is usually a bug rather than an intent: two copies of a
	// nightly export writing the same file corrupt it.
	ConcurrencyForbid ConcurrencyPolicy = "forbid"
	// ConcurrencyAllow starts the due run alongside the active one.
	ConcurrencyAllow ConcurrencyPolicy = "allow"
	// ConcurrencyReplace drains the active run and starts the due one.
	ConcurrencyReplace ConcurrencyPolicy = "replace"
)

// DefaultRunDeadline bounds a scheduled run that declares no deadline.
const DefaultRunDeadline = time.Hour

// Duration is a JSON-friendly time.Duration accepting "30s" or "1h30m".
//
// The standard library marshals a Duration as an integer nanosecond count, which
// is unreadable in a goal document an operator edits by hand.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(time.Duration(d).String())), nil
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	text, err := strconv.Unquote(string(raw))
	if err != nil {
		// Also accept a bare number of seconds, since that is what someone who
		// has not read the schema will write.
		seconds, numErr := strconv.ParseFloat(string(raw), 64)
		if numErr != nil {
			return fmt.Errorf("duration must be a string like \"30s\": %w", err)
		}
		*d = Duration(time.Duration(seconds * float64(time.Second)))
		return nil
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// Validate checks a schedule is usable before anything is authorized against it.
func (s *Schedule) Validate() error {
	if s == nil {
		return nil
	}
	if _, err := ParseCron(s.Cron); err != nil {
		return err
	}
	if s.Deadline < 0 {
		return fmt.Errorf("schedule deadline cannot be negative")
	}
	if s.Completions < 0 {
		return fmt.Errorf("schedule completions cannot be negative")
	}
	if s.Retries < 0 {
		return fmt.Errorf("schedule retries cannot be negative")
	}
	switch s.Concurrency {
	case "", ConcurrencyForbid, ConcurrencyAllow, ConcurrencyReplace:
	default:
		return fmt.Errorf("unknown concurrency policy %q", s.Concurrency)
	}
	return nil
}

// RunDeadline reports the effective deadline for one run.
func (s *Schedule) RunDeadline() time.Duration {
	if s == nil || s.Deadline <= 0 {
		return DefaultRunDeadline
	}
	return s.Deadline.Duration()
}

// RequiredCompletions reports how many successful exits complete a run.
func (s *Schedule) RequiredCompletions() int {
	if s == nil || s.Completions <= 0 {
		return 1
	}
	return s.Completions
}

// ConcurrencyPolicyOrDefault resolves the effective policy.
func (s *Schedule) ConcurrencyPolicyOrDefault() ConcurrencyPolicy {
	if s == nil || s.Concurrency == "" {
		return ConcurrencyForbid
	}
	return s.Concurrency
}

// CronSchedule is a parsed cron expression.
type CronSchedule struct {
	minutes  []int
	hours    []int
	days     []int
	months   []int
	weekdays []int
}

// ParseCron parses a five-field cron expression.
func ParseCron(expression string) (CronSchedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return CronSchedule{}, fmt.Errorf(
			"cron expression needs five fields (minute hour day month weekday), got %d", len(fields))
	}
	minutes, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron minute: %w", err)
	}
	hours, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron hour: %w", err)
	}
	days, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron day of month: %w", err)
	}
	months, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron month: %w", err)
	}
	weekdays, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron day of week: %w", err)
	}
	return CronSchedule{
		minutes: minutes, hours: hours, days: days,
		months: months, weekdays: weekdays,
	}, nil
}

// Matches reports whether a time falls on this schedule.
//
// The time is always evaluated in UTC. A schedule interpreted in a local zone
// would silently run twice or not at all across a daylight-saving transition,
// which is exactly the kind of failure nobody notices until the night it
// happens.
func (c CronSchedule) Matches(at time.Time) bool {
	at = at.UTC()
	if !contains(c.minutes, at.Minute()) || !contains(c.hours, at.Hour()) {
		return false
	}
	return c.matchesDate(at)
}

// matchesDate reports whether a date falls on this schedule, ignoring the time
// of day. It is separate so Next can reject a whole day at once.
func (c CronSchedule) matchesDate(at time.Time) bool {
	if !contains(c.months, int(at.Month())) {
		return false
	}
	// Standard cron semantics: when both day-of-month and day-of-week are
	// restricted, either matching is enough. This is surprising but it is what
	// every other cron does, and being subtly different would be worse.
	dayRestricted := len(c.days) != 31
	weekdayRestricted := len(c.weekdays) != 7
	dayMatch := contains(c.days, at.Day())
	weekdayMatch := contains(c.weekdays, int(at.Weekday()))
	switch {
	case dayRestricted && weekdayRestricted:
		return dayMatch || weekdayMatch
	case dayRestricted:
		return dayMatch
	case weekdayRestricted:
		return weekdayMatch
	default:
		return true
	}
}

// Next reports the first matching time strictly after the given time.
//
// It searches at most four years, which terminates on an impossible schedule
// such as February 30th rather than looping forever. A day whose date does not
// match is skipped whole rather than a minute at a time: an impossible schedule
// is evaluated on every reconciliation, and walking four years in one-minute
// steps costs milliseconds each time for an answer that is always the same.
func (c CronSchedule) Next(after time.Time) (time.Time, bool) {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(4, 0, 0)
	for candidate.Before(limit) {
		if !c.matchesDate(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(),
				candidate.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if c.Matches(candidate) {
			return candidate, true
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, false
}

// Due reports whether a run should start, given when the last one started.
//
// A zero lastRun means nothing has ever run, and the answer is the schedule's
// own opinion at that instant: a job is not retroactively due for every slot
// since the epoch. That keeps a newly submitted nightly job from firing
// immediately at noon.
func (c CronSchedule) Due(now, lastRun time.Time) bool {
	now = now.UTC().Truncate(time.Minute)
	if lastRun.IsZero() {
		return c.Matches(now)
	}
	next, ok := c.Next(lastRun.UTC())
	if !ok {
		return false
	}
	return !next.After(now)
}

// parseCronField parses one field into the set of values it matches.
func parseCronField(field string, min, max int) ([]int, error) {
	if field == "" {
		return nil, fmt.Errorf("empty field")
	}
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		step := 1
		if slash := strings.Index(part, "/"); slash >= 0 {
			parsed, err := strconv.Atoi(part[slash+1:])
			if err != nil || parsed <= 0 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step = parsed
			part = part[:slash]
		}
		low, high := min, max
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			bounds := strings.SplitN(part, "-", 2)
			var err error
			if low, err = strconv.Atoi(bounds[0]); err != nil {
				return nil, fmt.Errorf("invalid range start in %q", part)
			}
			if high, err = strconv.Atoi(bounds[1]); err != nil {
				return nil, fmt.Errorf("invalid range end in %q", part)
			}
		default:
			value, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", part)
			}
			low, high = value, value
		}
		if low < min || high > max || low > high {
			return nil, fmt.Errorf("value %q is outside %d-%d", part, min, max)
		}
		for value := low; value <= high; value += step {
			set[value] = true
		}
	}
	values := make([]int, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Ints(values)
	return values, nil
}

func contains(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
