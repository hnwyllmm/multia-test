package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	gh "github.com/hnwyllmm/multia-test/internal/github"
	"github.com/hnwyllmm/multia-test/internal/multica"
	"github.com/hnwyllmm/multia-test/internal/reviewer"
	"github.com/hnwyllmm/multia-test/internal/state"
)

const reviewCommentStream = "review_comments"

var activeIssueStatuses = map[string]struct{}{
	"todo": {}, "in_progress": {}, "in_review": {},
}

type Dispatcher struct {
	cfg      *config.Config
	github   *gh.Client
	multica  multicaClient
	reviewer *reviewer.Client
	store    *state.Store
	logger   *slog.Logger
	now      func() time.Time
}

type multicaClient interface {
	GetIssue(context.Context, string) (multica.Issue, error)
	ListAgents(context.Context) ([]multica.Agent, error)
	ListRuntimes(context.Context) ([]multica.Runtime, error)
	GetSquad(context.Context, string) (multica.Squad, error)
	ListComments(context.Context, string) ([]multica.Comment, error)
	AddComment(context.Context, string, string) error
}

func New(cfg *config.Config, github *gh.Client, multicaClient multicaClient, reviewerClient *reviewer.Client, store *state.Store, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		cfg: cfg, github: github, multica: multicaClient,
		reviewer: reviewerClient, store: store, logger: logger, now: time.Now,
	}
}

func (d *Dispatcher) Check(ctx context.Context) error {
	if err := d.github.Check(ctx); err != nil {
		return err
	}
	if d.cfg.ReviewDispatch.IsEnabled() {
		if d.reviewer == nil {
			return errors.New("review webhook client is not configured")
		}
		if err := d.reviewer.Check(); err != nil {
			return err
		}
	}
	d.logger.Info("review dependencies ready", "github_proxy", d.github.ProxyLabel(),
		"auto_review_enabled", d.cfg.ReviewDispatch.IsEnabled(), "reviewers", len(d.cfg.ReviewDispatch.Agents))
	return nil
}

func (d *Dispatcher) Once(ctx context.Context, dryRun bool) error {
	var laneErrors []error
	if d.cfg.ReviewDispatch.IsEnabled() {
		if err := d.recordConfiguredReviewers(ctx); err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("record reviewer configuration: %w", err))
		}
	} else if !dryRun {
		canceled, err := d.store.CancelPendingReviewRequests(ctx)
		if err != nil {
			return fmt.Errorf("disable pending review requests: %w", err)
		}
		if err := d.store.ReplaceAllReviewerReadiness(ctx, nil); err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("clear reviewer readiness: %w", err))
		}
		if canceled > 0 {
			d.logger.Info("pending review requests canceled because auto review is disabled", "count", canceled)
		}
	}
	for _, repository := range d.cfg.Repositories {
		owner, repo, _ := repository.OwnerRepo()
		pulls, err := d.github.ListOpenPulls(ctx, owner, repo)
		if err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("%s pull discovery: %w", repository.GitHub, err))
			continue
		}
		pullMap := make(map[int]gh.PullRequest, len(pulls))
		for _, pull := range pulls {
			pullMap[pull.Number] = pull
		}

		if d.cfg.ReviewDispatch.IsEnabled() {
			if err := d.discoverReviewRounds(ctx, repository, pulls, dryRun); err != nil {
				laneErrors = append(laneErrors, fmt.Errorf("%s review dispatch: %w", repository.GitHub, err))
			}
		}
		if err := d.discoverReviewComments(ctx, repository, owner, repo, pullMap, dryRun); err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("%s review comments: %w", repository.GitHub, err))
		}
		if err := d.discoverReviews(ctx, repository, owner, repo, pulls, dryRun); err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("%s reviews: %w", repository.GitHub, err))
		}
	}

	if !dryRun {
		if err := d.queueFeedback(ctx); err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("queue feedback: %w", err))
		}
		if err := d.deliverOutbox(ctx); err != nil {
			laneErrors = append(laneErrors, fmt.Errorf("deliver outbox: %w", err))
		}
	}
	return errors.Join(laneErrors...)
}

