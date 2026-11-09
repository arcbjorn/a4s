package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/arcbjorn/agentic-git/pkg/gitcmd"

	"github.com/arcbjorn/a4s/control"
	a4snode "github.com/arcbjorn/a4s/node"
	"github.com/arcbjorn/a4s/obs"
	"github.com/arcbjorn/a4s/server"
	"github.com/arcbjorn/a4s/source"
)

// runServer starts the control plane against a durable event log and reports
// the world it recovered. Recovery is the normal startup path, so the same code
// runs whether the log is empty or holds a full history.
func (c *cli) runServer(args []string) error {
	flags := c.flagSet("server")
	eventLog := flags.String("event-log", "", "absolute path to the durable event log")
	file := flags.String("file", "", "optional scenario supplying node inventory and approvals")
	statusOnly := flags.Bool("status", false, "report recovered state and exit")
	listen := flags.String("listen", "", "address to accept enrolled node connections on")
	keyID := flags.String("key-id", "control-1", "identifier for this server signing key")
	signingKeyPath := flags.String("signing-key", "", "path to the base64 Ed25519 private signing key")
	nodeKeyDir := flags.String("node-keys", "", "directory of <node-id>.pub enrolled node keys")
	operatorKeyDir := flags.String("operator-keys", "", "directory of <key-id>.pub operator keys")
	requireEncryption := flags.Bool("require-encryption", false,
		"refuse nodes that do not negotiate an encrypted channel")
	requireAttestation := flags.Bool("require-attestation", false,
		"refuse evidence a node did not sign with its identity key")
	anchorPath := flags.String("anchor", "",
		"absolute path to the external chain-head anchor (empty disables it)")
	imageSignerDir := flags.String("image-signers", "",
		"directory of <key-id>.pub build signer keys for image provenance")
	requireSignedImages := flags.Bool("require-signed-images", false,
		"refuse any image without a valid provenance attestation")
	maxCPU := flags.Int("cluster-cpu", 0,
		"cluster-wide cpu ceiling in millis; zero means no ceiling")
	maxMemory := flags.Int("cluster-memory", 0,
		"cluster-wide memory ceiling in MB; zero means no ceiling")
	maxAllocations := flags.Int("cluster-allocations", 0,
		"cluster-wide allocation ceiling; zero means no ceiling")
	maxTokens := flags.Int("cluster-tokens", 0,
		"cluster-wide agent token ceiling; zero means no ceiling")
	maxCost := flags.Int("cluster-cost-millis", 0,
		"cluster-wide agent cost ceiling in millis; zero means no ceiling")
	gitRemote := flags.String("git-remote", "",
		"repository to read goals from (empty disables the git source)")
	gitRef := flags.String("git-ref", "main", "branch or tag to track")
	gitPath := flags.String("git-path", "goals", "directory of goal documents in the repository")
	gitMirror := flags.String("git-mirror", "", "absolute path for the local bare mirror")
	gitInterval := flags.Duration("git-interval", source.DefaultPollInterval,
		"how often to poll the tracked ref")
	gitSSHKey := flags.String("git-ssh-key", "", "SSH identity for the repository")
	apiListen := flags.String("api", "", "address to serve the operator API on (empty disables it)")
	logLevel := flags.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	logFormat := flags.String("log-format", "text", "log format: text or json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *eventLog == "" {
		return fmt.Errorf("event-log is required")
	}
	// Everything a serving run needs is checked before the event log is
	// opened. Discovering a missing flag after recovery, a listener, and an
	// API had already started meant failing halfway through bringing up a
	// control plane, which is the worst moment to find out.
	if !*statusOnly {
		if *listen == "" {
			return fmt.Errorf("listen is required to serve; use --status to inspect recovered state")
		}
		if *signingKeyPath == "" || *nodeKeyDir == "" {
			return fmt.Errorf("signing-key and node-keys are required to serve")
		}
		if *gitRemote != "" && *gitMirror == "" {
			return fmt.Errorf("git-mirror is required with git-remote")
		}
	}
	logger, err := obs.New(obs.Config{
		Level: *logLevel, Format: obs.Format(*logFormat), Component: "server",
	})
	if err != nil {
		return err
	}

	base := control.World{}
	var goal *control.Goal
	if *file != "" {
		scenario, err := loadScenario(*file)
		if err != nil {
			return err
		}
		base = scenario.World
		goal = &scenario.Goal
	}

	// Operator keys are what let the server accept an approval or an API call.
	// Without them the control plane still reconciles, but no new authority can
	// enter it, which is the correct posture for a server nobody has told who
	// its operators are.
	operatorKeys := map[string]ed25519.PublicKey{}
	if *operatorKeyDir != "" {
		loaded, err := loadNodeKeys(*operatorKeyDir)
		if err != nil {
			return fmt.Errorf("load operator keys: %w", err)
		}
		operatorKeys = loaded
	}
	if *apiListen != "" && len(operatorKeys) == 0 {
		return fmt.Errorf("operator-keys is required to serve the API")
	}

	imageSigners := map[string]ed25519.PublicKey{}
	if *imageSignerDir != "" {
		loaded, err := loadNodeKeys(*imageSignerDir)
		if err != nil {
			return fmt.Errorf("load image signer keys: %w", err)
		}
		imageSigners = loaded
	}
	// Requiring provenance with nobody trusted to attest would refuse every
	// image, so the contradiction is reported at startup rather than as an
	// unexplained denial on the first goal.
	if *requireSignedImages && len(imageSigners) == 0 {
		return fmt.Errorf("image-signers is required to require signed images")
	}

	instance, openErr := server.Open(server.Config{
		EventLog: *eventLog, Base: base, OperatorKeys: operatorKeys,
		Anchor: *anchorPath, ImageSigners: imageSigners,
		RequireSignedImages: *requireSignedImages,
		ClusterCeiling: control.Resources{
			CPUMillis: *maxCPU, MemoryMB: *maxMemory,
		},
		MaxAllocations: *maxAllocations,
		ClusterBudget: control.Budget{
			Tokens: *maxTokens, CostMillis: *maxCost,
		},
	}, control.RemediationAgent{}, control.RolloutAgent{},
		control.PlacementAgent{}, control.NetworkAgent{})
	if openErr != nil {
		return openErr
	}
	defer instance.Close()

	if goal != nil {
		if err := instance.Submit(*goal); err != nil {
			return err
		}
	}
	status := instance.Status()
	if *statusOnly {
		// --status is an operator query, so its answer goes to stdout where it
		// can be read or piped, not into the log stream.
		fmt.Fprintf(c.stdout, "recovered revision %d from %d events: %d nodes, %d allocations, %d routes, %d goals\n",
			status.Revision, status.Events, status.Nodes, status.Allocations, status.Routes, status.Goals)
		return nil
	}
	logger.Info("recovered world from event log",
		slog.Uint64("revision", status.Revision),
		slog.Uint64("events", status.Events),
		slog.Int("nodes", status.Nodes),
		slog.Int("allocations", status.Allocations),
		slog.Int("routes", status.Routes),
		slog.Int("goals", status.Goals))
	signingKey, err := loadPrivateKey(*signingKeyPath)
	if err != nil {
		return err
	}
	nodeKeys, err := loadNodeKeys(*nodeKeyDir)
	if err != nil {
		return err
	}

	registry := a4snode.NewRegistry()
	defer registry.CloseAll()
	listener, err := a4snode.Listen("tcp", *listen, registry, a4snode.ListenerConfig{
		NodeKeys: nodeKeys, ServerKeyID: *keyID,
		RequireEncryption: *requireEncryption,
		OnError: func(err error) {
			// A rejected enrollment is a security-relevant event, not noise:
			// it is what an unenrolled or misconfigured peer looks like.
			logger.Warn("enrollment rejected", slog.Any("error", err))
		},
	})
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, cancel := signalContext()
	defer cancel()
	go func() {
		if err := listener.Serve(ctx); err != nil {
			logger.Error("listener stopped", slog.Any("error", err))
		}
	}()
	logger.Info("accepting enrolled nodes",
		slog.String("addr", listener.Addr().String()),
		slog.String("key_id", *keyID))

	metrics := obs.NewMetrics()
	if *apiListen != "" {
		api := server.NewAPI(instance, server.APIConfig{Logger: logger, Metrics: metrics})
		httpServer := &http.Server{
			Addr:    *apiListen,
			Handler: api.Handler(),
			// Timeouts bound how long one caller can hold a connection, so a
			// slow or stalled client cannot accumulate against the control
			// plane until it runs out of file descriptors.
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			logger.Info("serving operator API", slog.String("addr", *apiListen))
			if err := httpServer.ListenAndServe(); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				logger.Error("operator API stopped", slog.Any("error", err))
			}
		}()
		defer func() {
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			_ = httpServer.Shutdown(shutdownCtx)
		}()
	}

	// Goals may also arrive from a versioned repository. The adapter submits
	// through the same admission path as the operator API, so a repository is a
	// transport rather than a privileged channel.
	if *gitRemote != "" {
		mirror := *gitMirror
		git := &gitcmd.Runner{SSHKey: *gitSSHKey}
		if err := git.Available(); err != nil {
			return err
		}
		repository := source.NewRepository(
			gitcmd.NewSource(git, mirror, *gitRemote, *gitRef), *gitPath, instance)
		go watchGitSource(ctx, logger, repository, *gitInterval)
	}

	executor := a4snode.NewRegistryExecutor(registry, *keyID, signingKey)
	// Evidence is the only input that advances the world, so the executor
	// verifies each node's signature over what it reports rather than trusting
	// the authenticated channel to stand behind the claim.
	executor.NodeKeys = nodeKeys
	executor.RequireAttestation = *requireAttestation

	// Readiness is measured on the node holding each allocation, on the same
	// terms. Without this the control plane observes readiness once, when an
	// allocation is created, and then lets it expire with nothing to refresh
	// it: goals stop being satisfied and routes lose their endpoints for want
	// of a measurement nobody was taking.
	observer := a4snode.NewRegistryObserver(registry, *keyID, signingKey)
	observer.NodeKeys = nodeKeys
	observer.RequireAttestation = *requireAttestation
	return reconcileLoop(ctx, logger, instance, executor, observer, registry, metrics)
}

