package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	gh "github.com/hnwyllmm/multia-test/internal/github"
	"github.com/hnwyllmm/multia-test/internal/multica"
	"github.com/hnwyllmm/multia-test/internal/reviewer"
	"github.com/hnwyllmm/multia-test/internal/state"
)

func TestIssueKeyFromPull(t *testing.T) {
	for _, test := range []struct {
		title string
		body  string
		key   string
		ok    bool
	}{
		{"[WANG-1] Test", "", "WANG-1", true},
		{"ordinary title", "Implements WANG-42 acceptance criteria", "WANG-42", true},
		{"prefix [wang-3]", "", "WANG-3", true},
		{"[WANG-0] Invalid", "", "", false},
		{"[SEEK-1] Wrong", "", "", false},
	} {
		key, ok := issueKeyFromPull("WANG", test.title, test.body)
		if key != test.key || ok != test.ok {
			t.Fatalf("title %q body %q: got (%q,%v), want (%q,%v)", test.title, test.body, key, ok, test.key, test.ok)
		}
	}
}

func TestFindingMarkerIsHiddenAndPositionIndependent(t *testing.T) {
	if !hasFindingMarker("[P1] bug\n\n<!-- automated-review-finding:v1 fingerprint=abc -->", "automated-review-finding:v1") {
		t.Fatal("expected marker match")
	}
	if hasFindingMarker("multica:fix old visible prefix", "automated-review-finding:v1") {
		t.Fatal("legacy visible prefix must not qualify")
	}
}

func TestReviewerScoreStable(t *testing.T) {
	first := reviewerScore("owner/repo#1@sha", "agent")
	second := reviewerScore("owner/repo#1@sha", "agent")
	if first != second || first == reviewerScore("owner/repo#1@other", "agent") {
		t.Fatal("reviewer score is not stable and input-sensitive")
	}
}

