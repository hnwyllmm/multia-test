package reviewer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
)

func TestTriggerPostsJSONWithoutUsingStandardProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	var body, idempotency string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		body = string(data)
		idempotency = request.Header.Get("Idempotency-Key")
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("TEST_REVIEW_WEBHOOK", server.URL+"/opaque-token")
	client, err := New(config.ReviewDispatchConfig{
		RequestTimeout: config.Duration{Duration: time.Second},
		Agents: []config.ReviewAgent{{
			ID: "reviewer", Webhook: config.SecretRef{Type: "env", Name: "TEST_REVIEW_WEBHOOK"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(); err != nil {
		t.Fatal(err)
	}
	if err := client.Trigger(context.Background(), "reviewer", "round-key", []byte(`{"event_type":"review"}`)); err != nil {
		t.Fatal(err)
	}
	if body != `{"event_type":"review"}` || idempotency != "round-key" {
		t.Fatalf("body=%q idempotency=%q", body, idempotency)
	}
}

func TestTriggerRedactsWebhookFromNetworkErrors(t *testing.T) {
	const webhook = "http://127.0.0.1:1/api/webhooks/autopilots/secret-token"
	t.Setenv("TEST_REVIEW_WEBHOOK", webhook)
	client, err := New(config.ReviewDispatchConfig{
		RequestTimeout: config.Duration{Duration: 100 * time.Millisecond},
		Agents: []config.ReviewAgent{{
			ID: "reviewer", Webhook: config.SecretRef{Type: "env", Name: "TEST_REVIEW_WEBHOOK"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Trigger(context.Background(), "reviewer", "round-key", []byte(`{}`))
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected a redacted network error, got %v", err)
	}
}