// watchGitSource tracks a repository, logging what each sync applied.
//
// A failed sync is logged and retried rather than fatal: an unreachable remote
// is a network problem, and a control plane that stopped tracking its repository
// over a blip would need manual intervention to resume.
func watchGitSource(ctx context.Context, logger *slog.Logger,
	repository *source.Repository, interval time.Duration) {

	_ = repository.Watch(ctx, interval, func(result source.Result, err error) {
		if err != nil {
			logger.Warn("git source sync failed", slog.Any("error", err))
			return
		}
		if !result.Changed {
			return
		}
		logger.Info("applied goals from git",
			slog.String("commit", result.Commit),
			slog.String("message", result.Message),
			slog.Int("goals", len(result.Goals)))
		for file, reason := range result.Rejected {
			// A rejected goal is the operator's problem to fix, so name the file.
			logger.Warn("git source rejected a goal",
				slog.String("file", file), slog.String("reason", reason))
		}
	})
}

// reconcileLoop drives accepted goals whenever nodes are connected. A goal that
// cannot converge is reported and retried rather than terminating the server,
// because the cause is usually a node that has not connected yet.
func reconcileLoop(ctx context.Context, logger *slog.Logger, instance *server.Server,
	executor *a4snode.RegistryExecutor, observer *a4snode.RegistryObserver,
	registry *a4snode.Registry, metrics *obs.Metrics) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil
		case <-ticker.C:
			connected := registry.Nodes()
			metrics.SetGauge("a4s_connected_nodes", int64(len(connected)))
			// Reachability is recorded before reconciling, so this round's
			// placement decisions are made against what the control plane can
			// actually talk to rather than against last tick's belief.
			if err := instance.ObserveNodes(connected); err != nil {
				logger.Warn("record node reachability", slog.Any("error", err))
			}
			if len(connected) == 0 {
				continue
			}
			for _, goal := range instance.Goals() {
				metrics.Count("a4s_reconciliations_total")
				// The prober is rebuilt per goal because its target set is the
				// engine's, and the engine is rebuilt per reconciliation. The
				// observer underneath keeps its node connections.
				prober := control.NewMeasuredProber(observer, map[string]control.ProbeTarget{})
				if err := instance.Reconcile(goal.ID, executor, prober); err != nil {
					// A goal held back by a safeguard is not a failure. Counting
					// it as one would make a working governor indistinguishable
					// from a broken cluster on every dashboard watching this.
					var pacing control.Pacing
					if errors.As(err, &pacing) {
						metrics.Count("a4s_reconciliations_paced_total")
						logger.Info("goal paced by a safeguard",
							slog.String("goal", goal.ID),
							slog.String("reason", pacing.Reason),
							slog.Time("until", pacing.Until))
						continue
					}
					metrics.Count("a4s_reconcile_failures_total")
					logger.Warn("reconcile failed",
						slog.String("goal", goal.ID), slog.Any("error", err))
					continue
				}
				status := instance.Status()
				logger.Info("goal converged",
					slog.String("goal", goal.ID),
					slog.Uint64("revision", status.Revision),
					slog.Int("allocations", status.Allocations),
					slog.Int("routes", status.Routes))
			}
		}
	}
}
