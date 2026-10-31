// Package config reads PARADOX's settings from the environment.
//
// Every setting has a generic default except the database URL's password and
// the private key, which no default can supply. Site values (real paths,
// URLs, timezones) come from the deployment, never from this code
// (PX-NFR-07).
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds every PARADOX setting. Field comments name the variable.
type Config struct {
	SystemsDir   string         // PARADOX_SYSTEMS_DIR
	Timezone     *time.Location // PARADOX_TIMEZONE
	Listen       string         // PARADOX_LISTEN
	AdminListen  string         // PARADOX_ADMIN_LISTEN; loopback only
	DatabaseURL  string         // PARADOX_DATABASE_URL
	PrivateKey   string         // PARADOX_PRIVATE_KEY (format checked by the signing helper)
	SystemPort   int            // PARADOX_SYSTEM_PORT
	SablierURL   string         // PARADOX_SABLIER_URL; empty means no system may use wake: sablier
	HeartbeatURL string         // PARADOX_HEARTBEAT_URL; empty means heartbeats go to the log
	FailureURL   string         // PARADOX_FAILURE_URL; empty means failure reports go to the log

	HeartbeatEvery  time.Duration   // PARADOX_HEARTBEAT_EVERY (PX-REP-03)
	SignatureWindow time.Duration   // PARADOX_SIGNATURE_WINDOW (PX-SIG-03)
	WakeTimeout     time.Duration   // PARADOX_WAKE_TIMEOUT (PX-RUN-01)
	KeepAwakeEvery  time.Duration   // PARADOX_KEEP_AWAKE_EVERY (PX-RUN-02)
	MaxRunning      int             // PARADOX_MAX_RUNNING (PX-RUN-04)
	ShutdownGrace   time.Duration   // PARADOX_SHUTDOWN_GRACE (PX-RUN-05)
	RetryDelays     []time.Duration // PARADOX_RETRY_DELAYS (PX-REP-01)
	JobTimeout      time.Duration   // PARADOX_JOB_TIMEOUT (PX-SCH-02)
	JobRetries      int             // PARADOX_JOB_RETRIES (PX-SCH-02)
	CatchUpWindow   time.Duration   // PARADOX_CATCH_UP (PX-SCH-05)
	MaxDataBytes    int             // PARADOX_MAX_DATA_BYTES (PX-JOB-01)
	MaxAhead        time.Duration   // PARADOX_MAX_AHEAD (PX-JOB-02)
	RequestIDKeep   time.Duration   // PARADOX_REQUEST_ID_KEEP (PX-JOB-03)
	RecordKeep      time.Duration   // PARADOX_RECORD_KEEP (PX-REC-02)
	FailedDataKeep  time.Duration   // PARADOX_FAILED_DATA_KEEP (PX-REC-02, PX-REC-04)
}

// Defaults are the values used when a variable is unset or empty.
var Defaults = map[string]string{
	"PARADOX_SYSTEMS_DIR":      "./systems",
	"PARADOX_TIMEZONE":         "UTC",
	"PARADOX_LISTEN":           ":8080",
	"PARADOX_ADMIN_LISTEN":     "127.0.0.1:8081",
	"PARADOX_DATABASE_URL":     "postgres://paradox@localhost:5432/paradox",
	"PARADOX_PRIVATE_KEY":      "",
	"PARADOX_SYSTEM_PORT":      "8080",
	"PARADOX_SABLIER_URL":      "",
	"PARADOX_HEARTBEAT_URL":    "",
	"PARADOX_FAILURE_URL":      "",
	"PARADOX_HEARTBEAT_EVERY":  "60s",
	"PARADOX_SIGNATURE_WINDOW": "5m",
	"PARADOX_WAKE_TIMEOUT":     "2m",
	"PARADOX_KEEP_AWAKE_EVERY": "60s",
	"PARADOX_MAX_RUNNING":      "10",
	"PARADOX_SHUTDOWN_GRACE":   "30s",
	"PARADOX_RETRY_DELAYS":     "1m,5m,15m",
	"PARADOX_JOB_TIMEOUT":      "10m",
	"PARADOX_JOB_RETRIES":      "3",
	"PARADOX_CATCH_UP":         "1h",
	"PARADOX_MAX_DATA_BYTES":   "65536",
	"PARADOX_MAX_AHEAD":        "30d",
	"PARADOX_REQUEST_ID_KEEP":  "24h",
	"PARADOX_RECORD_KEEP":      "90d",
	"PARADOX_FAILED_DATA_KEEP": "7d",
}