func (d *Dispatcher) discoverReviewRounds(ctx context.Context, repository config.Repository, pulls []gh.PullRequest, dryRun bool) error {
	cutoff := d.now().Add(-d.cfg.BootstrapLookback.Duration)
	for _, pull := range pulls {
		if pull.Draft || !strings.EqualFold(pull.State, "open") || pull.UpdatedAt.Before(cutoff) {
			continue
		}
		exists, err := d.store.HasRound(ctx, repository.GitHub, pull.Number, pull.HeadSHA)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		issueKey, associated := issueKeyFromPull(d.cfg.Multica.Prefix, pull.Title, pull.Body)
		var issue *multica.Issue
		if associated && d.multica != nil {
			resolved, err := d.multica.GetIssue(ctx, issueKey)
			if err != nil {
				d.logger.Warn("optional issue context unavailable", "repo", repository.GitHub, "pr", pull.Number, "issue", issueKey, "error", err)
			} else {
				issue = &resolved
				if resolved.Identifier != "" {
					issueKey = resolved.Identifier
				}
			}
		}
		reviewers, err := d.selectReviewers(ctx, repository, issue, pull)
		if err != nil {
			d.logger.Warn("pull deferred: reviewer selection failed", "repo", repository.GitHub, "pr", pull.Number, "error", err)
			continue
		}
		var outbox []state.OutboxInput
		var reviewerIDs []string
		for _, reviewer := range reviewers {
			reviewerIDs = append(reviewerIDs, reviewer.ID)
			eventKey := fmt.Sprintf("review-request:%s:%d:%s:%s", repository.GitHub, pull.Number, pull.HeadSHA, reviewer.ID)
			body, err := reviewRequestPayload(eventKey, repository, reviewer, pull, issueKey, issue, d.now())
			if err != nil {
				return err
			}
			outbox = append(outbox, state.OutboxInput{
				EventKey: eventKey, Kind: "review_request", IssueKey: issueKey,
				Body: body, Marker: reviewer.ID,
			})
		}
		if dryRun {
			d.logger.Info("dry-run review request", "repo", repository.GitHub, "pr", pull.Number,
				"issue", issueKey, "head_sha", pull.HeadSHA, "reviewer_ids", reviewerIDs)
			continue
		}
		created, err := d.store.CreateRound(ctx, state.Round{
			Repo: repository.GitHub, PullNumber: pull.Number, HeadSHA: pull.HeadSHA,
			BaseSHA: pull.BaseSHA, IssueKey: issueKey, ReviewerIDs: reviewerIDs,
		}, outbox)
		if err != nil {
			return err
		}
		if created {
			d.logger.Info("review round queued", "repo", repository.GitHub, "pr", pull.Number,
				"issue", issueKey, "head_sha", pull.HeadSHA, "reviewer_ids", reviewerIDs)
		}
	}
	return nil
}

