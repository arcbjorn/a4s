package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

// Build metadata, injected at link time:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD)"
//
// The defaults describe an unstamped developer build. They are deliberately not
// a plausible-looking version: a binary that cannot say where it came from
// should say so, rather than claim a release it is not.
var (
	version = "0.2.0-dev"
	commit  = ""
	date    = ""
)

// BuildInfo describes exactly which binary is running.
//
// An operator debugging a cluster needs to know what is deployed, and "the
// version in the source tree" is not an answer when several builds of the same
// version exist. The commit is what makes a report reproducible.
type BuildInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit,omitempty"`
	Date     string `json:"date,omitempty"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
	// Modified reports whether the working tree had uncommitted changes when
	// this binary was built, which is the difference between a reproducible
	// build and one only its author can recreate.
	Modified bool `json:"modified,omitempty"`
}

// buildInfo assembles what this binary knows about itself.
//
// Values injected at link time win. Otherwise the Go toolchain's own VCS stamps
// are used, which means a plain `go build` still produces a binary that can
// identify its commit.
func buildInfo() BuildInfo {
	info := BuildInfo{
		Version:  version,
		Commit:   commit,
		Date:     date,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	settings, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, setting := range settings.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = setting.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = setting.Value
			}
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}
	return info
}

// String renders build info for a terminal.
func (b BuildInfo) String() string {
	var out strings.Builder
	out.WriteString(b.Version)
	if b.Commit != "" {
		short := b.Commit
		if len(short) > 12 {
			short = short[:12]
		}
		out.WriteString(" (" + short)
		if b.Modified {
			// A dirty build is worth calling out: whatever an operator reads
			// from the source tree may not be what this binary contains.
			out.WriteString(", modified")
		}
		out.WriteString(")")
	}
	out.WriteString("\n" + b.Go + " " + b.Platform)
	if b.Date != "" {
		out.WriteString("\nbuilt " + b.Date)
	}
	return out.String()
}

// showVersion prints build information.
func showVersion(args []string) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	short := flags.Bool("short", false, "print only the version string")
	if err := flags.Parse(args); err != nil {
		return err
	}

	info := buildInfo()
	switch {
	case *jsonOutput:
		return json.NewEncoder(os.Stdout).Encode(info)
	case *short:
		fmt.Println(info.Version)
	default:
		fmt.Println(info)
	}
	return nil
}
