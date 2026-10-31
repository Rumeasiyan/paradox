package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/Rumeasiyan/paradox"
	"github.com/Rumeasiyan/paradox/internal/admin"
	"github.com/Rumeasiyan/paradox/internal/api"
	"github.com/Rumeasiyan/paradox/internal/config"
	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/health"
	reporting "github.com/Rumeasiyan/paradox/internal/report"
	"github.com/Rumeasiyan/paradox/internal/retention"
	"github.com/Rumeasiyan/paradox/internal/runner"
	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/internal/systems"
	"github.com/Rumeasiyan/paradox/internal/timetable"
	"github.com/Rumeasiyan/paradox/internal/wake"
	"github.com/Rumeasiyan/paradox/sign"
)

// tickEvery is how often the scheduler looks for due jobs.
const tickEvery = time.Second

// runServe runs PARADOX until SIGINT or SIGTERM.
func runServe(args []string, stderr io.Writer, getenv func(string) string) error {
	if len(args) > 0 {
		return errUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, stderr, getenv, nil)
}

// serve is runServe with a context and a hook that receives the bound
// address once the service is ready, so tests can drive it.
func serve(ctx context.Context, stderr io.Writer, getenv func(string) string, ready func(addr string)) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("settings, refusing to start:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	// The real waker, when any system asks to be woken. Its session lasts a
	// keep-awake interval plus a wake timeout, so a running system is never
	// stopped between two keep-awake calls.
	var waker wake.Waker = wake.NewFake() // never called: no system may use wake: sablier without a Sablier URL
	if cfg.SablierURL != "" {
		sab, err := wake.NewSablier(cfg.SablierURL, cfg.KeepAwakeEvery+cfg.WakeTimeout, nil)
		if err != nil {
			return fmt.Errorf("PARADOX_SABLIER_URL: %w", err)
		}
		waker = sab
	}
	// Push URLs carry tokens: errors from here never repeat them.
	reporter, err := reporting.NewPush(cfg.HeartbeatURL, cfg.FailureURL, nil, logger)
	if err != nil {
		return fmt.Errorf("monitor settings: %w", err)
	}

	key, err := sign.ParsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("PARADOX_PRIVATE_KEY: %w", err)
	}
	list, err := systems.Load(os.DirFS(cfg.SystemsDir), systems.Options{
		Defaults:          schedule.Defaults{Timeout: cfg.JobTimeout, Retries: cfg.JobRetries},
		SablierConfigured: cfg.SablierURL != "",
	})
	if err != nil {
		return fmt.Errorf("systems folder %s, refusing to start:\n%w", cfg.SystemsDir, err)
	}

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.MigrateRiver(ctx, pool); err != nil {
		return fmt.Errorf("river migrations: %w", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	if err := timetable.Sync(ctx, pool, list, cfg.Timezone, time.Now()); err != nil {
		return fmt.Errorf("timetable: %w", err)
	}

	deps := &runner.Deps{
		Pool:           pool,
		Waker:          waker,
		Key:            sign.Signer{KeyID: sign.ParadoxKeyID, Key: key, Direction: sign.ToSystem},
		HTTP:           &http.Client{},
		Logger:         logger,
		Location:       cfg.Timezone,
		WakeTimeout:    cfg.WakeTimeout,
		KeepAwakeEvery: cfg.KeepAwakeEvery,
		SystemPort:     cfg.SystemPort,
		RetryDelays:    cfg.RetryDelays,
		Reporter:       reporter,
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &runner.Worker{D: deps})
	rcfg := db.RiverConfig(workers, cfg.MaxRunning)
	rcfg.Logger = logger
	rc, err := river.NewClient(riverpgxv5.New(pool), rcfg)
	if err != nil {
		return err
	}
	if err := rc.Start(ctx); err != nil {
		return err
	}

	queueAPI := (&api.API{
		Pool:          pool,
		River:         rc,
		Keys:          systems.NewIndex(list).Keys,
		Window:        cfg.SignatureWindow,
		MaxDataBytes:  cfg.MaxDataBytes,
		MaxAhead:      cfg.MaxAhead,
		RequestIDKeep: cfg.RequestIDKeep,
		Logger:        logger,
	}).Handler()
	mux := http.NewServeMux()
	mux.Handle(health.Path, health.Handler(health.DBCheck(pool), logger))
	mux.Handle(api.Prefix, queueAPI)
	mux.Handle(api.Prefix+"/", queueAPI)
	// Timeouts so a slow or stalled client cannot hold connections open.
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		stopRiver(rc, cfg.ShutdownGrace, logger)
		return fmt.Errorf("PARADOX_LISTEN %s: %w", cfg.Listen, err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// The operator endpoint answers only on loopback (config refuses any
	// other address), with the service's own connections.
	adminLn, err := net.Listen("tcp", cfg.AdminListen)
	if err != nil {
		srv.Close()
		stopRiver(rc, cfg.ShutdownGrace, logger)
		return fmt.Errorf("PARADOX_ADMIN_LISTEN %s: %w", cfg.AdminListen, err)
	}
	adminSrv := &http.Server{Handler: (&admin.Admin{Pool: pool, River: rc, FailedDataKeep: cfg.FailedDataKeep, Logger: logger}).Handler(), ReadHeaderTimeout: 10 * time.Second}
	go adminSrv.Serve(adminLn)

	sched := &runner.Scheduler{D: deps, River: rc, CatchUp: cfg.CatchUpWindow}
	go sched.Run(ctx, tickEvery)
	go reporting.HeartbeatLoop(ctx, reporter, cfg.HeartbeatEvery, logger)
	go retention.Loop(ctx, pool, retention.DefaultEvery, cfg.RecordKeep, cfg.RequestIDKeep, cfg.FailedDataKeep, logger)

	logger.Info("paradox started", "version", paradox.Version, "systems", len(list), "listen", ln.Addr().String())
	if ready != nil {
		ready(ln.Addr().String())
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
		}
	}
	logger.Info("paradox stopping")
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shut)
	adminSrv.Shutdown(shut)
	stopRiver(rc, cfg.ShutdownGrace, logger)
	return nil
}

// stopRiver lets running jobs finish within grace, then records any
// attempt still running as interrupted so it is retried after the restart
// (PX-RUN-05).
func stopRiver(rc *river.Client[pgx.Tx], grace time.Duration, logger *slog.Logger) {
	if err := db.StopRiver(rc, grace); err != nil {
		logger.Warn("stopping the worker", "error", err)
	}
}
