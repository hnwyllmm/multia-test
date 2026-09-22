package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type OutboxInput struct {
	EventKey string
	Kind     string
	IssueKey string
	Body     string
	Marker   string
}

type OutboxItem struct {
	ID       int64
	EventKey string
	Kind     string
	IssueKey string
	Body     string
	Marker   string
	Attempts int
}

type Round struct {
	Repo        string
	PullNumber  int
	HeadSHA     string
	BaseSHA     string
	IssueKey    string
	ReviewerIDs []string
}

type FeedbackInput struct {
	Repo       string
	EventID    int64
	PullNumber int
	IssueKey   string
	Body       string
	Metadata   map[string]any
	OccurredAt time.Time
}

type PendingFeedback struct {
	Kind       string
	Repo       string
	EventID    int64
	PullNumber int
	IssueKey   string
	Body       string
	Metadata   map[string]any
	Attempts   int
}

type Status struct {
	Rounds         int               `json:"rounds"`
	ReviewComments map[string]int    `json:"review_comments"`
	Reviews        map[string]int    `json:"reviews"`
	Outbox         map[string]int    `json:"outbox"`
	Cursors        map[string]string `json:"cursors"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open in-memory sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS repo_cursors (
			repo TEXT NOT NULL,
			stream TEXT NOT NULL,
			last_success_at TEXT NOT NULL,
			PRIMARY KEY (repo, stream)
		)`,
		`CREATE TABLE IF NOT EXISTS pr_rounds (
			repo TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			head_sha TEXT NOT NULL,
			base_sha TEXT NOT NULL,
			issue_key TEXT NOT NULL,
			reviewer_ids_json TEXT NOT NULL,
			status TEXT NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (repo, pr_number, head_sha)
		)`,
		`CREATE TABLE IF NOT EXISTS review_comments (
			repo TEXT NOT NULL,
			comment_id INTEGER NOT NULL,
			pr_number INTEGER NOT NULL,
			issue_key TEXT NOT NULL,
			body TEXT NOT NULL,
			metadata_json TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (repo, comment_id)
		)`,
		`CREATE TABLE IF NOT EXISTS reviews (
			repo TEXT NOT NULL,
			review_id INTEGER NOT NULL,
			pr_number INTEGER NOT NULL,
			issue_key TEXT NOT NULL,
			body TEXT NOT NULL,
			metadata_json TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (repo, review_id)
		)`,
		`CREATE TABLE IF NOT EXISTS outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_key TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL,
			issue_key TEXT NOT NULL,
			body TEXT NOT NULL,
			marker TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_comments_pending ON review_comments(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_reviews_pending ON reviews(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(status, next_attempt_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

func (s *Store) HasRound(ctx context.Context, repo string, pullNumber int, headSHA string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pr_rounds WHERE repo=? AND pr_number=? AND head_sha=?)`,
		repo, pullNumber, headSHA).Scan(&exists)
	return exists == 1, err
}

