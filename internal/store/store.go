package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const (
	// DefaultPath is the on-disk location of the Loupe state database
	// relative to the working directory.
	DefaultPath = ".loupe/state.db"

	// MemoryPath opens an ephemeral in-memory database. Used by tests.
	MemoryPath = ":memory:"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if path != MemoryPath {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create state dir for %s: %w", path, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}

	// SQLite serialises writers; cap to one connection so concurrent callers
	// queue cleanly instead of hitting "database is locked".
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set %s: %w", pragma, err)
		}
	}

	s := &Store{db: db, path: path}
	if err := s.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureSchema() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return s.applyAdditiveColumns()
}

// additiveColumns is the list of ALTER TABLE ADD COLUMN statements that
// can't live in schema.sql because SQLite errors when the column already
// exists. applyAdditiveColumns reads PRAGMA table_info and skips columns
// that are already present, keeping Open idempotent.
type additiveColumn struct {
	table  string
	column string
	spec   string
}

var additiveColumns = []additiveColumn{
	{"commits", "provider", "TEXT NOT NULL DEFAULT ''"},
	{"commits", "workspace", "TEXT NOT NULL DEFAULT ''"},
	{"prs", "provider", "TEXT NOT NULL DEFAULT ''"},
	{"prs", "workspace", "TEXT NOT NULL DEFAULT ''"},
	{"prs", "author_login", "TEXT NOT NULL DEFAULT ''"},
	{"prs", "author_is_bot", "INTEGER NOT NULL DEFAULT 0"},
	{"tickets", "provider", "TEXT NOT NULL DEFAULT ''"},
	// pr_commits gained author + message columns in v0.2 to support
	// squash-merge trailer recovery — old databases were created with
	// just (pr_id, commit_sha) before the table was used by ingest.
	{"pr_commits", "author_email", "TEXT NOT NULL DEFAULT ''"},
	{"pr_commits", "author_name", "TEXT NOT NULL DEFAULT ''"},
	{"pr_commits", "message", "TEXT NOT NULL DEFAULT ''"},
}

func (s *Store) applyAdditiveColumns() error {
	for _, c := range additiveColumns {
		has, err := s.hasColumn(c.table, c.column)
		if err != nil {
			return fmt.Errorf("check %s.%s: %w", c.table, c.column, err)
		}
		if has {
			continue
		}
		// PRAGMA table_info args can't be parameterised; the table and
		// column come from the trusted in-package list above.
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.spec) // #nosec G201 -- table+column from trusted const
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table)) // #nosec G201 -- table from trusted const
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying connection pool for callers that need raw queries.
// Higher-level packages should prefer typed methods on Store, but Loupe's data
// layer is small enough that the *sql.DB escape hatch is fine for now.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Path() string {
	return s.path
}
