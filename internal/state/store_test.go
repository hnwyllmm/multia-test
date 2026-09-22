package state

import (
	"context"
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
