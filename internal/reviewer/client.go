package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
)

const userAgent = "multica-github-dispatcher/1"

type Client struct {
	agents     map[string]config.ReviewAgent
	httpClient *http.Client
}

func New(cfg config.ReviewDispatchConfig) (*Client, error) {
	timeout := cfg.RequestTimeout.Duration
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	agents := make(map[string]config.ReviewAgent, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		if agent.ID == "" {
			return nil, errors.New("review agent id is empty")
		}
		agents[agent.ID] = agent
	}
	return &Client{
		agents: agents,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
		},
	}, nil
}

func (c *Client) Check() error {
	if len(c.agents) == 0 {
		return errors.New("no review webhook agents configured")
	}
	for id, agent := range c.agents {
		value, err := agent.Webhook.Resolve()
		if err != nil {
			return fmt.Errorf("resolve review webhook for agent %s: %w", id, err)
		}
		if _, err := validateWebhookURL(value); err != nil {
			return fmt.Errorf("review webhook for agent %s is invalid: %w", id, err)
		}
	}
	return nil
}

func (c *Client) Trigger(ctx context.Context, agentID, eventKey string, payload []byte) error {
	agent, found := c.agents[agentID]
	if !found {
		return fmt.Errorf("review agent %s has no webhook", agentID)
	}
	if !json.Valid(payload) {
		return errors.New("review webhook payload is not valid JSON")
	}
	webhook, err := agent.Webhook.Resolve()
	if err != nil {
		return fmt.Errorf("resolve review webhook for agent %s: %w", agentID, err)
	}
	endpoint, err := validateWebhookURL(webhook)
	if err != nil {
		return fmt.Errorf("review webhook for agent %s is invalid: %w", agentID, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return errors.New("create review webhook request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Idempotency-Key", eventKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		message := strings.ReplaceAll(err.Error(), webhook, "<redacted>")
		return fmt.Errorf("review webhook request failed: %s", message)
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("review webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

func validateWebhookURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return nil, errors.New("must not contain URL user information")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("must not contain a fragment")
	}
	return parsed, nil
}
