package main

import (
	"context"
	"strings"

	"github.com/Rumeasiyan/paradox/internal/config"
	"github.com/Rumeasiyan/paradox/internal/health"
)

// runHealthcheck answers the container health check (PX-REP-04): it probes
// the running service on the loopback address, and the service answers
// healthy only when it can reach its database. Only PARADOX_LISTEN is read,
// so the check works without the private key or the database URL.
func runHealthcheck(getenv func(string) string) error {
	listen := strings.TrimSpace(getenv("PARADOX_LISTEN"))
	if listen == "" {
		listen = config.Defaults["PARADOX_LISTEN"]
	}
	url, err := health.LoopbackURL(listen)
	if err != nil {
		return err
	}
	return health.Probe(context.Background(), url)
}
