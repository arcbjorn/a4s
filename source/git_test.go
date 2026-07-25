package source

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/agentic-git/pkg/gitcmd"

	"github.com/arcbjorn/a4s/control"
)

const testImage = "registry.example/web@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// recordingSubmitter stands in for the server's admission path.
type recordingSubmitter struct {
	goals []control.Goal
	err   error
}

func (s *recordingSubmitter) Submit(goal control.Goal) error {
	if s.err != nil {
		return s.err
	}
	s.goals = append(s.goals, goal)
	return nil
}

func goalJSON(id string, replicas int) string {
	return `{
	  "api_version": "a4s.io/v1alpha1",
	  "id": "` + id + `",
	  "objective": "keep ` + id + ` serving",
	  "workload": {
	    "name": "` + id + `",
	    "image": "` + testImage + `",
	    "replicas": ` + strconv.Itoa(replicas) + `,
	    "port": 8080,
	    "resources": {"cpu_millis": 100, "memory_mb": 128}
	  }
	}`
}

// upstream creates a real repository, so these tests exercise git end to end.
func upstream(t *testing.T, files map[string]string) (*gitcmd.Runner, string) {
	t.Helper()
	git := &gitcmd.Runner{Timeout: 30 * time.Second}
	if err := git.Available(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	if _, err := git.Run(context.Background(), dir, "init", "--quiet", "-b", "main"); err != nil {
		t.Fatalf("init: %v", err)
	}
	commitFiles(t, git, dir, files, "initial")
	return git, dir
}

func commitFiles(t *testing.T, git *gitcmd.Runner, dir string,
	files map[string]string, message string) {

	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := git.Run(ctx, dir, "add", "-A"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := git.Run(ctx, dir,
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit", "--quiet", "-m", message); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func newRepository(t *testing.T, git *gitcmd.Runner, remote string,
	submitter Submitter) *Repository {

	t.Helper()
	source := gitcmd.NewSource(git, filepath.Join(t.TempDir(), "mirror.git"), remote, "main")
	return NewRepository(source, "goals", submitter)
}

// The headline case: goals in a repository reach the control plane's admission
// path.
func TestSyncSubmitsGoalsFromRepository(t *testing.T) {
	git, remote := upstream(t, map[string]string{
		"goals/web.json": goalJSON("web", 1),
		"goals/api.json": goalJSON("api", 2),
		"README.md":      "not a goal",
	})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	result, err := repository.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !result.Changed {
		t.Fatal("the first sync reported no change")
	}
	if len(submitter.goals) != 2 {
		t.Fatalf("expected two goals, got %d: %+v", len(submitter.goals), result)
	}
	if result.Message != "initial" {
		t.Fatalf("commit message not attributed: %q", result.Message)
	}
	// Order is stable so history and logs are reproducible.
	if result.Goals[0] != "api" || result.Goals[1] != "web" {
		t.Fatalf("goals were not submitted in a stable order: %v", result.Goals)
	}
}

// Polling an unchanged ref must not resubmit, or every tick would rewrite the
// accepted goal set.
func TestSyncSkipsAnUnchangedCommit(t *testing.T) {
	git, remote := upstream(t, map[string]string{"goals/web.json": goalJSON("web", 1)})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	if _, err := repository.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := repository.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatal("an unchanged ref reported a change")
	}
	if len(submitter.goals) != 1 {
		t.Fatalf("the goal was submitted %d times", len(submitter.goals))
	}
}

// A new commit is picked up, which is what makes this a deployment mechanism.
func TestSyncObservesNewCommits(t *testing.T) {
	git, remote := upstream(t, map[string]string{"goals/web.json": goalJSON("web", 1)})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	if _, err := repository.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	commitFiles(t, git, remote,
		map[string]string{"goals/web.json": goalJSON("web", 3)}, "scale web to three")

	result, err := repository.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Message != "scale web to three" {
		t.Fatalf("the new commit was not observed: %+v", result)
	}
	if len(submitter.goals) != 2 {
		t.Fatalf("expected a resubmission, got %d goals", len(submitter.goals))
	}
	if submitter.goals[1].Workload.Replicas != 3 {
		t.Fatalf("the updated goal was not read: %+v", submitter.goals[1])
	}
}

// One broken file must not stop the rest of a repository from converging, and
// the operator must be told which one broke.
func TestSyncRejectsBadGoalsIndividually(t *testing.T) {
	git, remote := upstream(t, map[string]string{
		"goals/web.json":    goalJSON("web", 1),
		"goals/broken.json": `{"id": "broken", "nonsense_field": true}`,
	})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	result, err := repository.Sync(context.Background())
	if err != nil {
		t.Fatalf("one bad file failed the whole sync: %v", err)
	}
	if len(result.Goals) != 1 || result.Goals[0] != "web" {
		t.Fatalf("the good goal was not applied: %+v", result)
	}
	reason, rejected := result.Rejected["goals/broken.json"]
	if !rejected {
		t.Fatalf("the bad goal was not reported: %+v", result.Rejected)
	}
	if !strings.Contains(reason, "nonsense_field") {
		t.Fatalf("the rejection did not name the problem: %q", reason)
	}
}

// A revision where everything failed is not recorded as applied, so fixing the
// repository is enough to retry.
func TestSyncDoesNotApplyAFullyBrokenRevision(t *testing.T) {
	git, remote := upstream(t, map[string]string{
		"goals/broken.json": `{"id": "broken", "nonsense_field": true}`,
	})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	if _, err := repository.Sync(context.Background()); err == nil {
		t.Fatal("a revision with no applicable goal succeeded")
	}
	if repository.Applied() != "" {
		t.Fatal("a fully broken revision was recorded as applied")
	}

	// Fixing the file makes the next sync apply it, without a new commit being
	// required to reset any state.
	commitFiles(t, git, remote,
		map[string]string{"goals/broken.json": goalJSON("broken", 1)}, "fix the goal")
	result, err := repository.Sync(context.Background())
	if err != nil {
		t.Fatalf("the corrected revision was refused: %v", err)
	}
	if len(result.Goals) != 1 {
		t.Fatalf("the corrected goal was not applied: %+v", result)
	}
}

// A goal the server refuses is reported, not swallowed. The repository is a
// transport, so admission stays the server's decision.
func TestSyncReportsSubmitterRejection(t *testing.T) {
	git, remote := upstream(t, map[string]string{"goals/web.json": goalJSON("web", 1)})
	submitter := &recordingSubmitter{err: errRefused{}}
	repository := newRepository(t, git, remote, submitter)

	result, err := repository.Sync(context.Background())
	if err == nil {
		t.Fatal("a refused goal reported success")
	}
	if len(result.Rejected) != 1 {
		t.Fatalf("the refusal was not reported: %+v", result)
	}
}

type errRefused struct{}

func (errRefused) Error() string { return "goal refused by admission" }

// Files that are not goal documents are ignored rather than rejected, so a
// repository can hold a README beside its goals.
func TestSyncIgnoresNonGoalFiles(t *testing.T) {
	git, remote := upstream(t, map[string]string{
		"goals/web.json":    goalJSON("web", 1),
		"goals/notes.md":    "# notes",
		"goals/.gitkeep":    "",
		"goals/config.yaml": "unused: true",
	})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	result, err := repository.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Goals) != 1 || len(result.Rejected) != 0 {
		t.Fatalf("non-goal files were not ignored cleanly: %+v", result)
	}
}

func TestSyncRequiresConfiguration(t *testing.T) {
	if _, err := (&Repository{}).Sync(context.Background()); err == nil {
		t.Fatal("a source with no git configuration synced")
	}
	source := gitcmd.NewSource(&gitcmd.Runner{}, "/tmp/x.git", "https://example.invalid/x", "main")
	if _, err := (&Repository{Source: source}).Sync(context.Background()); err == nil {
		t.Fatal("a source with no submitter synced")
	}
}

// Watch must survive a failing sync: an unreachable remote is a network problem,
// not a reason to stop tracking a repository.
func TestWatchReportsAndKeepsRunning(t *testing.T) {
	git, remote := upstream(t, map[string]string{"goals/web.json": goalJSON("web", 1)})
	submitter := &recordingSubmitter{}
	repository := newRepository(t, git, remote, submitter)

	ctx, cancel := context.WithCancel(context.Background())
	reports := make(chan Result, 4)
	go func() {
		_ = repository.Watch(ctx, 20*time.Millisecond, func(result Result, err error) {
			if err == nil {
				select {
				case reports <- result:
				default:
				}
			}
		})
	}()

	// The first report arrives immediately rather than after a full interval.
	select {
	case result := <-reports:
		if !result.Changed {
			t.Fatal("the first watch report showed no change")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("watch produced no report")
	}
	cancel()
}
