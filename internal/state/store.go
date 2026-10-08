package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

type DashboardSummary struct {
	Repositories      int `json:"repositories"`
	Rounds            int `json:"rounds"`
	Feedback          int `json:"feedback"`
	PendingDeliveries int `json:"pending_deliveries"`
	Failures          int `json:"failures"`
}

type DashboardCursor struct {
	Repo          string `json:"repo"`
	Stream        string `json:"stream"`
	LastSuccessAt string `json:"last_success_at"`
}

type DashboardReviewer struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type DashboardRound struct {
	Repo          string              `json:"repo"`
	PullNumber    int                 `json:"pull_number"`
	HeadSHA       string              `json:"head_sha"`
	BaseSHA       string              `json:"base_sha"`
	IssueKey      string              `json:"issue_key"`
	ReviewerIDs   []string            `json:"reviewer_ids"`
	Reviewers     []DashboardReviewer `json:"reviewers"`
	Status        string              `json:"status"`
	LastError     string              `json:"last_error,omitempty"`
	CreatedAt     string              `json:"created_at"`
	UpdatedAt     string              `json:"updated_at"`
	DeliveryTotal int                 `json:"delivery_total"`
	Delivered     int                 `json:"delivered"`
}

type DashboardFeedback struct {
	Kind           string `json:"kind"`
	Repo           string `json:"repo"`
	EventID        int64  `json:"event_id"`
	PullNumber     int    `json:"pull_number"`
	IssueKey       string `json:"issue_key"`
	Author         string `json:"author,omitempty"`
	URL            string `json:"url,omitempty"`
	Path           string `json:"path,omitempty"`
	Line           string `json:"line,omitempty"`
	BodySummary    string `json:"body_summary,omitempty"`
	Status         string `json:"status"`
	DeliveryStatus string `json:"delivery_status,omitempty"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error,omitempty"`
	OccurredAt     string `json:"occurred_at"`
	UpdatedAt      string `json:"updated_at"`
}

