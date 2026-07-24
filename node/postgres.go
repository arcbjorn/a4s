package node

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// DefaultPostgresBackupTimeout bounds a pg_basebackup so a stuck backup cannot
// stall the storage agent forever.
const DefaultPostgresBackupTimeout = 30 * time.Minute

// PostgresEngine backs up and probes a PostgreSQL database using its own tools.
//
// It does not read the data directory. A running Postgres writes constantly, so
// a filesystem copy of its files is torn: pages half-written, the WAL and the
// heap out of step. pg_basebackup produces a copy Postgres can actually start
// from, and a real connection is the only proof it accepts queries.
type PostgresEngine struct {
	// User is the role backups and probes connect as.
	User string
	// Database is the maintenance database to connect to for readiness.
	Database string
	// BinDir holds pg_basebackup, if it is not on PATH.
	BinDir string
	// PasswordFile is a path whose contents are the connection password. It is
	// a file, not a field, so a password never sits in the process's memory as
	// a plain string longer than the connection.
	PasswordFile string
	// Timeout bounds a backup.
	Timeout time.Duration
	// probe opens a connection for readiness. Injectable so tests do not need a
	// live Postgres.
	probe func(ctx context.Context, dsn string) error
}

func NewPostgresEngine(user, database string) *PostgresEngine {
	return &PostgresEngine{
		User: user, Database: database, Timeout: DefaultPostgresBackupTimeout,
	}
}

func (e *PostgresEngine) Name() string { return "postgres" }

// Backup runs pg_basebackup against the live database.
//
// pg_basebackup takes a consistent base backup while the server keeps serving:
// it copies the data directory through the replication protocol and includes
// the WAL needed to reach a consistent point, so the result starts cleanly.
func (e *PostgresEngine) Backup(ctx context.Context, allocation, address string, port int, destination string) error {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultPostgresBackupTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	binary := "pg_basebackup"
	if e.BinDir != "" {
		binary = e.BinDir + "/pg_basebackup"
	}
	args := []string{
		"--host", address,
		"--port", strconv.Itoa(port),
		"--username", e.User,
		"--pgdata", destination,
		// Plain format with the WAL included, so the backup is self-contained
		// and does not depend on the server keeping WAL segments around.
		"--format", "plain",
		"--wal-method", "stream",
		"--no-password",
		"--checkpoint", "fast",
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = os.Environ()
	if e.PasswordFile != "" {
		password, err := os.ReadFile(e.PasswordFile)
		if err != nil {
			return fmt.Errorf("read postgres password: %w", err)
		}
		cmd.Env = append(cmd.Env, "PGPASSWORD="+string(bytes.TrimSpace(password)))
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_basebackup failed: %w: %s", err, stderr.String())
	}
	return nil
}

// Ready reports whether Postgres accepts connections and answers a query.
//
// A TCP probe passes the moment the port binds, which happens before recovery
// finishes. A database that is still replaying its WAL rejects queries with
// "the database system is starting up". Running an actual query is the only way
// to tell serving from listening.
func (e *PostgresEngine) Ready(ctx context.Context, allocation, address string, port int) (bool, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable connect_timeout=3",
		address, port, e.User, e.Database)
	probe := e.probe
	if probe == nil {
		probe = pingPostgres
	}
	if err := probe(ctx, dsn); err != nil {
		// Not ready is a normal answer during startup, not an error to escalate.
		return false, nil
	}
	return true, nil
}

// pingPostgres opens a connection and runs a trivial query. It is the default
// readiness probe; tests inject a fake to avoid a live database.
func pingPostgres(ctx context.Context, dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// SELECT 1 answers only once the database will accept queries, which is
	// exactly the readiness question.
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
