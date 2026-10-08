package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundAndOutboxAreIdempotent(t *testing.T) {
	store, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	round := Round{Repo: "owner/repo", PullNumber: 1, HeadSHA: "head", BaseSHA: "base", IssueKey: "WANG-1", ReviewerIDs: []string{"agent"}}
	outbox := []OutboxInput{{EventKey: "review-request:key", Kind: "review_request", IssueKey: "WANG-1", Body: "body", Marker: "marker"}}
	created, err := store.CreateRound(context.Background(), round, outbox)
	if err != nil || !created {
		t.Fatalf("first round create: created=%v err=%v", created, err)
	}
	created, err = store.CreateRound(context.Background(), round, outbox)
	if err != nil || created {
		t.Fatalf("duplicate round create: created=%v err=%v", created, err)
	}
	items, err := store.DueOutbox(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("outbox items=%d err=%v", len(items), err)
	}
}

func TestCancelPendingReviewRequestsLeavesFeedbackDeliveries(t *testing.T) {
	store, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	round := Round{Repo: "owner/repo", PullNumber: 1, HeadSHA: "head", BaseSHA: "base", ReviewerIDs: []string{"agent"}}
	created, err := store.CreateRound(ctx, round, []OutboxInput{{
		EventKey: "review-request:key", Kind: "review_request", Body: "body", Marker: "agent",
	}})
	if err != nil || !created {
		t.Fatalf("create round: created=%v err=%v", created, err)
	}
	feedback := FeedbackInput{
		Repo: "owner/repo", EventID: 9, PullNumber: 1, IssueKey: "SEEK-9",
		Body: "finding", Metadata: map[string]any{}, OccurredAt: time.Now(),
	}
	if _, err := store.IngestReviewComments(ctx, []FeedbackInput{feedback}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingFeedback(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending feedback=%+v err=%v", pending, err)
	}
	if err := store.QueueFeedback(ctx, pending[0], OutboxInput{
		EventKey: "review_comment:owner/repo:9", Kind: "review_comment",
		IssueKey: "SEEK-9", Body: "notify", Marker: "marker",
	}); err != nil {
		t.Fatal(err)
	}
	canceled, err := store.CancelPendingReviewRequests(ctx)
	if err != nil || canceled != 1 {
		t.Fatalf("canceled=%d err=%v", canceled, err)
	}
	due, err := store.DueOutbox(ctx, 10)
	if err != nil || len(due) != 1 || due[0].Kind != "review_comment" {
		t.Fatalf("due outbox=%+v err=%v", due, err)
	}
	status, err := store.Status(ctx)
	if err != nil || status.Outbox["canceled"] != 1 || status.Outbox["pending"] != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestCommentEditDoesNotCreateSecondEvent(t *testing.T) {
	store, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := FeedbackInput{Repo: "owner/repo", EventID: 7, PullNumber: 1, IssueKey: "WANG-1", Body: "first", Metadata: map[string]any{}, OccurredAt: time.Now()}
	inserted, err := store.IngestReviewComments(context.Background(), []FeedbackInput{first}, time.Now())
	if err != nil || inserted != 1 {
		t.Fatalf("first ingest=%d err=%v", inserted, err)
	}
	first.Body = "edited"
	inserted, err = store.IngestReviewComments(context.Background(), []FeedbackInput{first}, time.Now())
	if err != nil || inserted != 0 {
		t.Fatalf("edited ingest=%d err=%v", inserted, err)
	}
	pending, err := store.PendingFeedback(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].Body != "first" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
}

func TestQueueFeedbackIsTransactional(t *testing.T) {
	store, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := FeedbackInput{Repo: "owner/repo", EventID: 8, PullNumber: 2, IssueKey: "WANG-2", Body: "body", Metadata: map[string]any{}, OccurredAt: time.Now()}
	if _, err := store.IngestReviews(context.Background(), []FeedbackInput{input}); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.PendingFeedback(context.Background(), 10)
	if err := store.QueueFeedback(context.Background(), pending[0], OutboxInput{EventKey: "review:key", Kind: "review", IssueKey: "WANG-2", Body: "comment", Marker: "marker"}); err != nil {
		t.Fatal(err)
	}
	pending, _ = store.PendingFeedback(context.Background(), 10)
	if len(pending) != 0 {
		t.Fatalf("feedback remained pending: %+v", pending)
	}
	outbox, _ := store.DueOutbox(context.Background(), 10)
	if len(outbox) != 1 {
		t.Fatalf("outbox size=%d", len(outbox))
	}
}

func TestDashboardSummarizesDispatcherState(t *testing.T) {
	store, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	round := Round{
		Repo: "owner/repo", PullNumber: 4, HeadSHA: "head", BaseSHA: "base",
		IssueKey: "WANG-4", ReviewerIDs: []string{"reviewer-id"},
	}
	created, err := store.CreateRound(ctx, round, []OutboxInput{{
		EventKey: "review-request:owner/repo:4:head:reviewer-id",
		Kind:     "review_request", IssueKey: "WANG-4",
		Body: "[@Review General](mention://agent/reviewer-id)\n\nReview request", Marker: "round-marker",
	}})
	if err != nil || !created {
		t.Fatalf("create round: created=%v err=%v", created, err)
	}
	outbox, err := store.DueOutbox(ctx, 10)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("round outbox=%+v err=%v", outbox, err)
	}
	if err := store.DeliverOutbox(ctx, outbox[0].ID); err != nil {
		t.Fatal(err)
	}

	feedback := FeedbackInput{
		Repo: "owner/repo", EventID: 99, PullNumber: 4, IssueKey: "WANG-4",
		Body: "[P1] Investigate this line\nextra detail\n<!-- automated-review-finding:v1 fingerprint=abc -->",
		Metadata: map[string]any{
			"author": "alice", "url": "https://example.invalid/comment", "path": "main.go", "line": 12,
		},
		OccurredAt: time.Now(),
	}
	if _, err := store.IngestReviewComments(ctx, []FeedbackInput{feedback}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingFeedback(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := store.QueueFeedback(ctx, pending[0], OutboxInput{
		EventKey: "review_comment:owner/repo:99", Kind: "review_comment",
		IssueKey: "WANG-4", Body: "forwarded", Marker: "feedback-marker",
	}); err != nil {
		t.Fatal(err)
	}
	outbox, err = store.DueOutbox(ctx, 10)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("feedback outbox=%+v err=%v", outbox, err)
	}
	if err := store.DeliverOutbox(ctx, outbox[0].ID); err != nil {
		t.Fatal(err)
	}
	runID, err := store.BeginPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPoll(ctx, runID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceReviewerReadiness(ctx, "squad", []ReviewerReadiness{{
		SquadID: "squad", AgentID: "reviewer-id", AgentName: "Review General",
		AgentStatus: "idle", RuntimeID: "runtime", RuntimeName: "Codex",
		RuntimeStatus: "online", Ready: true, Reason: "ready",
	}}); err != nil {
		t.Fatal(err)
	}

	dashboard, err := store.Dashboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Summary.Repositories != 1 || dashboard.Summary.Rounds != 1 || dashboard.Summary.Feedback != 1 {
		t.Fatalf("unexpected summary %+v", dashboard.Summary)
	}
	if len(dashboard.Rounds) != 1 || dashboard.Rounds[0].Status != "dispatched" ||
		len(dashboard.Rounds[0].Reviewers) != 1 || dashboard.Rounds[0].Reviewers[0].Name != "Review General" {
		t.Fatalf("unexpected rounds %+v", dashboard.Rounds)
	}
	if len(dashboard.Feedback) != 1 || dashboard.Feedback[0].DeliveryStatus != "delivered" ||
		dashboard.Feedback[0].BodySummary != "[P1] Investigate this line" {
		t.Fatalf("unexpected feedback %+v", dashboard.Feedback)
	}
	if len(dashboard.PollRuns) != 1 || dashboard.PollRuns[0].Status != "success" {
		t.Fatalf("unexpected poll runs %+v", dashboard.PollRuns)
	}
	if len(dashboard.ReviewerReadiness) != 1 || !dashboard.ReviewerReadiness[0].Ready ||
		dashboard.ReviewerReadiness[0].RuntimeStatus != "online" {
		t.Fatalf("unexpected reviewer readiness %+v", dashboard.ReviewerReadiness)
	}
}

func TestReplaceAllReviewerReadinessRemovesPreviousSources(t *testing.T) {
	store, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.ReplaceReviewerReadiness(ctx, "old-squad", []ReviewerReadiness{{
		SquadID: "old-squad", AgentID: "old", AgentName: "Old",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceAllReviewerReadiness(ctx, []ReviewerReadiness{{
		SquadID: "review_dispatch", AgentID: "new", AgentName: "New", Ready: true,
	}}); err != nil {
		t.Fatal(err)
	}
	dashboard, err := store.Dashboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.ReviewerReadiness) != 1 || dashboard.ReviewerReadiness[0].AgentID != "new" {
		t.Fatalf("unexpected reviewer readiness %+v", dashboard.ReviewerReadiness)
	}
}

func TestReplaceReviewerReadinessRemovesStaleMembers(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.ReplaceReviewerReadiness(ctx, "squad", []ReviewerReadiness{
		{SquadID: "squad", AgentID: "old", AgentName: "Old", Reason: "runtime_offline"},
		{SquadID: "squad", AgentID: "keep", AgentName: "Keep", Ready: true, Reason: "ready"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceReviewerReadiness(ctx, "squad", []ReviewerReadiness{
		{SquadID: "squad", AgentID: "keep", AgentName: "Keep", Ready: true, Reason: "ready"},
	}); err != nil {
		t.Fatal(err)
	}
	dashboard, err := store.Dashboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.ReviewerReadiness) != 1 || dashboard.ReviewerReadiness[0].AgentID != "keep" {
		t.Fatalf("unexpected reviewer readiness %+v", dashboard.ReviewerReadiness)
	}
	if err := store.ReplaceReviewerReadiness(ctx, "squad", nil); err != nil {
		t.Fatal(err)
	}
	dashboard, err = store.Dashboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.ReviewerReadiness) != 0 {
		t.Fatalf("expected empty reviewer readiness, got %+v", dashboard.ReviewerReadiness)
	}
}