func TestReviewRequestPayloadSupportsOptionalIssueContext(t *testing.T) {
	repository := config.Repository{
		GitHub: "owner/repo", ReviewEngine: "ocr_delegate", OCRVersion: "1.12.8",
		FindingMarker: "automated-review-finding:v1",
	}
	agent := config.ReviewAgent{ID: "reviewer", Name: "Reviewer"}
	pull := gh.PullRequest{
		Number: 3, HTMLURL: "https://github.com/owner/repo/pull/3", Title: "PR",
		Author: "alice", BaseRef: "main", BaseSHA: "base", HeadRef: "feature", HeadSHA: "head",
	}
	encoded, err := reviewRequestPayload("event", repository, agent, pull, "", nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var withoutIssue reviewWebhookPayload
	if err := json.Unmarshal([]byte(encoded), &withoutIssue); err != nil {
		t.Fatal(err)
	}
	if withoutIssue.MulticaIssue != nil || withoutIssue.PullRequest.HeadSHA != "head" ||
		withoutIssue.ReviewRound.FindingMarker != "automated-review-finding:v1" {
		t.Fatalf("unexpected payload %+v", withoutIssue)
	}
	issue := &multica.Issue{ID: "issue-id", Identifier: "WANG-2", Title: "Context", Description: "Acceptance", Status: "in_progress"}
	encoded, err = reviewRequestPayload("event", repository, agent, pull, "WANG-2", issue, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var withIssue reviewWebhookPayload
	if err := json.Unmarshal([]byte(encoded), &withIssue); err != nil {
		t.Fatal(err)
	}
	if withIssue.MulticaIssue == nil || withIssue.MulticaIssue.ID != "issue-id" || withIssue.MulticaIssue.Description != "Acceptance" {
		t.Fatalf("missing issue context %+v", withIssue.MulticaIssue)
	}
	if strings.Contains(encoded, "MULTICA_TOKEN") || strings.Contains(encoded, "multica:fix") {
		t.Fatalf("payload contains legacy instructions or credentials: %s", encoded)
	}
}

func TestFeedbackMessageLinksWithoutCopyingBody(t *testing.T) {
	rawBody := "[P1] keep $() and `ticks` verbatim\n\n<!-- automated-review-finding:v1 fingerprint=abc -->"
	marker, body := feedbackMessage(state.PendingFeedback{
		Kind: "review_comment", Repo: "owner/repo", EventID: 99,
		PullNumber: 3, Body: rawBody,
		Metadata: map[string]any{"author": "alice", "url": "https://example", "path": "a.go", "line": float64(8)},
	}, "[@Worker](mention://agent/id)")
	if !strings.Contains(body, "https://example") || !strings.Contains(body, "owner/repo#3") ||
		!strings.Contains(body, "[@Worker](mention://agent/id)") || !strings.Contains(body, marker) {
		t.Fatalf("feedback notification is incomplete: %s", body)
	}
	if strings.Contains(body, rawBody) || strings.Contains(body, "GitHub review body") {
		t.Fatalf("feedback copied the GitHub review body: %s", body)
	}
}

func TestFeedbackMessageFallsBackToPullURL(t *testing.T) {
	_, body := feedbackMessage(state.PendingFeedback{
		Kind: "review", Repo: "owner/repo", EventID: 100, PullNumber: 4,
	}, "[@Worker](mention://agent/id)")
	if !strings.Contains(body, "https://github.com/owner/repo/pull/4") {
		t.Fatalf("feedback has no usable fallback URL: %s", body)
	}
}

func TestPullWithoutMulticaIssueDispatchesReviewOnce(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "github-token")
	var webhookCalls int
	var webhookPayload reviewWebhookPayload
	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		webhookCalls++
		if request.Header.Get("Idempotency-Key") == "" {
			t.Error("missing idempotency key")
		}
		if err := json.NewDecoder(request.Body).Decode(&webhookPayload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer webhook.Close()
	t.Setenv("TEST_REVIEWER_WEBHOOK", webhook.URL+"/opaque")

	updatedAt := time.Now().UTC().Format(time.RFC3339)
	githubAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/repos/owner/repo/pulls":
			_, _ = io.WriteString(writer, `[{"number":7,"title":"Review this change","body":"No issue key","html_url":"https://github.com/owner/repo/pull/7","state":"open","draft":false,"head":{"sha":"head-sha","ref":"feature"},"base":{"sha":"base-sha","ref":"main"},"created_at":"`+updatedAt+`","updated_at":"`+updatedAt+`","user":{"login":"alice"}}]`)
		case request.URL.Path == "/repos/owner/repo/pulls/comments":
			_, _ = io.WriteString(writer, `[]`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer githubAPI.Close()

	githubClient, err := gh.New(config.GitHubConfig{
		APIBaseURL: githubAPI.URL,
		Auth:       config.SecretRef{Type: "env", Name: "TEST_GITHUB_TOKEN"},
		Proxy: config.ProxyConfig{
			Type: "none", RequestTimeout: config.Duration{Duration: time.Second},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewerConfig := config.ReviewDispatchConfig{
		RequestTimeout: config.Duration{Duration: time.Second},
		Agents: []config.ReviewAgent{{
			ID: "reviewer", Name: "Reviewer",
			Webhook: config.SecretRef{Type: "env", Name: "TEST_REVIEWER_WEBHOOK"},
		}},
	}
	reviewerClient, err := reviewer.New(reviewerConfig)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		BootstrapLookback: config.Duration{Duration: 24 * time.Hour},
		Multica:           config.MulticaConfig{Prefix: "WANG"},
		ReviewDispatch:    reviewerConfig,
		Repositories: []config.Repository{{
			GitHub: "owner/repo", ReviewerIDs: []string{"reviewer"}, ReviewerCount: 1,
			ReviewEngine: "ocr_delegate", OCRVersion: "1.12.8",
			FindingMarker: "automated-review-finding:v1",
		}},
	}
	worker := New(cfg, githubClient, nil, reviewerClient, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := worker.Once(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := worker.Once(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if webhookCalls != 1 {
		t.Fatalf("webhook calls=%d, want 1", webhookCalls)
	}
	if webhookPayload.MulticaIssue != nil || webhookPayload.PullRequest.HeadSHA != "head-sha" {
		t.Fatalf("unexpected webhook payload %+v", webhookPayload)
	}
}

func TestUnavailableProxyDoesNotAdvanceCommentCursor(t *testing.T) {
	t.Setenv("TEST_GITHUB_TOKEN", "token")
	t.Setenv("TEST_GITHUB_PROXY", "http://127.0.0.1:1")
	githubClient, err := gh.New(config.GitHubConfig{
		APIBaseURL: "https://api.github.invalid",
		Auth:       config.SecretRef{Type: "env", Name: "TEST_GITHUB_TOKEN"},
		Proxy: config.ProxyConfig{
			Type: "url_env", Name: "TEST_GITHUB_PROXY", Required: true,
			ConnectTimeout: config.Duration{Duration: 50 * time.Millisecond},
			RequestTimeout: config.Duration{Duration: 200 * time.Millisecond},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	original := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := store.AdvanceCursor(context.Background(), "owner/repo", reviewCommentStream, original); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BootstrapLookback: config.Duration{Duration: 24 * time.Hour},
		Multica:           config.MulticaConfig{Prefix: "WANG"},
		Repositories:      []config.Repository{{GitHub: "owner/repo"}},
	}
	worker := New(cfg, githubClient, nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := worker.Once(context.Background(), false); err == nil {
		t.Fatal("expected GitHub proxy failure")
	}
	current, found, err := store.Cursor(context.Background(), "owner/repo", reviewCommentStream)
	if err != nil || !found || !current.Equal(original) {
		t.Fatalf("cursor changed: current=%s found=%v err=%v", current, found, err)
	}
}