// Load reads every setting through getenv (os.Getenv in production) and
// returns all problems at once, each naming its variable, so a bad
// deployment is fixed in one pass rather than one restart per mistake.
func Load(getenv func(string) string) (Config, error) {
	r := reader{getenv: getenv}
	c := Config{
		SystemsDir:   r.str("PARADOX_SYSTEMS_DIR"),
		Listen:       r.str("PARADOX_LISTEN"),
		AdminListen:  r.loopback("PARADOX_ADMIN_LISTEN"),
		DatabaseURL:  r.url("PARADOX_DATABASE_URL", "postgres", "postgresql"),
		PrivateKey:   r.str("PARADOX_PRIVATE_KEY"),
		SystemPort:   r.intIn("PARADOX_SYSTEM_PORT", 1, 65535),
		SablierURL:   r.url("PARADOX_SABLIER_URL", "http", "https"),
		HeartbeatURL: r.url("PARADOX_HEARTBEAT_URL", "http", "https"),
		FailureURL:   r.url("PARADOX_FAILURE_URL", "http", "https"),

		HeartbeatEvery:  r.dur("PARADOX_HEARTBEAT_EVERY"),
		SignatureWindow: r.dur("PARADOX_SIGNATURE_WINDOW"),
		WakeTimeout:     r.dur("PARADOX_WAKE_TIMEOUT"),
		KeepAwakeEvery:  r.dur("PARADOX_KEEP_AWAKE_EVERY"),
		MaxRunning:      r.intIn("PARADOX_MAX_RUNNING", 1, 1000),
		ShutdownGrace:   r.dur("PARADOX_SHUTDOWN_GRACE"),
		RetryDelays:     r.durList("PARADOX_RETRY_DELAYS"),
		JobTimeout:      r.dur("PARADOX_JOB_TIMEOUT"),
		JobRetries:      r.intIn("PARADOX_JOB_RETRIES", 0, 100),
		CatchUpWindow:   r.dur("PARADOX_CATCH_UP"),
		MaxDataBytes:    r.intIn("PARADOX_MAX_DATA_BYTES", 1, 16<<20),
		MaxAhead:        r.dur("PARADOX_MAX_AHEAD"),
		RequestIDKeep:   r.dur("PARADOX_REQUEST_ID_KEEP"),
		RecordKeep:      r.dur("PARADOX_RECORD_KEEP"),
		FailedDataKeep:  r.dur("PARADOX_FAILED_DATA_KEEP"),
	}
	c.Timezone = r.zone("PARADOX_TIMEZONE")
	if c.PrivateKey == "" {
		r.fail("PARADOX_PRIVATE_KEY", "is required (create one with the key generator)")
	}
	if c.KeepAwakeEvery >= c.WakeTimeout && c.WakeTimeout > 0 {
		// A keep-awake slower than the wake limit lets Sablier stop a system mid-job.
		r.fail("PARADOX_KEEP_AWAKE_EVERY", "must be shorter than PARADOX_WAKE_TIMEOUT")
	}
	return c, errors.Join(r.errs...)
}

type reader struct {
	getenv func(string) string
	errs   []error
}

func (r *reader) raw(name string) string {
	if v := strings.TrimSpace(r.getenv(name)); v != "" {
		return v
	}
	return Defaults[name]
}

func (r *reader) fail(name, msg string) {
	r.errs = append(r.errs, fmt.Errorf("%s %s", name, msg))
}

func (r *reader) str(name string) string { return r.raw(name) }

// loopback accepts only a loopback host:port: the admin endpoint must never
// be reachable from outside the container (PX-NFR-05).
func (r *reader) loopback(name string) string {
	v := r.raw(name)
	host, port, err := net.SplitHostPort(v)
	n, perr := strconv.Atoi(port)
	if err != nil || perr != nil || n < 1 || n > 65535 {
		r.fail(name, fmt.Sprintf("must be host:port, got %q", v))
		return v
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		r.fail(name, fmt.Sprintf("must be a loopback address such as 127.0.0.1:8081, got %q; the admin endpoint is for the operator inside the container only", v))
	}
	return v
}

func (r *reader) intIn(name string, lo, hi int) int {
	v := r.raw(name)
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		r.fail(name, fmt.Sprintf("must be a whole number from %d to %d, got %q", lo, hi, v))
	}
	return n
}

func (r *reader) dur(name string) time.Duration {
	v := r.raw(name)
	d, err := parseDuration(v)
	if err != nil || d <= 0 {
		r.fail(name, fmt.Sprintf("must be a positive duration such as 90s, 5m, 2h or 30d, got %q", v))
	}
	return d
}

func (r *reader) durList(name string) []time.Duration {
	v := r.raw(name)
	var out []time.Duration
	for _, part := range strings.Split(v, ",") {
		d, err := parseDuration(strings.TrimSpace(part))
		if err != nil || d <= 0 {
			r.fail(name, fmt.Sprintf("must be a comma-separated list of positive durations, got %q", v))
			return nil
		}
		out = append(out, d)
	}
	return out
}

func (r *reader) zone(name string) *time.Location {
	v := r.raw(name)
	loc, err := time.LoadLocation(v)
	if err != nil {
		r.fail(name, fmt.Sprintf("must be an IANA timezone such as UTC or Europe/London, got %q", v))
		return time.UTC
	}
	return loc
}

func (r *reader) url(name string, schemes ...string) string {
	v := r.raw(name)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		r.fail(name, "must be an absolute URL")
		return v
	}
	for _, s := range schemes {
		if u.Scheme == s {
			return v
		}
	}
	r.fail(name, fmt.Sprintf("must use %s", strings.Join(schemes, " or ")))
	return v
}

// parseDuration is time.ParseDuration plus a whole-day suffix ("30d"),
// because retention settings are naturally written in days.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