func (d *Dispatcher) discoverReviewComments(ctx context.Context, repository config.Repository, owner, repo string, pulls map[int]gh.PullRequest, dryRun bool) error {
	cursor, found, err := d.store.Cursor(ctx, repository.GitHub, reviewCommentStream)
	if err != nil {
		return err
	}
	if !found {
		cursor = d.now().Add(-d.cfg.BootstrapLookback.Duration)
	}
	since := cursor.Add(-5 * time.Minute)
	comments, err := d.github.ListReviewComments(ctx, owner, repo, since)
	if err != nil {
		return err
	}
	successAt := d.now()
	var inputs []state.FeedbackInput
	for _, comment := range comments {
		pull, ok := pulls[comment.PullNumber]
		if !ok || pull.Draft {
			continue
		}
		if !acceptReviewComment(repository, comment.Body) {
			continue
		}
		issueKey, ok := issueKeyFromPull(d.cfg.Multica.Prefix, pull.Title, pull.Body)
		if !ok {
			d.logger.Info("review finding has no associated Multica issue", "repo", repository.GitHub, "pr", pull.Number, "comment_id", comment.ID)
			continue
		}
		inputs = append(inputs, state.FeedbackInput{
			Repo: repository.GitHub, EventID: comment.ID, PullNumber: comment.PullNumber,
			IssueKey: issueKey, Body: comment.Body, OccurredAt: comment.CreatedAt,
			Metadata: map[string]any{
				"author": comment.Author, "url": comment.HTMLURL, "path": comment.Path,
				"line": comment.Line, "original_line": comment.OriginalLine,
				"commit_id": comment.CommitID, "original_commit_id": comment.OriginalCommitID,
			},
		})
	}
	if dryRun {
		d.logger.Info("dry-run review comments", "repo", repository.GitHub, "fetched", len(comments), "qualifying", len(inputs), "since", since)
		return nil
	}
	inserted := 0
	if len(inputs) > 0 {
		inserted, err = d.store.IngestReviewComments(ctx, inputs, successAt)
	} else {
		err = d.store.AdvanceCursor(ctx, repository.GitHub, reviewCommentStream, successAt)
	}
	if err != nil {
		return err
	}
	d.logger.Info("review comments ingested", "repo", repository.GitHub, "fetched", len(comments), "inserted", inserted)
	return nil
}

func (d *Dispatcher) discoverReviews(ctx context.Context, repository config.Repository, owner, repo string, pulls []gh.PullRequest, dryRun bool) error {
	if !repository.ProcessChangesRequested {
		return nil
	}
	cutoff := d.now().Add(-d.cfg.BootstrapLookback.Duration)
	var inputs []state.FeedbackInput
	var laneErrors []error
	for _, pull := range pulls {
		if pull.Draft {
			continue
		}
		issueKey, ok := issueKeyFromPull(d.cfg.Multica.Prefix, pull.Title, pull.Body)
		if !ok {
			continue
		}
		reviews, err := d.github.ListReviews(ctx, owner, repo, pull.Number)
		if err != nil {
			laneErrors = append(laneErrors, err)
			continue
		}
		for _, review := range reviews {
			if !strings.EqualFold(review.State, "CHANGES_REQUESTED") || review.SubmittedAt.Before(cutoff) {
				continue
			}
			inputs = append(inputs, state.FeedbackInput{
				Repo: repository.GitHub, EventID: review.ID, PullNumber: pull.Number,
				IssueKey: issueKey, Body: review.Body, OccurredAt: review.SubmittedAt,
				Metadata: map[string]any{
					"author": review.Author, "url": review.HTMLURL,
					"state": review.State, "commit_id": review.CommitID,
				},
			})
		}
	}
	if dryRun {
		d.logger.Info("dry-run requested-changes reviews", "repo", repository.GitHub, "qualifying", len(inputs))
		return errors.Join(laneErrors...)
	}
	if len(inputs) > 0 {
		inserted, err := d.store.IngestReviews(ctx, inputs)
		if err != nil {
			laneErrors = append(laneErrors, err)
		} else {
			d.logger.Info("requested-changes reviews ingested", "repo", repository.GitHub, "inserted", inserted)
		}
	}
	return errors.Join(laneErrors...)
}

func (d *Dispatcher) queueFeedback(ctx context.Context) error {
	items, err := d.store.PendingFeedback(ctx, 100)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	if d.multica == nil {
		return errors.New("Multica is unavailable for feedback routing")
	}
	agents, err := d.multica.ListAgents(ctx)
	if err != nil {
		return err
	}
	agentsByID := make(map[string]multica.Agent, len(agents))
	for _, agent := range agents {
		agentsByID[agent.ID] = agent
	}
	for _, item := range items {
		issue, err := d.multica.GetIssue(ctx, item.IssueKey)
		if err != nil {
			_ = d.store.RetryFeedback(ctx, item, err.Error(), multica.RetryDelay(item.Attempts))
			continue
		}
		if !isActiveIssue(issue.Status) {
			_ = d.store.PermanentFeedback(ctx, item, "issue is not active: "+issue.Status)
			continue
		}
		mention, err := d.assigneeMention(ctx, issue, agentsByID)
		if err != nil {
			_ = d.store.PermanentFeedback(ctx, item, err.Error())
			continue
		}
		marker, body := feedbackMessage(item, mention)
		outbox := state.OutboxInput{
			EventKey: fmt.Sprintf("%s:%s:%d", item.Kind, item.Repo, item.EventID),
			Kind:     item.Kind, IssueKey: item.IssueKey, Body: body, Marker: marker,
		}
		if err := d.store.QueueFeedback(ctx, item, outbox); err != nil {
			return err
		}
		d.logger.Info("feedback queued", "kind", item.Kind, "repo", item.Repo,
			"event_id", item.EventID, "issue", item.IssueKey, "assignee_id", issue.AssigneeID)
	}
	return nil
}

