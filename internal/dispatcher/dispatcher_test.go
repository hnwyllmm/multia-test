package dispatcher

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	gh "github.com/hnwyllmm/multia-test/internal/github"
	"github.com/hnwyllmm/multia-test/internal/multica"
	"github.com/hnwyllmm/multia-test/internal/state"
)

func TestIssueKeyFromTitle(t *testing.T) {
	for _, test := range []struct {
		title string
		key   string
		ok    bool
	}{
		{"[WANG-1] Test", "WANG-1", true},
		{"[WANG-42]", "WANG-42", true},
		{"prefix [WANG-1]", "", false},
		{"[WANG-0] Invalid", "", false},
		{"[SEEK-1] Wrong", "", false},
	} {
		key, ok := issueKeyFromTitle("WANG", test.title)
		if key != test.key || ok != test.ok {
			t.Fatalf("title %q: got (%q,%v), want (%q,%v)", test.title, key, ok, test.key, test.ok)
		}
	}
}

func TestFixMarkerMustBeFirstMeaningfulLine(t *testing.T) {
	if !hasFixMarker("\n  multica:fix [high] bug", "multica:fix") {
		t.Fatal("expected marker match")
	}
	if hasFixMarker("context first\nmultica:fix later", "multica:fix") {
		t.Fatal("marker after content must not match")
	}
	if !hasFixMarker("MULTICA:FIX issue", "multica:fix") {
		t.Fatal("marker should be case insensitive")
	}
}

func TestReviewerScoreStable(t *testing.T) {
	first := reviewerScore("owner/repo#1@sha", "agent")
	second := reviewerScore("owner/repo#1@sha", "agent")
	if first != second || first == reviewerScore("owner/repo#1@other", "agent") {
		t.Fatal("reviewer score is not stable and input-sensitive")
	}
}

func TestReviewerRuntimeReadinessRequiresOnlineRuntime(t *testing.T) {
	members := []multica.SquadMember{
		{ID: "leader", Type: "agent", Role: "leader"},
		{ID: "online-agent", Type: "agent", Role: "member"},
		{ID: "offline-agent", Type: "agent", Role: "member"},
		{ID: "missing-agent", Type: "agent", Role: "member"},
	}
	agents := []multica.Agent{
		{ID: "leader", Name: "Leader", Status: "idle", RuntimeBound: true, RuntimeID: "runtime-online"},
		{ID: "online-agent", Name: "Online", Status: "idle", RuntimeBound: true, RuntimeID: "runtime-online"},
		{ID: "offline-agent", Name: "Offline", Status: "idle", RuntimeBound: true, RuntimeID: "runtime-offline"},
	}
	runtimes := []multica.Runtime{
		{ID: "runtime-online", Name: "Online runtime", Status: "online"},
		{ID: "runtime-offline", Name: "Offline runtime", Status: "offline"},
	}
	items, byAgent := reviewerRuntimeReadiness("squad", members, agents, runtimes)
	if len(items) != 3 {
		t.Fatalf("readiness size=%d", len(items))
	}
	if _, found := byAgent["leader"]; found {
		t.Fatalf("squad leader must not be reported as a reviewer: %+v", byAgent["leader"])
	}
	if !byAgent["online-agent"].Ready || byAgent["online-agent"].Reason != "ready" {
		t.Fatalf("online agent=%+v", byAgent["online-agent"])
	}
	if byAgent["offline-agent"].Ready || byAgent["offline-agent"].Reason != "runtime_offline" {
		t.Fatalf("offline agent=%+v", byAgent["offline-agent"])
	}
	if byAgent["missing-agent"].Ready || byAgent["missing-agent"].Reason != "agent_missing" {
		t.Fatalf("missing agent=%+v", byAgent["missing-agent"])
	}
}

func TestReviewInvitationDoesNotRequireMulticaToken(t *testing.T) {
	message := reviewInvitation(multica.Agent{ID: "reviewer", Name: "Reviewer"}, "WANG-2", gh.PullRequest{
		HTMLURL: "https://github.com/owner/repo/pull/3", BaseSHA: "base", HeadSHA: "head",
	}, "1.12.8", "marker")
	for _, expected := range []string{"不要调用 Multica CLI", "不要读取或索取 MULTICA_TOKEN", "Multica runtime 自动写回当前工单"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("review invitation missing %q: %s", expected, message)
		}
	}
}

func TestFeedbackMessagePreservesBodyAndMarker(t *testing.T) {
	rawBody := "multica:fix keep $() and `ticks` verbatim"
	marker, body := feedbackMessage(state.PendingFeedback{
		Kind: "review_comment", Repo: "owner/repo", EventID: 99,
		PullNumber: 3, Body: rawBody,
		Metadata: map[string]any{"author": "alice", "url": "https://example", "path": "a.go", "line": float64(8)},
	}, "[@Worker](mention://agent/id)")
	if !strings.Contains(body, rawBody) || !strings.Contains(body, marker) {
		t.Fatal("feedback body or marker was not preserved")
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
	worker := New(cfg, githubClient, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := worker.Once(context.Background(), false); err == nil {
		t.Fatal("expected GitHub proxy failure")
	}
	current, found, err := store.Cursor(context.Background(), "owner/repo", reviewCommentStream)
	if err != nil || !found || !current.Equal(original) {
		t.Fatalf("cursor changed: current=%s found=%v err=%v", current, found, err)
	}
}
