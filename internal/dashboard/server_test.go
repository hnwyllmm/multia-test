package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	"github.com/hnwyllmm/multia-test/internal/state"
)

func TestDashboardAPIAndStaticPage(t *testing.T) {
	store, err := state.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		Dashboard: config.DashboardConfig{
			Listen: "127.0.0.1:8787", RefreshInterval: config.Duration{Duration: 10 * time.Second},
		},
		Multica: config.MulticaConfig{WorkspaceID: "workspace", Prefix: "WANG"},
	}
	server := New(store, cfg, "test-version", slog.New(slog.NewTextHandler(io.Discard, nil)))

	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Review Dispatcher") {
		t.Fatalf("page status=%d body=%q", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("missing CSP: %q", page.Header().Get("Content-Security-Policy"))
	}
	if page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected cache policy: %q", page.Header().Get("Cache-Control"))
	}

	api := httptest.NewRecorder()
	server.Handler().ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil))
	if api.Code != http.StatusOK {
		t.Fatalf("API status=%d body=%q", api.Code, api.Body.String())
	}
	var payload Payload
	if err := json.Unmarshal(api.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != "test-version" || payload.WorkspaceID != "workspace" ||
		payload.RefreshIntervalSeconds != 10 || payload.PollIntervalSeconds != 60 {
		t.Fatalf("unexpected payload %+v", payload)
	}
}

func TestUnsupportedMethodIsRejected(t *testing.T) {
	store, err := state.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		Dashboard: config.DashboardConfig{Listen: "127.0.0.1:8787"},
		Multica:   config.MulticaConfig{WorkspaceID: "workspace", Prefix: "WANG"},
	}
	server := New(store, cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/dashboard", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", response.Code)
	}
}
