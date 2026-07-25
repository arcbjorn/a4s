package eventlog

import (
	"database/sql"
	"fmt"
)

// schemaVersion is the migration level this build expects.
//
// A store at a higher version is refused rather than opened: a newer a4s may
// have added columns this build would silently ignore, and the event log is the
// one piece of state where silently ignoring something is unacceptable.
const schemaVersion = 1

// migrations are applied in order, each inside its own transaction.
//
// The SQL here is deliberately restricted to what libSQL also implements, so
// pointing this store at Turso later is a driver and DSN change rather than a
// schema rewrite. That rules out nothing a4s currently needs.
var migrations = []struct {
	version int
	name    string
	stmts   []string
}{
	{
		version: 1,
		name:    "event log with hash chain",
		stmts: []string{
			// The chain is enforced by the schema rather than only by code.
			//
			// sequence is the primary key and starts at 1, so a gap or a
			// duplicate is a constraint violation rather than something replay
			// has to notice. previous_hash and hash are the chain links, and
			// hash is unique because two records with the same hash would make
			// "which record does this one follow" ambiguous.
			//
			// STRICT rejects genuinely wrong types. It does allow SQLite's
			// documented INTEGER-to-TEXT coercion, so CHECK constraints carry
			// the invariants that actually matter.
			`CREATE TABLE records (
				sequence      INTEGER PRIMARY KEY CHECK (sequence > 0),
				previous_hash TEXT NOT NULL CHECK (
					(sequence = 1 AND previous_hash = '') OR
					(sequence > 1 AND length(previous_hash) = 64)
				),
				hash          TEXT NOT NULL UNIQUE CHECK (length(hash) = 64),
				recorded_at   TEXT NOT NULL,
				event         TEXT NOT NULL
			) STRICT`,

			// Replay reads every record in sequence order, which the primary
			// key already serves. This index supports the history queries that
			// narrow by time without scanning the event JSON.
			`CREATE INDEX records_recorded_at ON records (recorded_at)`,

			// A single-row table holding the head of the chain.
			//
			// It exists so an append can assert the chain it is extending
			// inside the same transaction, and so a reader can learn the tip
			// without loading every record.
			`CREATE TABLE chain_head (
				id       INTEGER PRIMARY KEY CHECK (id = 1),
				sequence INTEGER NOT NULL CHECK (sequence >= 0),
				hash     TEXT NOT NULL
			) STRICT`,

			`INSERT INTO chain_head (id, sequence, hash) VALUES (1, 0, '')`,
		},
	},
}

// migrate brings a database up to schemaVersion.
//
// Each migration runs in its own transaction with the version bump inside it,
// so an interrupted migration leaves the store at the last complete version
// rather than half-applied.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	var current int
	if err := db.QueryRow(
		`SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > schemaVersion {
		// Opening a store written by a newer build risks interpreting its
		// history through a schema that no longer describes it.
		return fmt.Errorf(
			"event log is at schema version %d but this build understands %d: upgrade a4s",
			current, schemaVersion)
	}

	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		transaction, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.version, err)
		}
		for _, statement := range migration.stmts {
			if _, err := transaction.Exec(statement); err != nil {
				transaction.Rollback()
				return fmt.Errorf("migration %d (%s): %w",
					migration.version, migration.name, err)
			}
		}
		if _, err := transaction.Exec(
			`INSERT INTO schema_migrations (version, name, applied_at)
			 VALUES (?, ?, datetime('now'))`,
			migration.version, migration.name); err != nil {
			transaction.Rollback()
			return fmt.Errorf("record migration %d: %w", migration.version, err)
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.version, err)
		}
	}
	return nil
}