func (d *Dispatcher) deliverOutbox(ctx context.Context) error {
	items, err := d.store.DueOutbox(ctx, 100)
	if err != nil {
		return err
	}
	var deliveryErrors []error
	for _, item := range items {
		if item.Kind == "review_request" {
			if !d.cfg.ReviewDispatch.IsEnabled() {
				deliveryErrors = append(deliveryErrors, fmt.Errorf("%s delivery blocked: auto review is disabled", item.EventKey))
				continue
			}
			if d.reviewer == nil {
				err := errors.New("review webhook client is not configured")
				_ = d.store.RetryOutbox(ctx, item.ID, err.Error(), multica.RetryDelay(item.Attempts))
				deliveryErrors = append(deliveryErrors, fmt.Errorf("%s delivery: %w", item.EventKey, err))
				continue
			}
			if err := d.reviewer.Trigger(ctx, item.Marker, item.EventKey, []byte(item.Body)); err != nil {
				_ = d.store.RetryOutbox(ctx, item.ID, err.Error(), multica.RetryDelay(item.Attempts))
				deliveryErrors = append(deliveryErrors, fmt.Errorf("%s delivery: %w", item.EventKey, err))
				continue
			}
			if err := d.store.DeliverOutbox(ctx, item.ID); err != nil {
				deliveryErrors = append(deliveryErrors, err)
				continue
			}
			d.logger.Info("review webhook delivered", "event_key", item.EventKey, "reviewer_id", item.Marker)
			continue
		}
		if d.multica == nil {
			err := errors.New("Multica is unavailable for feedback delivery")
			_ = d.store.RetryOutbox(ctx, item.ID, err.Error(), multica.RetryDelay(item.Attempts))
			deliveryErrors = append(deliveryErrors, fmt.Errorf("%s delivery: %w", item.EventKey, err))
			continue
		}
		comments, err := d.multica.ListComments(ctx, item.IssueKey)
		if err != nil {
			_ = d.store.RetryOutbox(ctx, item.ID, err.Error(), multica.RetryDelay(item.Attempts))
			deliveryErrors = append(deliveryErrors, fmt.Errorf("%s comment lookup: %w", item.EventKey, err))
			continue
		}
		alreadyDelivered := false
		for _, comment := range comments {
			if strings.Contains(comment.Content, item.Marker) {
				alreadyDelivered = true
				break
			}
		}
		if !alreadyDelivered {
			if err := d.multica.AddComment(ctx, item.IssueKey, item.Body); err != nil {
				_ = d.store.RetryOutbox(ctx, item.ID, err.Error(), multica.RetryDelay(item.Attempts))
				deliveryErrors = append(deliveryErrors, fmt.Errorf("%s delivery: %w", item.EventKey, err))
				continue
			}
		}
		if err := d.store.DeliverOutbox(ctx, item.ID); err != nil {
			deliveryErrors = append(deliveryErrors, err)
			continue
		}
		d.logger.Info("outbox delivered", "event_key", item.EventKey, "kind", item.Kind, "issue", item.IssueKey, "recovered_by_marker", alreadyDelivered)
	}
	return errors.Join(deliveryErrors...)
}

