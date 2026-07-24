package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arcbjorn/a4s/control"
)

// DatabaseEngine is the narrow contract between the node and a database.
//
// It is deliberately not a SQL client. The node needs exactly two things a
// generic volume cannot provide: a consistent backup taken while the database
// serves, and an answer to whether the database accepts connections. Everything
// else about the database is the workload's own business.
type DatabaseEngine interface {
	// Backup produces a consistent copy into destination while the database is
	// running, using the engine's own tooling. A filesystem copy of a live
	// database is inconsistent; this is what replaces it.
	Backup(ctx context.Context, allocation, address string, port int, destination string) error
	// Ready reports whether the database accepts connections. A TCP probe only
	// proves the port is open, which a recovering database passes while
	// refusing every query.
	Ready(ctx context.Context, allocation, address string, port int) (bool, error)
	// Name is the engine this backend serves, matched against a workload's
	// declared engine.
	Name() string
}

// DatabaseManager handles database-specific actions on the node.
type DatabaseManager struct {
	engines map[string]DatabaseEngine
	// volumes locates where a database's data and backups live.
	volumes *Volumes
	// endpoints resolves an allocation's address, so a backup connects to the
	// database rather than guessing.
	endpoints func(allocation string) (address string, port int, ok bool)
}

func NewDatabaseManager(volumes *Volumes, endpoints func(string) (string, int, bool), engines ...DatabaseEngine) *DatabaseManager {
	byName := make(map[string]DatabaseEngine, len(engines))
	for _, engine := range engines {
		byName[engine.Name()] = engine
	}
	return &DatabaseManager{engines: byName, volumes: volumes, endpoints: endpoints}
}

func (m *DatabaseManager) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	if action.Kind != control.ActionDatabaseBackup {
		return control.Evidence{}, fmt.Errorf("database manager does not support %q", action.Kind)
	}
	if action.Volume == nil || action.Snapshot == "" {
		return control.Evidence{}, fmt.Errorf("database backup requires a volume reference and label")
	}
	engine, ok := m.engines[action.Engine]
	if !ok {
		return control.Evidence{}, fmt.Errorf("no backend for database engine %q", action.Engine)
	}

	name := action.Volume.Name
	record, ok := m.volumes.record(name)
	if !ok {
		return control.Evidence{}, fmt.Errorf("volume %q does not exist on this node", name)
	}
	address, port, ok := m.endpoints(record.Owner)
	if !ok || address == "" {
		return control.Evidence{}, fmt.Errorf(
			"cannot reach database for volume %q; it must be attached and running", name)
	}

	// The backup lands in the volume's snapshot area, so it is a first-class
	// snapshot that can be verified, pruned, and restored like any other.
	destination := filepath.Join(m.volumes.snapshots, name, action.Snapshot)
	if _, err := os.Stat(destination); err == nil {
		// Backup labels are immutable, like snapshot ids.
		checksum, err := checksumTree(destination)
		if err != nil {
			return control.Evidence{}, err
		}
		return databaseBackupEvidence(name, action.Snapshot, checksum, true), nil
	}
	staging := destination + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return control.Evidence{}, fmt.Errorf("clear staging backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return control.Evidence{}, fmt.Errorf("create backup directory: %w", err)
	}
	if err := engine.Backup(ctx, record.Owner, address, port, staging); err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("database backup of %q: %w", name, err)
	}
	checksum, err := checksumTree(staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return control.Evidence{}, fmt.Errorf("finalize database backup: %w", err)
	}
	return databaseBackupEvidence(name, action.Snapshot, checksum, false), nil
}

func databaseBackupEvidence(volume, label, checksum string, repeated bool) control.Evidence {
	return control.Evidence{
		Kind: control.EvidenceDatabaseBackedUp, Target: volume,
		Observed: map[string]string{
			"label": label, "checksum": checksum, "repeated": fmt.Sprintf("%t", repeated),
		},
	}
}

// ObserveReadiness implements the readiness observer for database probes. A
// database probe delegates to the engine's connection check; other probe kinds
// fall through to the caller's own observer.
func (m *DatabaseManager) ObserveReadiness(target control.ProbeTarget) (bool, map[string]string, error) {
	if target.Kind != control.ProbeDatabase {
		return false, nil, fmt.Errorf("database manager only serves database probes")
	}
	engine, ok := m.engines[target.Engine]
	if !ok {
		return false, nil, fmt.Errorf("no backend for database engine %q", target.Engine)
	}
	address, port, ok := m.endpoints(target.Allocation)
	if !ok || address == "" {
		return false, map[string]string{"reason": "no address for database"}, nil
	}
	ready, err := engine.Ready(context.Background(), target.Allocation, address, port)
	if err != nil {
		// A failed check is absence of evidence, not proof the database is down.
		return false, map[string]string{"reason": err.Error()}, nil
	}
	return ready, map[string]string{"engine": engine.Name(), "probe": "database"}, nil
}

func (m *DatabaseManager) Close() error { return nil }
