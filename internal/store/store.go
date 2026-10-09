package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// SchemaVersion is written to meta.schema_version and emitted by export.
//
// 1 -> 2 added tasks.output (docs/task-outputs.md §7).
// 2 -> 3 added tasks.owner (docs/task-owners.md §5).
//
// The bump is what makes migrate() able to tell a database that already has a
// column from one that predates it, because CREATE TABLE IF NOT EXISTS is a
// no-op on a table that already exists.
const SchemaVersion = 3

// Statuses. There are exactly four and there will not be a fifth.
const (
	StatusTodo      = "todo"
	StatusDoing     = "doing"
	StatusDone      = "done"
	StatusCancelled = "cancelled"
)

// Owners. There are exactly two and they describe who is expected to do the
// work, not whether it can be started: a human-owned task is as ready as an
// agent-owned one, and the frontier math never consults this field
// (docs/task-owners.md §2).
const (
	OwnerAgent = "agent"
	OwnerHuman = "human"
)

// ValidOwner reports whether o is one of the two owners.
func ValidOwner(o string) bool {
	return o == OwnerAgent || o == OwnerHuman
}

// OwnerOrDefault maps an empty owner to the default. A caller that omits the
// field gets the boring answer — agent — never an error.
func OwnerOrDefault(o string) string {
	if o == "" {
		return OwnerAgent
	}
	return o
}

// isTerminal reports whether a status unblocks downstream work. This is the
// only place the terminal set is defined.
func isTerminal(s string) bool { return s == StatusDone || s == StatusCancelled }

// ValidStatus reports whether s is one of the four statuses.
func ValidStatus(s string) bool {
	switch s {
	case StatusTodo, StatusDoing, StatusDone, StatusCancelled:
		return true
	}
	return false
}

// Store owns one SQLite file. Both graphd subcommands open it directly; there
// is no daemon.
type Store struct {
	db   *sql.DB
	path string
}

// Path returns the on-disk path of the database.
func (s *Store) Path() string { return s.path }

// DB exposes the underlying handle (tests only).
func (s *Store) DB() *sql.DB { return s.db }

// Open creates the parent directory if needed, opens the database, applies the
// required pragmas, and migrates.
//
// Concurrency (SPEC §3.2): every connection gets WAL, a 5s busy timeout,
// foreign keys on and synchronous=NORMAL. SetMaxOpenConns(1) means writers are
// fully serialised inside the process, which removes SQLITE_BUSY retry logic
// from the picture entirely. Cross-process serialisation is handled by WAL plus
// busy_timeout.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	if err := s.addColumns(); err != nil {
		return err
	}
	seed := [][2]string{
		{"revision", "0"},
		{"schema_version", fmt.Sprint(SchemaVersion)},
	}
	for _, kv := range seed {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			return fmt.Errorf("store: seed meta %s: %w", kv[0], err)
		}
	}
	// The schema version is a fact about the schema, not a seed: an existing
	// database must be moved forward, not left reporting the version it was
	// created at.
	if _, err := s.db.Exec(
		`UPDATE meta SET value = ? WHERE key = 'schema_version'`, fmt.Sprint(SchemaVersion)); err != nil {
		return fmt.Errorf("store: update schema_version: %w", err)
	}
	return nil
}

// addColumns applies additive column migrations that CREATE TABLE IF NOT EXISTS
// cannot express. Adding a column to the CREATE statement only affects freshly
// created databases; a database that already exists would keep the old shape and
// every query naming the new column would fail at runtime. Each step therefore
// checks for the column first, which also makes this safe to run on every open.
func (s *Store) addColumns() error {
	type colMigration struct {
		table string
		col   string
		ddl   string
	}
	steps := []colMigration{
		{"tasks", "output", `ALTER TABLE tasks ADD COLUMN output TEXT NOT NULL DEFAULT ''`},
		{"tasks", "owner", `ALTER TABLE tasks ADD COLUMN owner TEXT NOT NULL DEFAULT 'agent'`},
	}
	for _, st := range steps {
		has, err := s.hasColumn(st.table, st.col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(st.ddl); err != nil {
			return fmt.Errorf("store: migrate %s.%s: %w", st.table, st.col, err)
		}
	}
	return nil
}

// hasColumn reports whether table already has a column named col.
func (s *Store) hasColumn(table, col string) (bool, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("store: inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// tx runs fn in a transaction. Every mutation in this package goes through it,
// and every mutation must also call bumpRevision inside the same transaction
// (SPEC §9.1) — a reader must never observe a revision that does not describe
// the data.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// bumpRevision increments meta.revision by one. Must be called inside the
// transaction that performs the mutation.
func bumpRevision(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT) WHERE key = 'revision'`)
	if err != nil {
		return fmt.Errorf("store: bump revision: %w", err)
	}
	return nil
}

// Revision returns the current monotonic revision counter.
func (s *Store) Revision(ctx context.Context) (int64, error) {
	var rev int64
	err := s.db.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'revision'`).Scan(&rev)
	if err != nil {
		return 0, fmt.Errorf("store: read revision: %w", err)
	}
	return rev, nil
}

// nowUTC is the one timestamp format used everywhere: RFC3339, UTC, second
// resolution.
func nowUTC() string {
	return time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
}
