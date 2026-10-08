package github

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
)

func TestHTTPProxyIsUsed(t *testing.T) {
	var requests atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"resources":{"core":{"remaining":1}}}`)
	}))
	defer proxyServer.Close()
	t.Setenv("TEST_GITHUB_PROXY", proxyServer.URL)
	t.Setenv("TEST_GITHUB_TOKEN", "token")
	client, err := New(config.GitHubConfig{
		APIBaseURL: "http://api.github.invalid",
		Auth:       config.SecretRef{Type: "env", Name: "TEST_GITHUB_TOKEN"},
		Proxy: config.ProxyConfig{
			Type: "url_env", Name: "TEST_GITHUB_PROXY", Required: true,
			ConnectTimeout: config.Duration{Duration: time.Second},
			RequestTimeout: config.Duration{Duration: time.Second},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("proxy received %d requests", requests.Load())
	}
}

func TestRequiredProxyMissing(t *testing.T) {
	t.Setenv("TEST_MISSING_PROXY", "")
	_, _, err := newTransport(config.ProxyConfig{Type: "url_env", Name: "TEST_MISSING_PROXY", Required: true})
	if err == nil {
		t.Fatal("expected missing proxy error")
	}
}

func TestHTTPConnectProxyIsUsed(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"resources":{"core":{"remaining":1}}}`)
	}))
	defer upstream.Close()

	var connects atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(writer, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		target, err := net.DialTimeout("tcp", request.Host, time.Second)
		if err != nil {
			http.Error(writer, "dial failed", http.StatusBadGateway)
			return
		}
		client, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			return
		}
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			defer client.Close()
			defer target.Close()
			done := make(chan struct{}, 1)
			go func() {
				_, _ = io.Copy(target, client)
				done <- struct{}{}
			}()
			_, _ = io.Copy(client, target)
			<-done
		}()
	}))
	defer proxyServer.Close()
	t.Setenv("TEST_CONNECT_PROXY", proxyServer.URL)

	transport, _, err := newTransport(config.ProxyConfig{
		Type: "url_env", Name: "TEST_CONNECT_PROXY", Required: true,
		ConnectTimeout: config.Duration{Duration: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test server certificate
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Get(upstream.URL + "/rate_limit")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if connects.Load() != 1 {
		t.Fatalf("CONNECT requests=%d", connects.Load())
	}
}

func TestProxyRedaction(t *testing.T) {
	got := RedactProxyURL("http://user:password@proxy.example:8080/path?q=secret")
	if got != "http://proxy.example:8080" {
		t.Fatalf("unexpected redacted URL %q", got)
	}
}

func TestAuthenticatedProxyFailureIsRedacted(t *testing.T) {
	const proxyValue = "http://alice:very-secret-password@127.0.0.1:1"
	t.Setenv("TEST_AUTH_PROXY", proxyValue)
	t.Setenv("TEST_GITHUB_TOKEN", "token")
	client, err := New(config.GitHubConfig{
		APIBaseURL: "https://api.github.invalid",
		Auth:       config.SecretRef{Type: "env", Name: "TEST_GITHUB_TOKEN"},
		Proxy: config.ProxyConfig{
			Type: "url_env", Name: "TEST_AUTH_PROXY", Required: true,
			ConnectTimeout: config.Duration{Duration: 50 * time.Millisecond},
			RequestTimeout: config.Duration{Duration: 200 * time.Millisecond},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Check(t.Context())
	if err == nil {
		t.Fatal("expected unavailable proxy failure")
	}
	for _, secret := range []string{proxyValue, "alice", "very-secret-password"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("proxy credential leaked in error: %v", err)
		}
	}
	if strings.Contains(client.ProxyLabel(), "alice") || strings.Contains(client.ProxyLabel(), "very-secret-password") {
		t.Fatalf("proxy credential leaked in label %q", client.ProxyLabel())
	}
}

func TestSOCKSProxyConfiguration(t *testing.T) {
	t.Setenv("TEST_SOCKS_PROXY", "socks5h://user:password@127.0.0.1:1080")
	_, label, err := newTransport(config.ProxyConfig{Type: "url_env", Name: "TEST_SOCKS_PROXY", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if label != "socks5h://127.0.0.1:1080" {
		t.Fatalf("unexpected label %q", label)
	}
}

func TestSOCKS5AndSOCKS5HProxyRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, "ok")
	}))
	defer upstream.Close()
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	targetURL := "http://localhost:" + port

	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			proxyAddress, requests, closeProxy := startSOCKS5Proxy(t, "proxy-user", "proxy-password")
			defer closeProxy()
			t.Setenv("TEST_SOCKS_PROXY", scheme+"://proxy-user:proxy-password@"+proxyAddress)
			transport, label, err := newTransport(config.ProxyConfig{
				Type: "url_env", Name: "TEST_SOCKS_PROXY", Required: true,
				ConnectTimeout: config.Duration{Duration: time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			if strings.Contains(label, "proxy-user") || strings.Contains(label, "proxy-password") {
				t.Fatalf("credentials leaked in label %q", label)
			}
			response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Get(targetURL)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if string(body) != "ok" || requests.Load() != 1 {
				t.Fatalf("body=%q proxy requests=%d", body, requests.Load())
			}
		})
	}
}

func startSOCKS5Proxy(t *testing.T, username, password string) (string, *atomic.Int32, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5Connection(connection, username, password, &requests)
		}
	}()
	return listener.Addr().String(), &requests, func() { _ = listener.Close() }
}

func serveSOCKS5Connection(connection net.Conn, username, password string, requests *atomic.Int32) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	if username != "" {
		_, _ = connection.Write([]byte{5, 2})
		if _, err := io.ReadFull(reader, header); err != nil || header[0] != 1 {
			return
		}
		user := make([]byte, int(header[1]))
		if _, err := io.ReadFull(reader, user); err != nil {
			return
		}
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		pass := make([]byte, int(length))
		if _, err := io.ReadFull(reader, pass); err != nil || string(user) != username || string(pass) != password {
			_, _ = connection.Write([]byte{1, 1})
			return
		}
		_, _ = connection.Write([]byte{1, 0})
	} else {
		_, _ = connection.Write([]byte{5, 0})
	}
	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(reader, requestHeader); err != nil || requestHeader[0] != 5 || requestHeader[1] != 1 {
		return
	}
	var host string
	switch requestHeader[3] {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		address := make([]byte, int(length))
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = string(address)
	case 4:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes)))), time.Second)
	if err != nil {
		_, _ = connection.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	requests.Add(1)
	_, _ = connection.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(target, reader)
		done <- struct{}{}
	}()
	_, _ = io.Copy(connection, target)
	<-done
}
