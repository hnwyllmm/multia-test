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
	"github.com/hnwyllmm/multia-test/internal/dashboard"
	"github.com/hnwyllmm/multia-test/internal/dispatcher"
	gh "github.com/hnwyllmm/multia-test/internal/github"
	"github.com/hnwyllmm/multia-test/internal/lock"
	"github.com/hnwyllmm/multia-test/internal/multica"
	"github.com/hnwyllmm/multia-test/internal/reviewer"
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
		return errors.New("usage: multica-github-dispatcher <run|once|dashboard|status|version> [flags]")
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
	if command != "run" && command != "once" && command != "dashboard" && command != "status" {
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
	if command == "dashboard" {
		return serveDashboard(cfg)
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
	reviewerClient, err := reviewer.New(cfg.ReviewDispatch)
	if err != nil {
		return err
	}
	// GitHub and review-webhook configuration are required for the independent
	// review lane. Multica is optional context and feedback routing.
	if err := githubClient.Check(ctx); err != nil {
		return err
	}
	if err := reviewerClient.Check(); err != nil {
		return err
	}
	var multicaClient *multica.Client
	if candidate, createErr := multica.New(cfg.Multica); createErr != nil {
		logger.Warn("optional Multica client unavailable", "error", createErr)
	} else if checkErr := candidate.Check(ctx); checkErr != nil {
		logger.Warn("optional Multica workspace unavailable", "error", checkErr)
	} else {
		multicaClient = candidate
	}
	logger.Info("dependencies ready", "github_proxy", githubClient.ProxyLabel(),
		"reviewers", len(cfg.ReviewDispatch.Agents), "multica_available", multicaClient != nil)

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

	worker := dispatcher.New(cfg, githubClient, multicaClient, reviewerClient, store, logger)

	if command == "once" {
		return pollOnce(ctx, store, worker, dryRun)
	}
	for {
		started := time.Now()
		if err := pollOnce(ctx, store, worker, false); err != nil {
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

func pollOnce(ctx context.Context, store *state.Store, worker *dispatcher.Dispatcher, dryRun bool) error {
	runID, err := store.BeginPoll(ctx)
	if err != nil {
		return fmt.Errorf("record poll start: %w", err)
	}
	runErr := worker.Once(ctx, dryRun)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	finishErr := store.FinishPoll(finishCtx, runID, runErr)
	if finishErr != nil {
		finishErr = fmt.Errorf("record poll completion: %w", finishErr)
	}
	return errors.Join(runErr, finishErr)
}

func serveDashboard(cfg *config.Config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := state.Open(cfg.StateDB)
	if err != nil {
		return err
	}
	defer store.Close()
	return dashboard.New(store, cfg, version, logger).Serve(ctx)
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
