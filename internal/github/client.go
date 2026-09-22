package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

const userAgent = "multica-github-dispatcher/1"

type Client struct {
	baseURL    string
	httpClient *http.Client
	auth       config.SecretRef
	proxyLabel string
	redactions []string
}

type PullRequest struct {
	Number    int
	Title     string
	HTMLURL   string
	State     string
	Draft     bool
	HeadSHA   string
	BaseSHA   string
	BaseRef   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ReviewComment struct {
	ID               int64
	PullNumber       int
	Body             string
	HTMLURL          string
	Author           string
	Path             string
	Line             *int
	OriginalLine     *int
	CommitID         string
	OriginalCommitID string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Review struct {
	ID          int64
	Body        string
	State       string
	HTMLURL     string
	Author      string
	CommitID    string
	SubmittedAt time.Time
}

func New(cfg config.GitHubConfig) (*Client, error) {
	transport, proxyLabel, err := newTransport(cfg.Proxy)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Proxy.RequestTimeout.Duration
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: cfg.APIBaseURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
		auth:       cfg.Auth,
		proxyLabel: proxyLabel,
		redactions: proxyRedactions(cfg.Proxy),
	}, nil
}

func (c *Client) ProxyLabel() string { return c.proxyLabel }

func (c *Client) Check(ctx context.Context) error {
	var response struct {
		Resources map[string]json.RawMessage `json:"resources"`
	}
	if err := c.getJSON(ctx, c.baseURL+"/rate_limit", &response); err != nil {
		return fmt.Errorf("github connectivity check failed: %w", err)
	}
	if response.Resources == nil {
		return errors.New("github connectivity check returned an invalid response")
	}
	return nil
}

func (c *Client) ListOpenPulls(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100", c.baseURL, url.PathEscape(owner), url.PathEscape(repo))
	var raw []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Draft   bool   `json:"draft"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"base"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := c.getAll(ctx, endpoint, &raw); err != nil {
		return nil, fmt.Errorf("list open pulls: %w", err)
	}
	result := make([]PullRequest, 0, len(raw))
	for _, item := range raw {
		result = append(result, PullRequest{
			Number: item.Number, Title: item.Title, HTMLURL: item.HTMLURL,
			State: item.State, Draft: item.Draft, HeadSHA: item.Head.SHA,
			BaseSHA: item.Base.SHA, BaseRef: item.Base.Ref,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		})
	}
	return result, nil
}

func (c *Client) ListReviewComments(ctx context.Context, owner, repo string, since time.Time) ([]ReviewComment, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/comments?sort=updated&direction=asc&per_page=100&since=%s",
		c.baseURL, url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(since.UTC().Format(time.RFC3339)))
	var raw []struct {
		ID             int64  `json:"id"`
		Body           string `json:"body"`
		HTMLURL        string `json:"html_url"`
		Path           string `json:"path"`
		Line           *int   `json:"line"`
		OriginalLine   *int   `json:"original_line"`
		CommitID       string `json:"commit_id"`
		OriginalCommit string `json:"original_commit_id"`
		PullRequestURL string `json:"pull_request_url"`
		User           struct {
			Login string `json:"login"`
		} `json:"user"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := c.getAll(ctx, endpoint, &raw); err != nil {
		return nil, fmt.Errorf("list review comments: %w", err)
	}
	result := make([]ReviewComment, 0, len(raw))
	for _, item := range raw {
		number, err := pullNumberFromURL(item.PullRequestURL)
		if err != nil {
			return nil, err
		}
		result = append(result, ReviewComment{
			ID: item.ID, PullNumber: number, Body: item.Body, HTMLURL: item.HTMLURL,
			Author: item.User.Login, Path: item.Path, Line: item.Line, OriginalLine: item.OriginalLine,
			CommitID: item.CommitID, OriginalCommitID: item.OriginalCommit,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		})
	}
	return result, nil
}

func (c *Client) ListReviews(ctx context.Context, owner, repo string, pullNumber int) ([]Review, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), pullNumber)
	var raw []struct {
		ID       int64  `json:"id"`
		Body     string `json:"body"`
		State    string `json:"state"`
		HTMLURL  string `json:"html_url"`
		CommitID string `json:"commit_id"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	if err := c.getAll(ctx, endpoint, &raw); err != nil {
		return nil, fmt.Errorf("list reviews for pull %d: %w", pullNumber, err)
	}
	result := make([]Review, 0, len(raw))
	for _, item := range raw {
		result = append(result, Review{
			ID: item.ID, Body: item.Body, State: item.State, HTMLURL: item.HTMLURL,
			Author: item.User.Login, CommitID: item.CommitID, SubmittedAt: item.SubmittedAt,
		})
	}
	return result, nil
}

func (c *Client) getAll(ctx context.Context, endpoint string, destination any) error {
	// GitHub pagination returns arrays. Accumulate pages through raw messages so
	// callers retain concrete result types without reflection-heavy mutation.
	var pages []json.RawMessage
	next := endpoint
	for next != "" {
		request, err := c.newRequest(ctx, next)
		if err != nil {
			return err
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			return errors.New("github request failed: " + c.redact(err.Error()))
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read github response: %w", readErr)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return githubStatusError(response, body)
		}
		var page []json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("decode github page: %w", err)
		}
		pages = append(pages, page...)
		next = nextLink(response.Header.Get("Link"))
	}
	combined, err := json.Marshal(pages)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(combined, destination); err != nil {
		return fmt.Errorf("decode github result: %w", err)
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, destination any) error {
	request, err := c.newRequest(ctx, endpoint)
	if err != nil {
		return err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return errors.New("github request failed: " + c.redact(err.Error()))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read github response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return githubStatusError(response, body)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decode github response: %w", err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, endpoint string) (*http.Request, error) {
	token, err := c.auth.Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve github token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Authorization", "Bearer "+token)
	return request, nil
}

func githubStatusError(response *http.Response, body []byte) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	message := strings.TrimSpace(payload.Message)
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	if len(message) > 300 {
		message = message[:300]
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		reset := response.Header.Get("X-RateLimit-Reset")
		return fmt.Errorf("github returned %s: %s (rate-limit-reset=%s)", response.Status, message, reset)
	}
	return fmt.Errorf("github returned %s: %s", response.Status, message)
}

func pullNumberFromURL(value string) (int, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return 0, fmt.Errorf("parse pull request URL: %w", err)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return 0, errors.New("pull request URL has no path")
	}
	number, err := strconv.Atoi(segments[len(segments)-1])
	if err != nil {
		return 0, fmt.Errorf("parse pull number from URL: %w", err)
	}
	return number, nil
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		pieces := strings.Split(strings.TrimSpace(part), ";")
		if len(pieces) < 2 || !strings.Contains(pieces[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(pieces[0]), "<>")
	}
	return ""
}

func newTransport(cfg config.ProxyConfig) (*http.Transport, string, error) {
	connectTimeout := cfg.ConnectTimeout.Duration
	if connectTimeout == 0 {
		connectTimeout = 10 * time.Second
	}
	directDialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           directDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   connectTimeout,
		ExpectContinueTimeout: time.Second,
	}

	switch cfg.Type {
	case "none":
		return transport, "direct", nil
	case "standard_env":
		envConfig := httpproxy.FromEnvironment()
		proxyFunc := envConfig.ProxyFunc()
		probeURL, _ := url.Parse("https://api.github.com")
		probeProxy, err := proxyFunc(probeURL)
		if err != nil {
			return nil, "", fmt.Errorf("resolve standard proxy environment: %w", err)
		}
		if cfg.Required && probeProxy == nil {
			return nil, "", errors.New("github proxy is required but standard proxy environment resolves to direct")
		}
		transport.Proxy = func(request *http.Request) (*url.URL, error) {
			return proxyFunc(request.URL)
		}
		if probeProxy == nil {
			return transport, "standard_env(direct)", nil
		}
		return transport, "standard_env(" + RedactProxyURL(probeProxy.String()) + ")", nil
	case "url_env":
		value := os.Getenv(cfg.Name)
		if value == "" {
			if cfg.Required {
				return nil, "", fmt.Errorf("github proxy is required but %s is empty", cfg.Name)
			}
			return transport, "url_env(direct)", nil
		}
		proxyURL, err := url.Parse(value)
		if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
			return nil, "", fmt.Errorf("invalid proxy URL in %s", cfg.Name)
		}
		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
		case "socks5", "socks5h":
			var auth *proxy.Auth
			if proxyURL.User != nil {
				password, _ := proxyURL.User.Password()
				auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
			}
			dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, directDialer)
			if err != nil {
				return nil, "", errors.New("configure SOCKS5 proxy")
			}
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		default:
			return nil, "", fmt.Errorf("unsupported github proxy scheme %q", proxyURL.Scheme)
		}
		return transport, RedactProxyURL(value), nil
	default:
		return nil, "", fmt.Errorf("unsupported github proxy type %q", cfg.Type)
	}
}

func RedactProxyURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "configured"
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func proxyRedactions(cfg config.ProxyConfig) []string {
	var values []string
	add := func(value string) {
		if value == "" {
			return
		}
		values = append(values, value)
		parsed, err := url.Parse(value)
		if err == nil && parsed.User != nil {
			values = append(values, parsed.User.Username())
			if password, ok := parsed.User.Password(); ok {
				values = append(values, password)
			}
		}
	}
	switch cfg.Type {
	case "url_env":
		add(os.Getenv(cfg.Name))
	case "standard_env":
		for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
			add(os.Getenv(name))
		}
	}
	return values
}

func (c *Client) redact(message string) string {
	result := message
	for _, value := range c.redactions {
		if value != "" {
			result = strings.ReplaceAll(result, value, "[REDACTED]")
		}
	}
	return result
}
