package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hnwyllmm/multia-test/internal/config"
	"github.com/hnwyllmm/multia-test/internal/dispatcher"
	gh "github.com/hnwyllmm/multia-test/internal/github"
	"github.com/hnwyllmm/multia-test/internal/lock"
	"github.com/hnwyllmm/multia-test/internal/multica"
	"github.com/hnwyllmm/multia-test/internal/state"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: multica-github-dispatcher <run|once|status|version> [flags]")
	}
	command := arguments[0]
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "", "path to YAML configuration")
	dryRun := flags.Bool("dry-run", false, "discover and report actions without persistent writes")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if command != "run" && command != "once" && command != "status" {
		return fmt.Errorf("unknown command %q", command)
	}
	if *configPath == "" {
		return errors.New("--config is required")
	}
	if command != "once" && *dryRun {
		return errors.New("--dry-run is only valid with once")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if command == "status" {
		return printStatus(cfg)
	}
	return execute(command, *dryRun, cfg)
}

func execute(command string, dryRun bool, cfg *config.Config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	githubClient, err := gh.New(cfg.GitHub)
	if err != nil {
		return err
	}
	multicaClient, err := multica.New(cfg.Multica)
	if err != nil {
		return err
	}
	// Validate both credentials and both network paths before touching the state
	// database. In particular, a bad GitHub proxy must never advance a cursor.
	if err := githubClient.Check(ctx); err != nil {
		return err
	}
	if err := multicaClient.Check(ctx); err != nil {
		return err
	}
	logger.Info("dependencies ready", "github_proxy", githubClient.ProxyLabel(), "workspace_id", cfg.Multica.WorkspaceID)

	if !dryRun {
		if err := os.MkdirAll(filepath.Dir(cfg.StateDB), 0o700); err != nil {
			return fmt.Errorf("create state directory: %w", err)
		}
		processLock, err := lock.Acquire(cfg.StateDB + ".lock")
		if err != nil {
			return err
		}
		defer processLock.Close()
	}

	var store *state.Store
	if dryRun {
		store, err = state.OpenMemory()
	} else {
		store, err = state.Open(cfg.StateDB)
	}
	if err != nil {
		return err
	}
	defer store.Close()

	worker := dispatcher.New(cfg, githubClient, multicaClient, store, logger)

	if command == "once" {
		return worker.Once(ctx, dryRun)
	}
	for {
		started := time.Now()
		if err := worker.Once(ctx, false); err != nil {
			logger.Error("poll cycle completed with errors", "error", err, "duration", time.Since(started).String())
		} else {
			logger.Info("poll cycle complete", "duration", time.Since(started).String())
		}
		timer := time.NewTimer(cfg.PollInterval.Duration)
		select {
		case <-ctx.Done():
			timer.Stop()
			logger.Info("dispatcher stopping")
			return nil
		case <-timer.C:
		}
	}
}

func printStatus(cfg *config.Config) error {
	store, err := state.Open(cfg.StateDB)
	if err != nil {
		return err
	}
	defer store.Close()
	status, err := store.Status(context.Background())
	if err != nil {
		return err
	}
	output := struct {
		Version     string       `json:"version"`
		WorkspaceID string       `json:"workspace_id"`
		StateDB     string       `json:"state_db"`
		Status      state.Status `json:"status"`
	}{version, cfg.Multica.WorkspaceID, cfg.StateDB, status}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