func (d *Dispatcher) selectReviewers(ctx context.Context, repository config.Repository, issue *multica.Issue, pull gh.PullRequest) ([]config.ReviewAgent, error) {
	readiness, readinessByAgent, err := d.configuredReviewerReadiness(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.store.ReplaceAllReviewerReadiness(ctx, readiness); err != nil {
		return nil, fmt.Errorf("record reviewer runtime readiness: %w", err)
	}
	excluded := map[string]struct{}{}
	if issue != nil && issue.AssigneeType == "agent" && issue.AssigneeID != "" {
		excluded[issue.AssigneeID] = struct{}{}
	}
	var candidates []config.ReviewAgent
	for _, reviewerID := range repository.ReviewerIDs {
		if _, skip := excluded[reviewerID]; skip {
			continue
		}
		agent, ok := d.cfg.ReviewAgent(reviewerID)
		if !ok {
			return nil, fmt.Errorf("reviewer %s is not configured", reviewerID)
		}
		if item, ok := readinessByAgent[reviewerID]; !ok || !item.Ready {
			continue
		}
		candidates = append(candidates, agent)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("repository has no eligible ready reviewers (%s)", reviewerReadinessSummary(readiness))
	}
	seed := fmt.Sprintf("%s#%d@%s", repository.GitHub, pull.Number, pull.HeadSHA)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	sort.Slice(candidates, func(i, j int) bool {
		left := reviewerScore(seed, candidates[i].ID)
		right := reviewerScore(seed, candidates[j].ID)
		if left == right {
			return candidates[i].ID < candidates[j].ID
		}
		return left < right
	})
	count := repository.ReviewerCount
	if count <= 0 {
		count = 1
	}
	if count > len(candidates) {
		count = len(candidates)
		d.logger.Warn("reviewer count degraded", "repo", repository.GitHub, "requested", repository.ReviewerCount, "available", len(candidates))
	}
	return candidates[:count], nil
}

func (d *Dispatcher) recordConfiguredReviewers(ctx context.Context) error {
	items, _, err := d.configuredReviewerReadiness(ctx)
	if err != nil {
		return err
	}
	return d.store.ReplaceAllReviewerReadiness(ctx, items)
}

func (d *Dispatcher) configuredReviewerReadiness(ctx context.Context) ([]state.ReviewerReadiness, map[string]state.ReviewerReadiness, error) {
	if d.multica == nil {
		return nil, nil, errors.New("Multica is unavailable for reviewer runtime readiness")
	}
	agents, err := d.multica.ListAgents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list reviewer agents: %w", err)
	}
	runtimes, err := d.multica.ListRuntimes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list reviewer runtimes: %w", err)
	}
	items, byAgent := reviewerRuntimeReadiness(d.cfg.ReviewDispatch.Agents, agents, runtimes)
	return items, byAgent, nil
}

func reviewerRuntimeReadiness(configured []config.ReviewAgent, agents []multica.Agent, runtimes []multica.Runtime) ([]state.ReviewerReadiness, map[string]state.ReviewerReadiness) {
	agentByID := make(map[string]multica.Agent, len(agents))
	for _, agent := range agents {
		agentByID[agent.ID] = agent
	}
	runtimeByID := make(map[string]multica.Runtime, len(runtimes))
	for _, runtime := range runtimes {
		runtimeByID[runtime.ID] = runtime
	}
	items := make([]state.ReviewerReadiness, 0, len(configured))
	byAgent := make(map[string]state.ReviewerReadiness, len(configured))
	for _, configuredAgent := range configured {
		item := state.ReviewerReadiness{SquadID: "review_dispatch", AgentID: configuredAgent.ID, AgentName: configuredAgent.Name}
		agent, found := agentByID[configuredAgent.ID]
		if !found {
			item.Reason = "agent_missing"
		} else {
			item.AgentName = agent.Name
			item.AgentStatus = agent.Status
			item.RuntimeID = agent.RuntimeID
			switch {
			case agent.Archived:
				item.Reason = "agent_archived"
			case strings.EqualFold(agent.Status, "disabled"), strings.EqualFold(agent.Status, "archived"):
				item.Reason = "agent_" + strings.ToLower(agent.Status)
			case !agent.RuntimeBound || agent.RuntimeID == "":
				item.Reason = "runtime_unbound"
			default:
				runtime, found := runtimeByID[agent.RuntimeID]
				if !found {
					item.Reason = "runtime_missing"
				} else {
					item.RuntimeName = runtime.Name
					item.RuntimeStatus = runtime.Status
					if strings.EqualFold(runtime.Status, "online") {
						item.Ready = true
						item.Reason = "ready"
					} else if runtime.Status == "" {
						item.Reason = "runtime_unknown"
					} else {
						item.Reason = "runtime_" + strings.ToLower(runtime.Status)
					}
				}
			}
		}
		items = append(items, item)
		byAgent[item.AgentID] = item
	}
	return items, byAgent
}