type DashboardOutbox struct {
	ID            int64  `json:"id"`
	EventKey      string `json:"event_key"`
	Kind          string `json:"kind"`
	IssueKey      string `json:"issue_key"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type DashboardPollRun struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type ReviewerReadiness struct {
	SquadID       string `json:"squad_id"`
	AgentID       string `json:"agent_id"`
	AgentName     string `json:"agent_name,omitempty"`
	AgentStatus   string `json:"agent_status,omitempty"`
	RuntimeID     string `json:"runtime_id,omitempty"`
	RuntimeName   string `json:"runtime_name,omitempty"`
	RuntimeStatus string `json:"runtime_status,omitempty"`
	Ready         bool   `json:"ready"`
	Reason        string `json:"reason,omitempty"`
	CheckedAt     string `json:"checked_at"`
}

type DashboardData struct {
	GeneratedAt       string              `json:"generated_at"`
	Summary           DashboardSummary    `json:"summary"`
	Cursors           []DashboardCursor   `json:"cursors"`
	Rounds            []DashboardRound    `json:"rounds"`
	Feedback          []DashboardFeedback `json:"feedback"`
	Outbox            []DashboardOutbox   `json:"outbox"`
	PollRuns          []DashboardPollRun  `json:"poll_runs"`
	ReviewerReadiness []ReviewerReadiness `json:"reviewer_readiness"`
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
		`CREATE TABLE IF NOT EXISTS poll_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			completed_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS reviewer_readiness (
			squad_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			agent_name TEXT NOT NULL DEFAULT '',
			agent_status TEXT NOT NULL DEFAULT '',
			runtime_id TEXT NOT NULL DEFAULT '',
			runtime_name TEXT NOT NULL DEFAULT '',
			runtime_status TEXT NOT NULL DEFAULT '',
			ready INTEGER NOT NULL DEFAULT 0,
			reason TEXT NOT NULL DEFAULT '',
			checked_at TEXT NOT NULL,
			PRIMARY KEY (squad_id, agent_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_comments_pending ON review_comments(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_reviews_pending ON reviews(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_poll_runs_started ON poll_runs(started_at DESC)`,
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

func (s *Store) CancelPendingReviewRequests(ctx context.Context) (int64, error) {
	now := timestamp(time.Now())
	result, err := s.db.ExecContext(ctx, `UPDATE outbox
		SET status='canceled', next_attempt_at=NULL, last_error='', updated_at=?
		WHERE kind='review_request' AND status='pending'`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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

func (s *Store) BeginPoll(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO poll_runs(status, started_at) VALUES ('running', ?)`, timestamp(time.Now()))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) FinishPoll(ctx context.Context, id int64, runErr error) error {
	status := "success"
	message := ""
	if runErr != nil {
		status = "error"
		message = truncate(runErr.Error(), 1000)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE poll_runs SET status=?, error=?, completed_at=? WHERE id=?`,
		status, message, timestamp(time.Now()), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM poll_runs WHERE id NOT IN (SELECT id FROM poll_runs ORDER BY id DESC LIMIT 200)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReplaceReviewerReadiness(ctx context.Context, squadID string, items []ReviewerReadiness) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reviewer_readiness WHERE squad_id=?`, squadID); err != nil {
		return err
	}
	checkedAt := timestamp(time.Now())
	for _, item := range items {
		ready := 0
		if item.Ready {
			ready = 1
		}
		if item.SquadID != squadID {
			return fmt.Errorf("reviewer readiness squad %q does not match %q", item.SquadID, squadID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reviewer_readiness
			(squad_id, agent_id, agent_name, agent_status, runtime_id, runtime_name,
			 runtime_status, ready, reason, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(squad_id, agent_id) DO UPDATE SET
				agent_name=excluded.agent_name, agent_status=excluded.agent_status,
				runtime_id=excluded.runtime_id, runtime_name=excluded.runtime_name,
				runtime_status=excluded.runtime_status, ready=excluded.ready,
				reason=excluded.reason, checked_at=excluded.checked_at`,
			item.SquadID, item.AgentID, item.AgentName, item.AgentStatus,
			item.RuntimeID, item.RuntimeName, item.RuntimeStatus, ready,
			item.Reason, checkedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReplaceAllReviewerReadiness(ctx context.Context, items []ReviewerReadiness) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reviewer_readiness`); err != nil {
		return err
	}
	checkedAt := timestamp(time.Now())
	for _, item := range items {
		ready := 0
		if item.Ready {
			ready = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reviewer_readiness
			(squad_id, agent_id, agent_name, agent_status, runtime_id, runtime_name,
			 runtime_status, ready, reason, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.SquadID, item.AgentID, item.AgentName, item.AgentStatus,
			item.RuntimeID, item.RuntimeName, item.RuntimeStatus, ready,
			item.Reason, checkedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Dashboard(ctx context.Context, limit int) (DashboardData, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	result := DashboardData{GeneratedAt: timestamp(time.Now())}
	var err error
	if result.Summary, err = s.dashboardSummary(ctx); err != nil {
		return result, err
	}
	if result.Cursors, err = s.dashboardCursors(ctx); err != nil {
		return result, err
	}
	if result.Rounds, err = s.dashboardRounds(ctx, limit); err != nil {
		return result, err
	}
	if result.Feedback, err = s.dashboardFeedback(ctx, limit); err != nil {
		return result, err
	}
	if result.Outbox, err = s.dashboardOutbox(ctx, limit); err != nil {
		return result, err
	}
	if result.PollRuns, err = s.dashboardPollRuns(ctx, 20); err != nil {
		return result, err
	}
	if result.ReviewerReadiness, err = s.dashboardReviewerReadiness(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) dashboardSummary(ctx context.Context) (DashboardSummary, error) {
	var result DashboardSummary
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(DISTINCT repo) FROM (
			SELECT repo FROM pr_rounds UNION SELECT repo FROM review_comments UNION SELECT repo FROM reviews
		)),
		(SELECT COUNT(*) FROM pr_rounds),
		(SELECT COUNT(*) FROM review_comments) + (SELECT COUNT(*) FROM reviews),
		(SELECT COUNT(*) FROM outbox WHERE status='pending'),
		(SELECT COUNT(*) FROM review_comments WHERE status='permanent_error') +
		(SELECT COUNT(*) FROM reviews WHERE status='permanent_error') +
		(SELECT COUNT(*) FROM outbox WHERE last_error!='')`).Scan(
		&result.Repositories, &result.Rounds, &result.Feedback,
		&result.PendingDeliveries, &result.Failures)
	return result, err
}

func (s *Store) dashboardCursors(ctx context.Context) ([]DashboardCursor, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, stream, last_success_at FROM repo_cursors ORDER BY repo, stream`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DashboardCursor{}
	for rows.Next() {
		var item DashboardCursor
		if err := rows.Scan(&item.Repo, &item.Stream, &item.LastSuccessAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) dashboardRounds(ctx context.Context, limit int) ([]DashboardRound, error) {
	reviewerNames, err := s.reviewerNames(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		r.repo, r.pr_number, r.head_sha, r.base_sha, r.issue_key,
		r.reviewer_ids_json,
		COALESCE((SELECT GROUP_CONCAT(o.body, char(10)) FROM outbox o WHERE o.kind='review_request' AND
			substr(o.event_key, 1, length('review-request:' || r.repo || ':' || r.pr_number || ':' || r.head_sha || ':')) =
			'review-request:' || r.repo || ':' || r.pr_number || ':' || r.head_sha || ':'), ''),
		r.status, r.last_error, r.created_at, r.updated_at,
		(SELECT COUNT(*) FROM outbox o WHERE o.kind='review_request' AND
			substr(o.event_key, 1, length('review-request:' || r.repo || ':' || r.pr_number || ':' || r.head_sha || ':')) =
			'review-request:' || r.repo || ':' || r.pr_number || ':' || r.head_sha || ':'),
		(SELECT COUNT(*) FROM outbox o WHERE o.kind='review_request' AND o.status='delivered' AND
			substr(o.event_key, 1, length('review-request:' || r.repo || ':' || r.pr_number || ':' || r.head_sha || ':')) =
			'review-request:' || r.repo || ':' || r.pr_number || ':' || r.head_sha || ':')
		FROM pr_rounds r ORDER BY r.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DashboardRound{}
	for rows.Next() {
		var item DashboardRound
		var reviewerIDs, reviewerBodies string
		if err := rows.Scan(&item.Repo, &item.PullNumber, &item.HeadSHA, &item.BaseSHA,
			&item.IssueKey, &reviewerIDs, &reviewerBodies, &item.Status, &item.LastError,
			&item.CreatedAt, &item.UpdatedAt, &item.DeliveryTotal, &item.Delivered); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reviewerIDs), &item.ReviewerIDs); err != nil {
			return nil, err
		}
		item.Reviewers = make([]DashboardReviewer, 0, len(item.ReviewerIDs))
		for _, reviewerID := range item.ReviewerIDs {
			name := reviewerNames[reviewerID]
			if name == "" {
				name = reviewerNameFromMentions(reviewerBodies, reviewerID)
			}
			item.Reviewers = append(item.Reviewers, DashboardReviewer{
				ID: reviewerID, Name: name,
			})
		}
		if item.DeliveryTotal > 0 && item.DeliveryTotal == item.Delivered {
			item.Status = "dispatched"
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) reviewerNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id, agent_name FROM reviewer_readiness WHERE agent_name!=''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result[id] = name
	}
	return result, rows.Err()
}

func reviewerNameFromMentions(value, reviewerID string) string {
	suffix := "](mention://agent/" + reviewerID + ")"
	end := strings.Index(value, suffix)
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(value[:end], "[@")
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(value[start+2 : end])
}

func (s *Store) dashboardFeedback(ctx context.Context, limit int) ([]DashboardFeedback, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		f.kind, f.repo, f.event_id, f.pr_number, f.issue_key, f.body,
		f.metadata_json, f.status, f.attempts, f.last_error, f.occurred_at, f.updated_at,
		COALESCE((SELECT o.status FROM outbox o
			WHERE o.event_key=f.kind || ':' || f.repo || ':' || f.event_id LIMIT 1), '')
		FROM (
			SELECT 'review_comment' AS kind, repo, comment_id AS event_id, pr_number,
				issue_key, body, metadata_json, status, attempts, last_error, occurred_at, updated_at
			FROM review_comments
			UNION ALL
			SELECT 'review' AS kind, repo, review_id AS event_id, pr_number,
				issue_key, body, metadata_json, status, attempts, last_error, occurred_at, updated_at
			FROM reviews
		) f ORDER BY f.occurred_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DashboardFeedback{}
	for rows.Next() {
		var item DashboardFeedback
		var body, metadata string
		if err := rows.Scan(&item.Kind, &item.Repo, &item.EventID, &item.PullNumber,
			&item.IssueKey, &body, &metadata, &item.Status, &item.Attempts,
			&item.LastError, &item.OccurredAt, &item.UpdatedAt, &item.DeliveryStatus); err != nil {
			return nil, err
		}
		var values map[string]any
		if err := json.Unmarshal([]byte(metadata), &values); err != nil {
			return nil, err
		}
		item.Author = stringFromMetadata(values, "author")
		item.URL = stringFromMetadata(values, "url")
		item.Path = stringFromMetadata(values, "path")
		item.Line = stringFromMetadata(values, "line")
		if item.Line == "" {
			item.Line = stringFromMetadata(values, "original_line")
		}
		item.BodySummary = firstMeaningfulLine(body, 180)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) dashboardOutbox(ctx context.Context, limit int) ([]DashboardOutbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, event_key, kind, issue_key, status,
		attempts, next_attempt_at, last_error, created_at, updated_at
		FROM outbox ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DashboardOutbox{}
	for rows.Next() {
		var item DashboardOutbox
		var next sql.NullString
		if err := rows.Scan(&item.ID, &item.EventKey, &item.Kind, &item.IssueKey,
			&item.Status, &item.Attempts, &next, &item.LastError,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if next.Valid {
			item.NextAttemptAt = next.String
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) dashboardPollRuns(ctx context.Context, limit int) ([]DashboardPollRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, status, error, started_at, completed_at
		FROM poll_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DashboardPollRun{}
	for rows.Next() {
		var item DashboardPollRun
		var completed sql.NullString
		if err := rows.Scan(&item.ID, &item.Status, &item.Error, &item.StartedAt, &completed); err != nil {
			return nil, err
		}
		if completed.Valid {
			item.CompletedAt = completed.String
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) dashboardReviewerReadiness(ctx context.Context) ([]ReviewerReadiness, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT squad_id, agent_id, agent_name, agent_status,
		runtime_id, runtime_name, runtime_status, ready, reason, checked_at
		FROM reviewer_readiness ORDER BY agent_name, agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ReviewerReadiness{}
	for rows.Next() {
		var item ReviewerReadiness
		var ready int
		if err := rows.Scan(&item.SquadID, &item.AgentID, &item.AgentName, &item.AgentStatus,
			&item.RuntimeID, &item.RuntimeName, &item.RuntimeStatus, &ready,
			&item.Reason, &item.CheckedAt); err != nil {
			return nil, err
		}
		item.Ready = ready == 1
		result = append(result, item)
	}
	return result, rows.Err()
}

func stringFromMetadata(metadata map[string]any, key string) string {
	value := metadata[key]
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func firstMeaningfulLine(value string, limit int) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return truncate(line, limit)
	}
	return ""
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
