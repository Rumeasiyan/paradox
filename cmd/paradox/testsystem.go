package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Rumeasiyan/paradox/internal/testsystem"
)

// runTestSystem serves the test system that answers /jobs/<name> until
// SIGINT or SIGTERM (backlog story 15). Its settings are described in the
// testsystem package.
func runTestSystem(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveTestSystem(ctx, args, stderr, getenv, nil)
}

// serveTestSystem is runTestSystem with the context given, so tests can stop
// it. ready, if set, receives the listening address once calls are answered.
func serveTestSystem(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string, ready chan<- string) error {
	if len(args) > 0 {
		return errUsage
	}
	cfg, err := testsystem.LoadConfig(getenv)
	if err != nil {
		return fmt.Errorf("testsystem settings, refusing to start:\n%w", err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("testsystem: %w", err)
	}
	if ready != nil {
		ready <- ln.Addr().String()
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	return testsystem.Serve(ctx, ln, cfg, logger)
}
