package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
	"github.com/arcbjorn/a4s/obs"
	"github.com/arcbjorn/a4s/server"
)

// DefaultFollowInterval is how often a standby asks the primary for new records.
const DefaultFollowInterval = 5 * time.Second

// standby follows a primary's history into a local replica.
//
// Losing the control plane does not stop workloads; nodes keep running what they
// were told to run. What is lost is the ability to change anything, and that is
// what a follower shortens. It is not consensus and holds no election: there is
// one writer, and promotion is a decision a person or a supervisor makes.
//
// What this adds is the part that is easy to get wrong by hand. A replica that
// had fallen behind and was promoted anyway would silently roll history
// backwards, resurrecting revoked approvals and forgetting allocations that
// exist. Every record is re-derived against the follower's own chain rather than
// copied, and the head it reaches is witnessed in an anchor, so a later `a4s
// server` against the same log and anchor refuses to start if it is behind.
//
// Promotion is therefore not a mode of this command: it is starting `a4s server`
// on the replica's log and anchor, which runs the same recovery, verification,
// and anchor check as any other start. A promotion-only shortcut would be a
// second path nothing else exercises.
func standby(args []string) error {
	flags := flag.NewFlagSet("standby", flag.ContinueOnError)
	connection := registerClientFlags(flags)
	eventLog := flags.String("event-log", "", "absolute path to this replica's event log")
	anchorPath := flags.String("anchor", "",
		"absolute path to this replica's chain-head anchor; required")
	file := flags.String("file", "",
		"scenario supplying the same base world the primary was started with")
	interval := flags.Duration("interval", DefaultFollowInterval,
		"how often to ask the primary for new records")
	once := flags.Bool("once", false, "sync once and report, rather than following")
	logLevel := flags.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	logFormat := flags.String("log-format", "text", "log format: text or json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" || *anchorPath == "" {
		return fmt.Errorf("event-log and anchor are required")
	}
	logger, err := obs.New(obs.Config{
		Level: *logLevel, Format: obs.Format(*logFormat), Component: "standby",
	})
	if err != nil {
		return err
	}
	client, err := connection.client()
	if err != nil {
		return err
	}

	// The base world carries what the log does not: node inventory, capacity,
	// and any approval granted outside recorded history. A follower configured
	// without it promotes successfully and comes up missing exactly those
	// facts, which is a quiet failure rather than a loud one.
	base := control.World{}
	if *file != "" {
		scenario, err := loadScenario(*file)
		if err != nil {
			return err
		}
		base = scenario.World
	} else {
		logger.Warn("no base scenario supplied; a promoted replica will have no node inventory")
	}

	follower, err := server.OpenStandby(server.Config{
		EventLog: *eventLog, Anchor: *anchorPath, Base: base,
	})
	if err != nil {
		return err
	}
	defer follower.Close()

	if *once {
		return syncStandby(logger, client, follower)
	}

	ctx, stop := signalContext()
	defer stop()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	logger.Info("following primary",
		slog.String("event_log", *eventLog), slog.Duration("interval", *interval))
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil
		case <-ticker.C:
			if err := syncStandby(logger, client, follower); err != nil {
				// A primary that is unreachable is the case a standby exists
				// for. Retrying is the whole job, so this is logged and not
				// fatal.
				logger.Warn("sync failed", slog.Any("error", err))
			}
		}
	}
}

// syncStandby fetches everything the follower is missing and applies it.
//
// It loops rather than making one request because the primary batches: a
// follower that had been down for a day would otherwise catch up one batch per
// tick and never converge on a busy cluster.
func syncStandby(logger *slog.Logger, client *operatorClient, follower *server.Standby) error {
	applied := 0
	for {
		head := follower.Head()
		answer, err := client.do(http.MethodGet,
			fmt.Sprintf("/v1/records?after=%d&limit=%d", head.Sequence, server.MaxRecordBatch), nil)
		if err != nil {
			return err
		}
		var records []eventlog.Record
		if err := json.Unmarshal(answer, &records); err != nil {
			return fmt.Errorf("decode records: %w", err)
		}
		if len(records) == 0 {
			break
		}
		count, err := follower.Ingest(records)
		if err != nil {
			// A divergence is not a transient failure. The two logs are not the
			// same history, and continuing to pull would be pulling from a
			// primary this replica can never legitimately become.
			return fmt.Errorf("ingest records after %d: %w", head.Sequence, err)
		}
		applied += count
		if count == 0 {
			// Nothing new was appended despite records arriving, which means
			// the follower already held them. Stop rather than spin.
			break
		}
	}

	head := follower.Head()
	if applied > 0 {
		logger.Info("replicated history",
			slog.Int("records", applied), slog.Uint64("head", head.Sequence))
	} else {
		logger.Debug("already caught up", slog.Uint64("head", head.Sequence))
	}
	return nil
}
