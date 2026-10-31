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
	"github.com/Rumeasiyan/paradox/internal/api"
	"github.com/Rumeasiyan/paradox/internal/config"
	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/health"
	reporting "github.com/Rumeasiyan/paradox/internal/report"
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

	// Adapters for the real waker and monitor arrive in backlog stories 34
	// and 35. Until then a setting that asks for them stops the start,
	// rather than pretending systems are awake or dropping alerts.
	var missing []error
	if cfg.SablierURL != "" {
		missing = append(missing, errors.New("PARADOX_SABLIER_URL is set, but the Sablier adapter is not built yet (backlog story 34); leave it empty and use wake: none"))
	}
	if cfg.HeartbeatURL != "" || cfg.FailureURL != "" {
		missing = append(missing, errors.New("PARADOX_HEARTBEAT_URL or PARADOX_FAILURE_URL is set, but the push adapter is not built yet (backlog story 35); leave them empty to report to the log"))
	}
	if len(missing) > 0 {
		return errors.Join(missing...)
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

	reporter := reporting.Log{Logger: logger}
	deps := &runner.Deps{
		Pool:           pool,
		Waker:          wake.NewFake(), // only reached by wake: sablier, which needs story 34 first
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
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		stopRiver(rc, cfg.ShutdownGrace)
		return fmt.Errorf("PARADOX_LISTEN %s: %w", cfg.Listen, err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sched := &runner.Scheduler{D: deps, River: rc}
	go sched.Run(ctx, tickEvery)
	go reporting.HeartbeatLoop(ctx, reporter, cfg.HeartbeatEvery, logger)

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
	stopRiver(rc, cfg.ShutdownGrace)
	return nil
}

// stopRiver lets running jobs finish within grace (PX-RUN-05).
func stopRiver(rc *river.Client[pgx.Tx], grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	rc.Stop(ctx)
}
