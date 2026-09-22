package multica

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hnwyllmm/multia-test/internal/config"
)

func TestSanitizedEnvironmentRemovesProxyAndInjectsMultica(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://secret-proxy")
	t.Setenv("http_proxy", "http://secret-proxy")
	t.Setenv("UNCHANGED", "yes")
	environment := sanitizedEnvironment("token", "https://multica.example", "workspace")
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{"HTTPS_PROXY=", "http_proxy=", "secret-proxy"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment contains %q", forbidden)
		}
	}
	for _, expected := range []string{
		"UNCHANGED=yes", "MULTICA_TOKEN=token",
		"MULTICA_SERVER_URL=https://multica.example", "MULTICA_WORKSPACE_ID=workspace",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("environment missing %q", expected)
		}
	}
}

func TestClientUsesEnvironmentTokenAndRedactsFailure(t *testing.T) {
	cli := fakeCLI(t)
	t.Setenv("TEST_MULTICA_SERVER", "https://multica.example")
	t.Setenv("TEST_MULTICA_TOKEN", "invalid-super-secret")
	t.Setenv("EXPECTED_MULTICA_TOKEN", "valid")
	client, err := New(config.MulticaConfig{
		CLIPath: cli, ServerURLEnv: "TEST_MULTICA_SERVER", WorkspaceID: "workspace",
		Auth: config.SecretRef{Type: "env", Name: "TEST_MULTICA_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Check(context.Background())
	if err == nil {
		t.Fatal("expected invalid token failure")
	}
	if strings.Contains(err.Error(), "invalid-super-secret") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestClientReloadsRotatedTokenFileAndStripsProxy(t *testing.T) {
	cli := fakeCLI(t)
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "multica-token")
	if err := os.WriteFile(tokenPath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MULTICA_SERVER", "https://multica.example")
	t.Setenv("EXPECTED_MULTICA_TOKEN", "first")
	t.Setenv("HTTPS_PROXY", "http://user:password@github-proxy.example:8080")
	t.Setenv("ALL_PROXY", "socks5://github-proxy.example:1080")
	client, err := New(config.MulticaConfig{
		CLIPath: cli, ServerURLEnv: "TEST_MULTICA_SERVER", WorkspaceID: "workspace",
		Auth: config.SecretRef{Type: "file", Path: tokenPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, tokenPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXPECTED_MULTICA_TOKEN", "second")
	if err := client.Check(context.Background()); err != nil {
		t.Fatalf("check after atomic token rotation: %v", err)
	}
}

func fakeCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "multica")
	script := `#!/bin/sh
if [ -n "${HTTP_PROXY+x}" ] || [ -n "${HTTPS_PROXY+x}" ] || [ -n "${ALL_PROXY+x}" ] || [ -n "${NO_PROXY+x}" ]; then
  echo "proxy leaked to multica child" >&2
  exit 41
fi
if [ "$MULTICA_TOKEN" != "$EXPECTED_MULTICA_TOKEN" ]; then
  echo "invalid token: $MULTICA_TOKEN" >&2
  exit 42
fi
if [ "$MULTICA_SERVER_URL" != "https://multica.example" ] || [ "$MULTICA_WORKSPACE_ID" != "workspace" ]; then
  echo "wrong multica environment" >&2
  exit 43
fi
printf '{"id":"workspace"}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
