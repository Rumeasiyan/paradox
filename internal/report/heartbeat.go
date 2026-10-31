package report

import (
	"context"
	"log/slog"
	"time"
)

// DefaultHeartbeatEvery is the heartbeat interval used when HeartbeatLoop is
// given a zero or negative one (PX-REP-03).
const DefaultHeartbeatEvery = 60 * time.Second

// HeartbeatLoop sends one heartbeat at once, then one every interval, until
// ctx ends (PX-REP-03). A failed heartbeat is logged at Warn and the loop
// carries on: a missing heartbeat is the monitor's signal, so PARADOX must not
// stop sending because one push failed. Each heartbeat gets at most one
// interval to finish, so a hung push cannot hold up the next. A nil logger
// means slog.Default().
func HeartbeatLoop(ctx context.Context, r Reporter, every time.Duration, logger *slog.Logger) {
	if every <= 0 {
		every = DefaultHeartbeatEvery
	}
	if logger == nil {
		logger = slog.Default()
	}
	if ctx.Err() != nil {
		return
	}
	beat := func() {
		hctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		if err := r.Heartbeat(hctx); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "heartbeat failed", slog.String("error", err.Error()))
		}
	}
	beat()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
		}
	}
}
