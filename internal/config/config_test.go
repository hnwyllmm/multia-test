package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretRefEnvironment(t *testing.T) {
	t.Setenv("TEST_DISPATCHER_SECRET", "secret-value")
	ref := SecretRef{Type: "env", Name: "TEST_DISPATCHER_SECRET"}
	if err := ref.Validate("secret"); err != nil {
		t.Fatal(err)
	}
	value, err := ref.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if value != "secret-value" {
		t.Fatalf("unexpected secret %q", value)
	}
}

func TestSecretRefEnvironmentMissingOrEmpty(t *testing.T) {
	const name = "TEST_DISPATCHER_MISSING_SECRET"
	t.Setenv(name, "")
	if _, err := (SecretRef{Type: "env", Name: name}).Resolve(); err == nil {
		t.Fatal("expected an empty environment secret to fail")
	}
}

func TestSecretRefFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := SecretRef{Type: "file", Path: path}
	value, err := ref.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if value != "token" {
		t.Fatalf("unexpected token %q", value)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Resolve(); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("expected permission error, got %v", err)
	}
}

func TestSecretRefFileRotationAndEmptyFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := SecretRef{Type: "file", Path: path}
	first, err := ref.Resolve()
	if err != nil || first != "first" {
		t.Fatalf("first resolve=%q err=%v", first, err)
	}
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	second, err := ref.Resolve()
	if err != nil || second != "second" {
		t.Fatalf("rotated resolve=%q err=%v", second, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Resolve(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty file error, got %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `poll_interval: 1m
bootstrap_lookback: 24h
unknown: true
github:
  auth: {type: env, name: GITHUB_TOKEN}
  proxy: {type: none}
multica:
  server_url_env: MULTICA_SERVER_URL
  workspace_id: workspace
  workspace_prefix: WANG
  auth: {type: env, name: MULTICA_TOKEN}
repositories:
  - github: owner/repo
    reviewer_squad_id: squad
    reviewer_count: 1
    ocr_version: 1.12.8
state_db: /tmp/state.db
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}
