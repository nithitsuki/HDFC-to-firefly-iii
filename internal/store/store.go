// Package store persists which messages have been processed, so that a restart
// or an overlapping poll never imports the same alert twice.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo required
)

// Status describes the outcome of processing one message.
type Status string

const (
	// StatusPending means a run has claimed the message and is working on it.
	// A record left in this state means the run died mid-import.
	StatusPending   Status = "pending"
	StatusImported  Status = "imported"  // posted to Firefly III
	StatusDuplicate Status = "duplicate" // Firefly already had it
	StatusSkipped   Status = "skipped"   // parsed, but not importable (e.g. unknown account)
	StatusFailed    Status = "failed"    // parse or API error
	StatusDryRun    Status = "dry_run"   // would have been imported

	// StatusIgnored means the message must never be imported. It is set by
	// hand when a transaction is already recorded in Firefly another way, and
	// is deliberately excluded from replay: retrying it would recreate a
	// duplicate that was removed on purpose.
	StatusIgnored Status = "ignored"
)

// Record is one processed message.
type Record struct {
	MessageID   string
	UID         uint32
	Subject     string
	Kind        string
	AmountMinor int64
	OccurredAt  time.Time
	Status      Status
	Detail      string
	FireflyID   string
	ProcessedAt time.Time
}

// Store is a SQLite-backed record of processed messages.
type Store struct{ db *sql.DB }

// Open opens (and if necessary creates) the database at path.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite handles one writer at a time; keeping the pool small avoids
	// spurious SQLITE_BUSY errors under the poll loop.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS messages (
	message_id   TEXT PRIMARY KEY,
	uid          INTEGER NOT NULL DEFAULT 0,
	subject      TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL DEFAULT '',
	amount_minor INTEGER NOT NULL DEFAULT 0,
	occurred_at  TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL,
	detail       TEXT NOT NULL DEFAULT '',
	firefly_id   TEXT NOT NULL DEFAULT '',
	processed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_status ON messages(status);
CREATE INDEX IF NOT EXISTS messages_kind ON messages(kind);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Claim takes ownership of a message before any work is done, so that two runs
// cannot both import the same alert.
//
// It reports whether the claim succeeded. false means the message was already
// claimed or finished, and the caller must not process it again. The insert is
// a single statement, so it is atomic even across processes: a systemd timer
// firing while a manual run is in flight cannot double-import.
func (s *Store) Claim(r Record) (bool, error) {
	if r.ProcessedAt.IsZero() {
		r.ProcessedAt = time.Now()
	}
	res, err := s.db.Exec(`
INSERT INTO messages (message_id, uid, subject, kind, amount_minor, occurred_at, status, detail, firefly_id, processed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, '', '', ?)
ON CONFLICT(message_id) DO NOTHING`,
		r.MessageID, r.UID, r.Subject, r.Kind, r.AmountMinor, formatTime(r.OccurredAt),
		string(StatusPending), r.ProcessedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("store: claim %s: %w", r.MessageID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim %s: %w", r.MessageID, err)
	}
	return n == 1, nil
}

// Finish records the outcome for a message that was previously claimed.
func (s *Store) Finish(r Record) error {
	if r.ProcessedAt.IsZero() {
		r.ProcessedAt = time.Now()
	}
	_, err := s.db.Exec(`
UPDATE messages
SET status = ?, detail = ?, firefly_id = ?, kind = ?, amount_minor = ?, occurred_at = ?, processed_at = ?
WHERE message_id = ?`,
		string(r.Status), r.Detail, r.FireflyID, r.Kind, r.AmountMinor,
		formatTime(r.OccurredAt), r.ProcessedAt.UTC().Format(time.RFC3339), r.MessageID)
	if err != nil {
		return fmt.Errorf("store: finish %s: %w", r.MessageID, err)
	}
	return nil
}

// ReleasePending marks messages left pending by an interrupted run as failed,
// so they show up as replayable rather than being stuck forever. It returns how
// many were released.
//
// Replaying a released message is safe: the Firefly-side external_id check
// catches the case where the transaction was created before the crash.
func (s *Store) ReleasePending() (int, error) {
	res, err := s.db.Exec(`
UPDATE messages SET status = ?, detail = 'interrupted before completion; replayed on startup'
WHERE status = ?`, string(StatusFailed), string(StatusPending))
	if err != nil {
		return 0, fmt.Errorf("store: release pending: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: release pending: %w", err)
	}
	return int(n), nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ByStatus lists recorded messages with the given status, newest first.
func (s *Store) ByStatus(status Status, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`
SELECT message_id, uid, subject, kind, amount_minor, occurred_at, status, detail, firefly_id, processed_at
FROM messages WHERE status = ? ORDER BY processed_at DESC LIMIT ?`, string(status), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", status, err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var (
			r          Record
			occ, procS string
			st         string
		)
		if err := rows.Scan(&r.MessageID, &r.UID, &r.Subject, &r.Kind, &r.AmountMinor,
			&occ, &st, &r.Detail, &r.FireflyID, &procS); err != nil {
			return nil, fmt.Errorf("store: scan: %w", err)
		}
		r.Status = Status(st)
		r.OccurredAt, _ = time.Parse(time.RFC3339, occ)
		r.ProcessedAt, _ = time.Parse(time.RFC3339, procS)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes records by message ID. Replay uses this to put messages back
// into an unclaimed state so the normal claim path can take them again.
func (s *Store) Delete(messageIDs ...string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: delete: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`DELETE FROM messages WHERE message_id = ?`)
	if err != nil {
		return fmt.Errorf("store: delete: %w", err)
	}
	defer stmt.Close()

	for _, id := range messageIDs {
		if _, err := stmt.Exec(id); err != nil {
			return fmt.Errorf("store: delete %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// Counts summarises the message table by status, for the status command.
func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM messages GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: counts: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: scan counts: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// GetMeta reads a value from the key/value table, returning "" if unset.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get meta %s: %w", key, err)
	}
	return v, nil
}

const metaLastPollOK = "last_poll_ok"

// MarkPollOK records that a poll completed successfully. The healthcheck
// command reads this, so the container runtime can tell a working service from
// one that is up but failing every cycle.
func (s *Store) MarkPollOK(t time.Time) error {
	return s.SetMeta(metaLastPollOK, t.UTC().Format(time.RFC3339))
}

// LastPollOK returns the time of the last successful poll, or the zero time if
// none has completed yet.
func (s *Store) LastPollOK() (time.Time, error) {
	v, err := s.GetMeta(metaLastPollOK)
	if err != nil {
		return time.Time{}, err
	}
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: bad %s value %q: %w", metaLastPollOK, v, err)
	}
	return t, nil
}

// SetMeta writes a value to the key/value table.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: set meta %s: %w", key, err)
	}
	return nil
}