func (s *Store) CreateRound(ctx context.Context, round Round, outbox []OutboxInput) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := timestamp(time.Now())
	reviewers, err := json.Marshal(round.ReviewerIDs)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pr_rounds
		(repo, pr_number, head_sha, base_sha, issue_key, reviewer_ids_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		round.Repo, round.PullNumber, round.HeadSHA, round.BaseSHA, round.IssueKey, string(reviewers), now, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}
	for _, item := range outbox {
		if err := insertOutbox(ctx, tx, item, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Cursor(ctx context.Context, repo, stream string) (time.Time, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT last_success_at FROM repo_cursors WHERE repo=? AND stream=?`, repo, stream).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse cursor: %w", err)
	}
	return parsed, true, nil
}

func (s *Store) IngestReviewComments(ctx context.Context, items []FeedbackInput, cursor time.Time) (int, error) {
	return s.ingestFeedback(ctx, "review_comments", "comment_id", items, &cursor)
}

func (s *Store) IngestReviews(ctx context.Context, items []FeedbackInput) (int, error) {
	return s.ingestFeedback(ctx, "reviews", "review_id", items, nil)
}

func (s *Store) ingestFeedback(ctx context.Context, table, idColumn string, items []FeedbackInput, cursor *time.Time) (int, error) {
	if table != "review_comments" && table != "reviews" {
		return 0, errors.New("unsupported feedback table")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := timestamp(time.Now())
	inserted := 0
	for _, item := range items {
		metadata, err := json.Marshal(item.Metadata)
		if err != nil {
			return 0, err
		}
		query := fmt.Sprintf(`INSERT OR IGNORE INTO %s
			(repo, %s, pr_number, issue_key, body, metadata_json, occurred_at, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`, table, idColumn)
		result, err := tx.ExecContext(ctx, query,
			item.Repo, item.EventID, item.PullNumber, item.IssueKey, item.Body,
			string(metadata), timestamp(item.OccurredAt), now, now)
		if err != nil {
			return 0, err
		}
		rows, _ := result.RowsAffected()
		inserted += int(rows)
	}
	if cursor != nil {
		if len(items) > 0 {
			repo := items[0].Repo
			_, err = tx.ExecContext(ctx, `INSERT INTO repo_cursors(repo, stream, last_success_at)
				VALUES (?, 'review_comments', ?)
				ON CONFLICT(repo, stream) DO UPDATE SET last_success_at=excluded.last_success_at`,
				repo, timestamp(*cursor))
		} else {
			return 0, errors.New("cannot advance cursor without repository context")
		}
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func (s *Store) AdvanceCursor(ctx context.Context, repo, stream string, cursor time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repo_cursors(repo, stream, last_success_at)
		VALUES (?, ?, ?)
		ON CONFLICT(repo, stream) DO UPDATE SET last_success_at=excluded.last_success_at`,
		repo, stream, timestamp(cursor))
	return err
}

func (s *Store) PendingFeedback(ctx context.Context, limit int) ([]PendingFeedback, error) {
	if limit <= 0 {
		limit = 100
	}
	now := timestamp(time.Now())
	query := `SELECT kind, repo, event_id, pr_number, issue_key, body, metadata_json, attempts FROM (
		SELECT 'review_comment' AS kind, repo, comment_id AS event_id, pr_number, issue_key, body, metadata_json, attempts, occurred_at
		FROM review_comments WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=?)
		UNION ALL
		SELECT 'review' AS kind, repo, review_id AS event_id, pr_number, issue_key, body, metadata_json, attempts, occurred_at
		FROM reviews WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=?)
	) ORDER BY occurred_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingFeedback
	for rows.Next() {
		var item PendingFeedback
		var metadata string
		if err := rows.Scan(&item.Kind, &item.Repo, &item.EventID, &item.PullNumber,
			&item.IssueKey, &item.Body, &metadata, &item.Attempts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(metadata), &item.Metadata); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) QueueFeedback(ctx context.Context, item PendingFeedback, outbox OutboxInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := timestamp(time.Now())
	if err := insertOutbox(ctx, tx, outbox, now); err != nil {
		return err
	}
	table, idColumn, err := feedbackTable(item.Kind)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`UPDATE %s SET status='queued', updated_at=?, last_error='' WHERE repo=? AND %s=?`, table, idColumn)
	if _, err := tx.ExecContext(ctx, query, now, item.Repo, item.EventID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RetryFeedback(ctx context.Context, item PendingFeedback, message string, delay time.Duration) error {
	table, idColumn, err := feedbackTable(item.Kind)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`UPDATE %s SET attempts=attempts+1, next_attempt_at=?, last_error=?, updated_at=? WHERE repo=? AND %s=?`, table, idColumn)
	now := time.Now()
	_, err = s.db.ExecContext(ctx, query, timestamp(now.Add(delay)), truncate(message, 1000), timestamp(now), item.Repo, item.EventID)
	return err
}

func (s *Store) PermanentFeedback(ctx context.Context, item PendingFeedback, message string) error {
	table, idColumn, err := feedbackTable(item.Kind)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`UPDATE %s SET status='permanent_error', last_error=?, updated_at=? WHERE repo=? AND %s=?`, table, idColumn)
	_, err = s.db.ExecContext(ctx, query, truncate(message, 1000), timestamp(time.Now()), item.Repo, item.EventID)
	return err
}

func (s *Store) DueOutbox(ctx context.Context, limit int) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, event_key, kind, issue_key, body, marker, attempts
		FROM outbox WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=?)
		ORDER BY id ASC LIMIT ?`, timestamp(time.Now()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []OutboxItem
	for rows.Next() {
		var item OutboxItem
		if err := rows.Scan(&item.ID, &item.EventKey, &item.Kind, &item.IssueKey,
			&item.Body, &item.Marker, &item.Attempts); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeliverOutbox(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbox SET status='delivered', last_error='', next_attempt_at=NULL, updated_at=? WHERE id=?`,
		timestamp(time.Now()), id)
	return err
}

func (s *Store) RetryOutbox(ctx context.Context, id int64, message string, delay time.Duration) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts=attempts+1, next_attempt_at=?, last_error=?, updated_at=? WHERE id=?`,
		timestamp(now.Add(delay)), truncate(message, 1000), timestamp(now), id)
	return err
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	result := Status{
		ReviewComments: map[string]int{}, Reviews: map[string]int{},
		Outbox: map[string]int{}, Cursors: map[string]string{},
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_rounds`).Scan(&result.Rounds); err != nil {
		return result, err
	}
	if err := statusCounts(ctx, s.db, "review_comments", result.ReviewComments); err != nil {
		return result, err
	}
	if err := statusCounts(ctx, s.db, "reviews", result.Reviews); err != nil {
		return result, err
	}
	if err := statusCounts(ctx, s.db, "outbox", result.Outbox); err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT repo || ':' || stream, last_success_at FROM repo_cursors ORDER BY repo, stream`)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return result, err
		}
		result.Cursors[key] = value
	}
	return result, rows.Err()
}

func insertOutbox(ctx context.Context, tx *sql.Tx, item OutboxInput, now string) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox
		(event_key, kind, issue_key, body, marker, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		item.EventKey, item.Kind, item.IssueKey, item.Body, item.Marker, now, now)
	return err
}

func feedbackTable(kind string) (string, string, error) {
	switch kind {
	case "review_comment":
		return "review_comments", "comment_id", nil
	case "review":
		return "reviews", "review_id", nil
	default:
		return "", "", fmt.Errorf("unsupported feedback kind %q", kind)
	}
}

func statusCounts(ctx context.Context, db *sql.DB, table string, target map[string]int) error {
	if table != "review_comments" && table != "reviews" && table != "outbox" {
		return errors.New("unsupported status table")
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT status, COUNT(*) FROM %s GROUP BY status`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		target[status] = count
	}
	return rows.Err()
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