func reviewerReadinessSummary(items []state.ReviewerReadiness) string {
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Reason]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	if len(parts) == 0 {
		return "no configured reviewers"
	}
	return strings.Join(parts, ", ")
}

func (d *Dispatcher) assigneeMention(ctx context.Context, issue multica.Issue, agents map[string]multica.Agent) (string, error) {
	if issue.AssigneeID == "" {
		return "", errors.New("issue has no assignee")
	}
	switch issue.AssigneeType {
	case "agent":
		name := "assigned agent"
		agent, ok := agents[issue.AssigneeID]
		if ok && strings.TrimSpace(agent.Name) != "" {
			name = agent.Name
		}
		// Private agents may be valid issue assignees while remaining absent from
		// `agent list` for the dispatcher's member-scoped token. The mention URI,
		// not its display label, is authoritative for wake-up routing.
		return fmt.Sprintf("[@%s](mention://agent/%s)", name, issue.AssigneeID), nil
	case "squad":
		squad, err := d.multica.GetSquad(ctx, issue.AssigneeID)
		if err != nil {
			d.logger.Warn("assignee squad name unavailable; using ID-based mention",
				"squad_id", issue.AssigneeID, "error", err)
			return fmt.Sprintf("[@assigned squad](mention://squad/%s)", issue.AssigneeID), nil
		}
		return fmt.Sprintf("[@%s](mention://squad/%s)", squad.Name, squad.ID), nil
	default:
		return "", fmt.Errorf("unsupported issue assignee type %q", issue.AssigneeType)
	}
}

func issueKeyFromPull(prefix, title, body string) (string, bool) {
	pattern := regexp.MustCompile(`(?i)(?:^|[^A-Z0-9])(` + regexp.QuoteMeta(prefix) + `-([1-9][0-9]*))(?:[^A-Z0-9]|$)`)
	for _, value := range []string{title, body} {
		matches := pattern.FindStringSubmatch(value)
		if len(matches) == 3 {
			return strings.ToUpper(matches[1]), true
		}
	}
	return "", false
}

func hasFindingMarker(body, marker string) bool {
	pattern := regexp.MustCompile(`(?i)<!--\s*` + regexp.QuoteMeta(marker) + `(?:\s|-->)`)
	return pattern.MatchString(body)
}

func acceptReviewComment(repository config.Repository, body string) bool {
	return repository.ReviewCommentMode == "all" || hasFindingMarker(body, repository.FindingMarker)
}

func isActiveIssue(status string) bool {
	_, ok := activeIssueStatuses[strings.ToLower(status)]
	return ok
}

func reviewerScore(seed, agentID string) string {
	sum := sha256.Sum256([]byte(seed + agentID))
	return hex.EncodeToString(sum[:])
}

type reviewWebhookPayload struct {
	SchemaVersion int                `json:"schema_version"`
	EventType     string             `json:"event_type"`
	EventID       string             `json:"event_id"`
	OccurredAt    string             `json:"occurred_at"`
	ReviewRound   reviewRoundPayload `json:"review_round"`
	Repository    repositoryPayload  `json:"repository"`
	PullRequest   pullPayload        `json:"pull_request"`
	MulticaIssue  *issuePayload      `json:"multica_issue,omitempty"`
}

