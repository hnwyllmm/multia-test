package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultGitHubAPIBaseURL = "https://api.github.com"

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	value, err := time.ParseDuration(strings.TrimSpace(node.Value))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	if value <= 0 {
		return fmt.Errorf("duration must be positive: %q", node.Value)
	}
	d.Duration = value
	return nil
}

type SecretRef struct {
	Type string `yaml:"type"`
	Name string `yaml:"name,omitempty"`
	Path string `yaml:"path,omitempty"`
}

func (s SecretRef) Validate(label string) error {
	switch s.Type {
	case "env":
		if !validEnvName(s.Name) {
			return fmt.Errorf("%s env name is invalid", label)
		}
		if s.Path != "" {
			return fmt.Errorf("%s must not set path for env source", label)
		}
	case "file":
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("%s file path is required", label)
		}
		if s.Name != "" {
			return fmt.Errorf("%s must not set name for file source", label)
		}
	default:
		return fmt.Errorf("%s type must be env or file", label)
	}
	return nil
}

func (s SecretRef) Resolve() (string, error) {
	switch s.Type {
	case "env":
		value := os.Getenv(s.Name)
		if value == "" {
			return "", fmt.Errorf("required environment variable %s is empty", s.Name)
		}
		return value, nil
	case "file":
		info, err := os.Lstat(s.Path)
		if err != nil {
			return "", fmt.Errorf("read secret file metadata: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("secret path must be a regular file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("secret file permissions must be 0600 or stricter, got %04o", info.Mode().Perm())
		}
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		if value == "" {
			return "", errors.New("secret file is empty")
		}
		return value, nil
	default:
		return "", errors.New("unsupported secret source")
	}
}

type ProxyConfig struct {
	Type           string   `yaml:"type"`
	Name           string   `yaml:"name,omitempty"`
	Required       bool     `yaml:"required,omitempty"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	RequestTimeout Duration `yaml:"request_timeout"`
}

type GitHubConfig struct {
	APIBaseURL string      `yaml:"api_base_url,omitempty"`
	Auth       SecretRef   `yaml:"auth"`
	Proxy      ProxyConfig `yaml:"proxy"`
}

type MulticaConfig struct {
	CLIPath      string    `yaml:"cli_path,omitempty"`
	ServerURLEnv string    `yaml:"server_url_env"`
	WorkspaceID  string    `yaml:"workspace_id"`
	Prefix       string    `yaml:"workspace_prefix"`
	Auth         SecretRef `yaml:"auth"`
}

type Repository struct {
	GitHub                  string `yaml:"github"`
	ReviewerSquadID         string `yaml:"reviewer_squad_id"`
	ReviewerCount           int    `yaml:"reviewer_count"`
	ReviewEngine            string `yaml:"review_engine"`
	OCRVersion              string `yaml:"ocr_version"`
	FixMarker               string `yaml:"fix_marker"`
	ProcessChangesRequested bool   `yaml:"process_changes_requested"`
	TrustMode               string `yaml:"trust_mode"`
}

func (r Repository) OwnerRepo() (string, string, error) {
	parts := strings.Split(r.GitHub, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository %q must be owner/name", r.GitHub)
	}
	return parts[0], parts[1], nil
}

type Config struct {
	PollInterval      Duration      `yaml:"poll_interval"`
	BootstrapLookback Duration      `yaml:"bootstrap_lookback"`
	GitHub            GitHubConfig  `yaml:"github"`
	Multica           MulticaConfig `yaml:"multica"`
	Repositories      []Repository  `yaml:"repositories"`
	StateDB           string        `yaml:"state_db"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.setDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) setDefaultsAndValidate() error {
	if c.PollInterval.Duration == 0 {
		c.PollInterval.Duration = time.Minute
	}
	if c.BootstrapLookback.Duration == 0 {
		c.BootstrapLookback.Duration = 24 * time.Hour
	}
	if c.GitHub.APIBaseURL == "" {
		c.GitHub.APIBaseURL = defaultGitHubAPIBaseURL
	}
	baseURL, err := url.Parse(c.GitHub.APIBaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return errors.New("github.api_base_url must be an absolute URL")
	}
	c.GitHub.APIBaseURL = strings.TrimRight(c.GitHub.APIBaseURL, "/")
	if err := c.GitHub.Auth.Validate("github.auth"); err != nil {
		return err
	}
	if err := validateProxy(c.GitHub.Proxy); err != nil {
		return err
	}
	if c.Multica.CLIPath == "" {
		c.Multica.CLIPath = "multica"
	}
	if !validEnvName(c.Multica.ServerURLEnv) {
		return errors.New("multica.server_url_env is invalid")
	}
	if c.Multica.WorkspaceID == "" {
		return errors.New("multica.workspace_id is required")
	}
	if !regexp.MustCompile(`^[A-Z][A-Z0-9]*$`).MatchString(c.Multica.Prefix) {
		return errors.New("multica.workspace_prefix must contain uppercase letters and digits")
	}
	if err := c.Multica.Auth.Validate("multica.auth"); err != nil {
		return err
	}
	if len(c.Repositories) == 0 {
		return errors.New("at least one repository is required")
	}
	seen := map[string]struct{}{}
	for i := range c.Repositories {
		r := &c.Repositories[i]
		if _, _, err := r.OwnerRepo(); err != nil {
			return err
		}
		key := strings.ToLower(r.GitHub)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate repository %s", r.GitHub)
		}
		seen[key] = struct{}{}
		if r.ReviewerSquadID == "" {
			return fmt.Errorf("repository %s reviewer_squad_id is required", r.GitHub)
		}
		if r.ReviewerCount <= 0 {
			r.ReviewerCount = 1
		}
		if r.ReviewEngine == "" {
			r.ReviewEngine = "ocr_delegate"
		}
		if r.ReviewEngine != "ocr_delegate" {
			return fmt.Errorf("repository %s unsupported review_engine %q", r.GitHub, r.ReviewEngine)
		}
		if r.OCRVersion == "" {
			return fmt.Errorf("repository %s ocr_version is required", r.GitHub)
		}
		if r.FixMarker == "" {
			r.FixMarker = "multica:fix"
		}
		if r.TrustMode == "" {
			r.TrustMode = "any"
		}
		if r.TrustMode != "any" {
			return fmt.Errorf("repository %s only trust_mode=any is supported in v1", r.GitHub)
		}
	}
	if c.StateDB == "" {
		return errors.New("state_db is required")
	}
	if !filepath.IsAbs(c.StateDB) {
		return errors.New("state_db must be an absolute path")
	}
	return nil
}

func validateProxy(p ProxyConfig) error {
	switch p.Type {
	case "none":
		if p.Required {
			return errors.New("github.proxy.required cannot be true when type=none")
		}
	case "standard_env":
		if p.Name != "" {
			return errors.New("github.proxy.name is only valid for type=url_env")
		}
	case "url_env":
		if !validEnvName(p.Name) {
			return errors.New("github.proxy.name is invalid")
		}
	default:
		return errors.New("github.proxy.type must be none, standard_env, or url_env")
	}
	return nil
}

func validEnvName(name string) bool {
	return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name)
}
