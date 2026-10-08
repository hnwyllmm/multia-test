package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	"github.com/hnwyllmm/multia-test/internal/state"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	store   *state.Store
	cfg     *config.Config
	version string
	logger  *slog.Logger
}

type Payload struct {
	Version                string              `json:"version"`
	WorkspaceID            string              `json:"workspace_id"`
	WorkspacePrefix        string              `json:"workspace_prefix"`
	RefreshIntervalSeconds int                 `json:"refresh_interval_seconds"`
	PollIntervalSeconds    int                 `json:"poll_interval_seconds"`
	Data                   state.DashboardData `json:"data"`
}

func New(store *state.Store, cfg *config.Config, version string, logger *slog.Logger) *Server {
	return &Server{store: store, cfg: cfg, version: version, logger: logger}
}

func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/dashboard", s.dashboard)
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return securityHeaders(mux)
}

func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Dashboard.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("dashboard shutdown failed", "error", err)
		}
	}()
	s.logger.Info("dashboard listening", "address", listener.Addr().String())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func (s *Server) dashboard(writer http.ResponseWriter, request *http.Request) {
	data, err := s.store.Dashboard(request.Context(), 100)
	if err != nil {
		s.logger.Error("dashboard query failed", "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "dashboard data is unavailable"})
		return
	}
	payload := Payload{
		Version: s.version, WorkspaceID: s.cfg.Multica.WorkspaceID,
		WorkspacePrefix:        s.cfg.Multica.Prefix,
		RefreshIntervalSeconds: int(s.cfg.Dashboard.RefreshInterval.Duration.Seconds()),
		PollIntervalSeconds:    int(s.cfg.PollInterval.Duration.Seconds()),
		Data:                   data,
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, payload)
}

func (s *Server) health(writer http.ResponseWriter, request *http.Request) {
	if _, err := s.store.Status(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "error"})
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(writer, request)
	})
}
