// Package source submits goals from a versioned repository.
//
// It is deliberately thin. A goal that arrives from git is admitted through the
// same validation as one submitted over the operator API: the repository is a
// transport for goal documents, not a privileged path into the control plane.
// Nothing here can authorize anything, and a repository cannot carry an approval
// — those are separately signed operator decisions, so a goal file naming one
// would be rejected by the kernel rather than honoured.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/arcbjorn/agentic-git/pkg/gitcmd"

	"github.com/arcbjorn/a4s/control"
)

// DefaultPollInterval is how often the adapter checks the tracked ref.
//
// Goals describe desired state, so a delay costs convergence latency rather than
// correctness. This is slow enough not to hammer a remote and fast enough that a
// deploy feels like a deploy.
const DefaultPollInterval = 30 * time.Second

// Submitter accepts goals. The server implements it; a test can substitute its
// own without a repository.
type Submitter interface {
	Submit(control.Goal) error
}

// Repository is a git-backed source of goal documents.
type Repository struct {
	// Source is the underlying mirror and blob reader.
	Source *gitcmd.Source
	// Path is the directory inside the repository holding goal documents. Empty
	// means the repository root.
	Path string
	// Submitter receives every goal read from a new commit.
	Submitter Submitter
	// Now is injectable so tests do not depend on the wall clock.
	Now func() time.Time

	// applied is the last commit whose goals were submitted successfully.
	applied string
}

// Result reports what one sync did.
type Result struct {
	// Commit is the revision that was read.
	Commit string
	// Message is that commit's subject, so history can attribute a change.
	Message string
	// Goals names the goals submitted, in a stable order.
	Goals []string
	// Changed is false when the ref had not moved since the last sync.
	Changed bool
	// Rejected maps a file path to why its goal was refused. A bad file does not
	// stop the good ones: a repository with one broken goal should still converge
	// the rest, and the operator needs to know which one broke.
	Rejected map[string]string
}

// NewRepository builds an adapter over a prepared git source.
func NewRepository(source *gitcmd.Source, dir string, submitter Submitter) *Repository {
	return &Repository{Source: source, Path: dir, Submitter: submitter, Now: time.Now}
}

// Sync fetches the tracked ref and submits every goal it finds.
//
// A commit that was already applied is skipped, so polling is cheap and a goal
// is not resubmitted on every tick. The commit is recorded as applied only when
// at least one goal was read, which means a transient failure is retried on the
// next sync rather than silently swallowed.
func (r *Repository) Sync(ctx context.Context) (Result, error) {
	if r.Source == nil {
		return Result{}, fmt.Errorf("git source is not configured")
	}
	if r.Submitter == nil {
		return Result{}, fmt.Errorf("git source has no submitter")
	}
	commit, err := r.Source.Sync(ctx)
	if err != nil {
		return Result{}, err
	}
	if commit == r.applied {
		return Result{Commit: commit, Changed: false}, nil
	}

	files, err := r.Source.ListFiles(ctx, commit, r.Path)
	if err != nil {
		return Result{}, err
	}
	message, err := r.Source.CommitMessage(ctx, commit)
	if err != nil {
		// A missing subject is not worth failing a deploy over.
		message = ""
	}

	result := Result{Commit: commit, Message: message, Changed: true}
	sort.Strings(files)
	for _, file := range files {
		if !isGoalDocument(file) {
			continue
		}
		raw, err := r.Source.ReadFile(ctx, commit, file)
		if err != nil {
			result.reject(file, err.Error())
			continue
		}
		goal, err := decodeGoal(raw)
		if err != nil {
			result.reject(file, err.Error())
			continue
		}
		if err := r.Submitter.Submit(goal); err != nil {
			result.reject(file, err.Error())
			continue
		}
		result.Goals = append(result.Goals, goal.ID)
	}

	if len(result.Goals) == 0 && len(result.Rejected) > 0 {
		// Every goal in this revision failed. Not recording it as applied means the
		// next sync tries again, which is what an operator fixing a typo expects.
		return result, fmt.Errorf("no goal in %s could be applied", shortCommit(commit))
	}
	r.applied = commit
	return result, nil
}

// Applied reports the last commit whose goals were submitted.
func (r *Repository) Applied() string { return r.applied }

// Watch polls until the context is cancelled, reporting each sync.
//
// Errors are reported rather than returned, because a source loop must survive
// an unreachable remote: a network blip should not stop a control plane from
// tracking its repository once the network returns.
func (r *Repository) Watch(ctx context.Context, interval time.Duration,
	report func(Result, error)) error {

	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Sync once immediately so startup does not wait a full interval.
	if report != nil {
		report(r.Sync(ctx))
	} else {
		_, _ = r.Sync(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if report != nil {
				report(r.Sync(ctx))
			} else {
				_, _ = r.Sync(ctx)
			}
		}
	}
}

func (result *Result) reject(file, reason string) {
	if result.Rejected == nil {
		result.Rejected = map[string]string{}
	}
	result.Rejected[file] = reason
}

// isGoalDocument reports whether a path looks like a goal file. Anything else in
// the directory — a README, a CI config — is ignored rather than rejected, so a
// repository can hold more than goals.
func isGoalDocument(file string) bool {
	return strings.EqualFold(path.Ext(file), ".json")
}

// decodeGoal strictly decodes one goal document.
//
// Unknown fields are refused: a goal written against a newer schema may mean
// something this build would silently ignore, and silently ignoring part of a
// deployment request is worse than refusing it.
func decodeGoal(raw []byte) (control.Goal, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var goal control.Goal
	if err := decoder.Decode(&goal); err != nil {
		return control.Goal{}, fmt.Errorf("decode goal: %w", err)
	}
	if decoder.More() {
		return control.Goal{}, fmt.Errorf("goal document has trailing content")
	}
	if goal.ID == "" {
		return control.Goal{}, fmt.Errorf("goal document has no id")
	}
	return goal, nil
}

func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
