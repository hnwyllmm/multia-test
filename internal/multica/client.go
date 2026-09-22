package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
)

type Client struct {
	cliPath     string
	serverURL   string
	workspaceID string
	auth        config.SecretRef
}

type Issue struct {
	ID           string         `json:"id"`
	Identifier   string         `json:"identifier"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	Status       string         `json:"status"`
	AssigneeID   string         `json:"assignee_id"`
	AssigneeType string         `json:"assignee_type"`
	Raw          map[string]any `json:"-"`
}

type Agent struct {
	ID           string
	Name         string
	RuntimeBound bool
	Status       string
	Archived     bool
}

type Squad struct {
	ID       string
	Name     string
	LeaderID string
}

type SquadMember struct {
	ID   string
	Type string
	Role string
}

type Comment struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func New(cfg config.MulticaConfig) (*Client, error) {
	serverURL := os.Getenv(cfg.ServerURLEnv)
	if serverURL == "" {
		return nil, fmt.Errorf("required environment variable %s is empty", cfg.ServerURLEnv)
	}
	return &Client{
		cliPath: cfg.CLIPath, serverURL: serverURL,
		workspaceID: cfg.WorkspaceID, auth: cfg.Auth,
	}, nil
}

func (c *Client) Check(ctx context.Context) error {
	var workspace struct {
		ID string `json:"id"`
	}
	if err := c.runJSON(ctx, nil, &workspace, "workspace", "get", c.workspaceID, "--output", "json"); err != nil {
		return fmt.Errorf("multica workspace check failed: %w", err)
	}
	if workspace.ID != c.workspaceID {
		return errors.New("multica workspace check returned the wrong workspace")
	}
	return nil
}

func (c *Client) GetIssue(ctx context.Context, identifier string) (Issue, error) {
	var raw map[string]any
	if err := c.runJSON(ctx, nil, &raw, "issue", "get", identifier, "--output", "json"); err != nil {
		return Issue{}, fmt.Errorf("get issue %s: %w", identifier, err)
	}
	return Issue{
		ID: stringValue(raw, "id"), Identifier: stringValue(raw, "identifier"),
		Title: stringValue(raw, "title"), Description: stringValue(raw, "description"),
		Status: stringValue(raw, "status"), AssigneeID: stringValue(raw, "assignee_id"),
		AssigneeType: stringValue(raw, "assignee_type"), Raw: raw,
	}, nil
}

func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	raw, err := c.runList(ctx, "agent", "list", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	result := make([]Agent, 0, len(raw))
	for _, item := range raw {
		result = append(result, Agent{
			ID: stringValue(item, "id"), Name: stringValue(item, "name"),
			RuntimeBound: boolValue(item, "runtime_bound"), Status: stringValue(item, "status"),
			Archived: item["archived_at"] != nil && stringValue(item, "archived_at") != "",
		})
	}
	return result, nil
}

func (c *Client) GetSquad(ctx context.Context, squadID string) (Squad, error) {
	var raw map[string]any
	if err := c.runJSON(ctx, nil, &raw, "squad", "get", squadID, "--output", "json"); err != nil {
		return Squad{}, fmt.Errorf("get squad: %w", err)
	}
	leaderID := stringValue(raw, "leader_id")
	if leaderID == "" {
		if leader, ok := raw["leader"].(map[string]any); ok {
			leaderID = stringValue(leader, "id")
		}
	}
	return Squad{ID: stringValue(raw, "id"), Name: stringValue(raw, "name"), LeaderID: leaderID}, nil
}

func (c *Client) ListSquadMembers(ctx context.Context, squadID string) ([]SquadMember, error) {
	raw, err := c.runList(ctx, "squad", "member", "list", squadID, "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("list squad members: %w", err)
	}
	result := make([]SquadMember, 0, len(raw))
	for _, item := range raw {
		id := firstString(item, "member_id", "agent_id", "id")
		kind := firstString(item, "member_type", "type")
		role := stringValue(item, "role")
		if nested, ok := item["member"].(map[string]any); ok {
			if id == "" {
				id = stringValue(nested, "id")
			}
			if kind == "" {
				kind = firstString(nested, "member_type", "type")
			}
		}
		if kind == "" {
			kind = "agent"
		}
		result = append(result, SquadMember{ID: id, Type: kind, Role: role})
	}
	return result, nil
}

func (c *Client) ListComments(ctx context.Context, issueID string) ([]Comment, error) {
	raw, err := c.runList(ctx, "issue", "comment", "list", issueID, "--full", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("list issue comments: %w", err)
	}
	result := make([]Comment, 0, len(raw))
	for _, item := range raw {
		result = append(result, Comment{
			ID: stringValue(item, "id"), Content: stringValue(item, "content"),
			CreatedAt: stringValue(item, "created_at"),
		})
	}
	return result, nil
}

func (c *Client) AddComment(ctx context.Context, issueID, content string) error {
	var response json.RawMessage
	if err := c.runJSON(ctx, strings.NewReader(content), &response,
		"issue", "comment", "add", issueID, "--content-stdin", "--output", "json"); err != nil {
		return fmt.Errorf("add issue comment: %w", err)
	}
	return nil
}

func (c *Client) runList(ctx context.Context, args ...string) ([]map[string]any, error) {
	var raw json.RawMessage
	if err := c.runJSON(ctx, nil, &raw, args...); err != nil {
		return nil, err
	}
	var direct []map[string]any
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, errors.New("multica list response is not JSON")
	}
	for _, key := range []string{"items", "agents", "members", "comments", "squads", "data"} {
		value, ok := wrapper[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, &direct); err == nil {
			return direct, nil
		}
	}
	return nil, errors.New("multica list response has no supported collection")
}

func (c *Client) runJSON(ctx context.Context, stdin ioReader, destination any, args ...string) error {
	token, err := c.auth.Resolve()
	if err != nil {
		return fmt.Errorf("resolve multica token: %w", err)
	}
	fullArgs := append([]string{}, args...)
	fullArgs = append(fullArgs, "--server-url", c.serverURL, "--workspace-id", c.workspaceID)
	command := exec.CommandContext(ctx, c.cliPath, fullArgs...)
	if stdin != nil {
		command.Stdin = stdin
	}
	command.Env = sanitizedEnvironment(token, c.serverURL, c.workspaceID)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		message = redact(message, token)
		if len(message) > 800 {
			message = message[:800]
		}
		if message == "" {
			message = err.Error()
		}
		return errors.New(message)
	}
	if err := json.Unmarshal(stdout.Bytes(), destination); err != nil {
		return fmt.Errorf("decode multica JSON: %w", err)
	}
	return nil
}

type ioReader interface {
	Read([]byte) (int, error)
}

func sanitizedEnvironment(token, serverURL, workspaceID string) []string {
	blocked := map[string]struct{}{
		"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {},
		"http_proxy": {}, "https_proxy": {}, "all_proxy": {}, "no_proxy": {},
		"MULTICA_TOKEN": {}, "MULTICA_SERVER_URL": {}, "MULTICA_WORKSPACE_ID": {},
	}
	environment := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, skip := blocked[key]; !skip {
			environment = append(environment, item)
		}
	}
	environment = append(environment,
		"MULTICA_TOKEN="+token,
		"MULTICA_SERVER_URL="+serverURL,
		"MULTICA_WORKSPACE_ID="+workspaceID,
	)
	return environment
}

func redact(value string, secrets ...string) string {
	result := value
	for _, secret := range secrets {
		if secret != "" {
			result = strings.ReplaceAll(result, secret, "[REDACTED]")
		}
	}
	return result
}

func stringValue(item map[string]any, key string) string {
	value, ok := item[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(item, key); value != "" {
			return value
		}
	}
	return ""
}

func boolValue(item map[string]any, key string) bool {
	value, ok := item[key]
	if !ok || value == nil {
		return false
	}
	if result, ok := value.(bool); ok {
		return result
	}
	return strings.EqualFold(fmt.Sprint(value), "true")
}

func RetryDelay(attempt int) time.Duration {
	switch {
	case attempt <= 0:
		return time.Minute
	case attempt == 1:
		return 5 * time.Minute
	case attempt == 2:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}