type reviewRoundPayload struct {
	ID            string `json:"id"`
	ReviewerID    string `json:"reviewer_agent_id"`
	ReviewerName  string `json:"reviewer_agent_name"`
	Engine        string `json:"engine"`
	OCRVersion    string `json:"ocr_version"`
	PolicyID      string `json:"policy_id"`
	FindingMarker string `json:"finding_marker"`
}

type repositoryPayload struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
}

type pullPayload struct {
	Number        int    `json:"number"`
	HTMLURL       string `json:"html_url"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	Draft         bool   `json:"draft"`
	BaseRef       string `json:"base_ref"`
	BaseSHA       string `json:"base_sha"`
	HeadRef       string `json:"head_ref"`
	HeadSHA       string `json:"head_sha"`
	ReviewFromSHA string `json:"review_from_sha"`
}

type issuePayload struct {
	ID          string `json:"id,omitempty"`
	Key         string `json:"key"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
}

func reviewRequestPayload(eventKey string, repository config.Repository, reviewer config.ReviewAgent, pull gh.PullRequest, issueKey string, issue *multica.Issue, now time.Time) (string, error) {
	payload := reviewWebhookPayload{
		SchemaVersion: 1,
		EventType:     "github_pr_review_requested",
		EventID:       eventKey,
		OccurredAt:    now.UTC().Format(time.RFC3339Nano),
		ReviewRound: reviewRoundPayload{
			ID:         fmt.Sprintf("%s#%d@%s", repository.GitHub, pull.Number, pull.HeadSHA),
			ReviewerID: reviewer.ID, ReviewerName: reviewer.Name,
			Engine: repository.ReviewEngine, OCRVersion: repository.OCRVersion,
			PolicyID: "ocr-review-v3", FindingMarker: repository.FindingMarker,
		},
		Repository: repositoryPayload{
			FullName: repository.GitHub,
			HTMLURL:  "https://github.com/" + repository.GitHub,
			CloneURL: "https://github.com/" + repository.GitHub + ".git",
		},
		PullRequest: pullPayload{
			Number: pull.Number, HTMLURL: pull.HTMLURL, Title: pull.Title,
			Author: pull.Author, Draft: pull.Draft,
			BaseRef: pull.BaseRef, BaseSHA: pull.BaseSHA,
			HeadRef: pull.HeadRef, HeadSHA: pull.HeadSHA,
			ReviewFromSHA: pull.BaseSHA,
		},
	}
	if issueKey != "" {
		payload.MulticaIssue = &issuePayload{Key: issueKey}
	}
	if issue != nil {
		payload.MulticaIssue = &issuePayload{
			ID: issue.ID, Key: issue.Identifier, Title: issue.Title,
			Description: truncateRunes(issue.Description, 16000), Status: issue.Status,
		}
		if payload.MulticaIssue.Key == "" {
			payload.MulticaIssue.Key = issueKey
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode review webhook payload: %w", err)
	}
	return string(encoded), nil
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func feedbackMessage(item state.PendingFeedback, mention string) (string, string) {
	markerKind := "github-review-comment-dispatch"
	idName := "comment"
	if item.Kind == "review" {
		markerKind = "github-review-dispatch"
		idName = "review"
	}
	marker := fmt.Sprintf("<!-- %s:v1 repo=%s %s=%d -->", markerKind, item.Repo, idName, item.EventID)
	url := strings.TrimSpace(stringMetadata(item.Metadata, "url"))
	if url == "" {
		url = fmt.Sprintf("https://github.com/%s/pull/%d", item.Repo, item.PullNumber)
	}
	body := fmt.Sprintf(`%s

PR %s#%d 有新的 Review 意见，请打开 GitHub 链接查看并处理：

%s

%s`, mention, item.Repo, item.PullNumber, url, marker)
	return marker, body
}

func stringMetadata(metadata map[string]any, key string) string {
	value := metadata[key]
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
