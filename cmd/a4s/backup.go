package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/arcbjorn/a4s/eventlog"
)

// backup writes a verified copy of authoritative controller state.
//
// The event log is the only authoritative state a4s holds; everything else is
// derived from it. Backing it up is therefore the whole disaster-recovery
// story, and verifying the chain during the copy is what keeps a recovery point
// from being a guess.
func backup(args []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	out := flags.String("out", "", "absolute path to write the backup to")
	verifyOnly := flags.String("verify", "", "verify an existing backup and exit")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *verifyOnly != "" {
		manifest, err := eventlog.VerifyBackup(*verifyOnly)
		if err != nil {
			return err
		}
		return reportManifest(manifest, *jsonOutput, "verified")
	}

	if *eventLog == "" || *out == "" {
		return fmt.Errorf("event-log and out are required")
	}
	store, err := eventlog.Open(*eventLog)
	if err != nil {
		return err
	}
	defer store.Close()

	manifest, err := store.Backup(*out)
	if err != nil {
		return err
	}
	return reportManifest(manifest, *jsonOutput, "wrote")
}

// restore installs a verified backup.
//
// Verification happens before the destination is touched, so an operator
// recovering from a bad backup finds their existing log intact rather than
// overwritten by something unusable.
func restore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	from := flags.String("from", "", "absolute path to the backup archive")
	eventLog := flags.String("event-log", "", "absolute path to restore the log to")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" || *eventLog == "" {
		return fmt.Errorf("from and event-log are required")
	}

	manifest, err := eventlog.Restore(*from, *eventLog)
	if err != nil {
		return err
	}
	// Reopening proves the restored file is a log this build can actually
	// recover from, rather than only bytes that verified.
	store, err := eventlog.Open(*eventLog)
	if err != nil {
		return fmt.Errorf("restored log does not open: %w", err)
	}
	defer store.Close()

	return reportManifest(manifest, *jsonOutput, "restored")
}

func reportManifest(manifest eventlog.BackupManifest, asJSON bool, verb string) error {
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(manifest)
	}
	fmt.Printf("%s backup: %d records, head %d, %d bytes, taken %s\n",
		verb, manifest.Records, manifest.HeadSequence, manifest.Bytes,
		manifest.TakenAt.Format("2006-01-02 15:04:05 UTC"))
	if manifest.HeadHash != "" {
		fmt.Printf("head hash: %s\n", manifest.HeadHash)
	}
	return nil
}
