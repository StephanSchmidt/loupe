package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Run status values stored in runs.status.
const (
	RunStatusRunning = "running"
	RunStatusOK      = "ok"
	RunStatusError   = "error"
)

// StartRun records the beginning of a baseline or run in the runs table and
// returns the new row id. kind is "baseline" or "run". The row is left in
// status "running" until FinishRun closes it out — so an interrupted process
// leaves a visible "running" row rather than silently dropping the attempt.
func (s *Store) StartRun(ctx context.Context, kind string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (kind, started_at, status) VALUES (?, ?, ?)`,
		kind, time.Now().UTC().Unix(), RunStatusRunning)
	if err != nil {
		return 0, fmt.Errorf("start run (%s): %w", kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("start run (%s): %w", kind, err)
	}
	return id, nil
}

// FinishRun closes out a run row started by StartRun, stamping finished_at,
// the final status ("ok" or "error"), the newest indexed commit time, and a
// free-text note (typically the one-line summary). lastCommitAt may be the
// zero NullInt64 when nothing has been indexed.
func (s *Store) FinishRun(ctx context.Context, id int64, status string, lastCommitAt sql.NullInt64, notes string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET finished_at = ?, status = ?, last_ingested_commit_at = ?, notes = ? WHERE id = ?`,
		time.Now().UTC().Unix(), status, lastCommitAt, notes, id)
	if err != nil {
		return fmt.Errorf("finish run %d: %w", id, err)
	}
	return nil
}

// MaxCommitTime returns the newest committed_at across all commits for a
// provider, as a NullInt64 (invalid when the store holds no commits yet).
// Used to stamp runs.last_ingested_commit_at.
func (s *Store) MaxCommitTime(ctx context.Context, provider string) (sql.NullInt64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(committed_at) FROM commits WHERE provider = ?`, provider).Scan(&ts)
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("max commit time for %s: %w", provider, err)
	}
	return ts, nil
}
