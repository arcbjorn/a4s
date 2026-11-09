package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runCLI drives the real command dispatch with in-memory streams, so a test
// observes exactly what an operator would see on each stream.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCLIWithInput(t, "", args...)
}

func runCLIWithInput(t *testing.T, input string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{stdin: strings.NewReader(input), stdout: &out, stderr: &errOut}
	err = c.run(args)
	return out.String(), errOut.String(), err
}

// mustRun fails the test if the command errors, and returns its stdout.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("a4s %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

// example returns the path of a committed example scenario. Tests run from the
// package directory, two levels below the repository root.
func example(name string) string {
	return filepath.Join("..", "..", "examples", name)
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRequiresCommand(t *testing.T) {
	stdout, stderr, err := runCLI(t)
	if err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("want a missing-command error, got %v", err)
	}
	if stdout != "" {
		t.Fatalf("usage must not reach stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("usage should be printed to stderr, got %q", stderr)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	_, _, err := runCLI(t, "deploy")
	if err == nil || !strings.Contains(err.Error(), `unknown command "deploy"`) {
		t.Fatalf("want an unknown-command error, got %v", err)
	}
}

func TestHelpPrintsUsageToStderr(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		stdout, stderr, err := runCLI(t, arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if stdout != "" || !strings.Contains(stderr, "a4s validate") {
			t.Fatalf("%s: usage should go to stderr only; stdout=%q stderr=%q", arg, stdout, stderr)
		}
	}
}

func TestFlagErrorsGoToInjectedStderr(t *testing.T) {
	_, stderr, err := runCLI(t, "validate", "--no-such-flag")
	if err == nil {
		t.Fatal("an undefined flag must be refused")
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("flag diagnostics should reach the cli stderr, got %q", stderr)
	}
}

func TestVersionShort(t *testing.T) {
	stdout := mustRun(t, "version", "--short")
	if strings.TrimSpace(stdout) != version {
		t.Fatalf("version --short = %q, want %q", stdout, version)
	}
}

func TestVersionJSON(t *testing.T) {
	stdout := mustRun(t, "version", "--json")
	var info BuildInfo
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		t.Fatalf("decode version: %v\n%s", err, stdout)
	}
	if info.Version != version || info.Go != runtime.Version() ||
		info.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("unexpected build info %+v", info)
	}
}

func TestBuildInfoString(t *testing.T) {
	tests := []struct {
		name string
		info BuildInfo
		want []string
		deny []string
	}{
		{
			name: "unstamped",
			info: BuildInfo{Version: "0.2.0-dev", Go: "go1.26", Platform: "linux/amd64"},
			want: []string{"0.2.0-dev\ngo1.26 linux/amd64"},
			deny: []string{"(", "built"},
		},
		{
			name: "commit is shortened",
			info: BuildInfo{Version: "1.0.0", Commit: "0123456789abcdef0123", Go: "go1.26",
				Platform: "linux/arm64", Date: "2026-01-02"},
			want: []string{"1.0.0 (0123456789ab)", "built 2026-01-02"},
			deny: []string{"0123456789abc", "modified"},
		},
		{
			name: "dirty tree is called out",
			info: BuildInfo{Version: "1.0.0", Commit: "abc", Modified: true, Go: "go1.26",
				Platform: "darwin/arm64"},
			want: []string{"1.0.0 (abc, modified)"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.info.String()
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("String() = %q, want it to contain %q", got, want)
				}
			}
			for _, deny := range test.deny {
				if strings.Contains(got, deny) {
					t.Errorf("String() = %q, must not contain %q", got, deny)
				}
			}
		})
	}
}
