// Package testsystem is a stand-in system that answers PARADOX's job calls
// (contract section 5; how-it-works Flow A steps 8 and 9). A deployment runs
// it with `paradox testsystem` as a sleeping test target, to prove wake, run,
// retry, report and the system-side signature check. PARADOX's own tests run
// its handler with httptest.
//
// Settings, read from the environment:
//
//	TESTSYSTEM_LISTEN               Address to listen on. Default ":8080".
//	TESTSYSTEM_PARADOX_PUBLIC_KEY   Required. PARADOX's public key in the
//	                                contract's one-line form
//	                                (ed25519-pub:<base64>). Two keys separated
//	                                by a comma are allowed while PARADOX's key
//	                                is being changed.
//	TESTSYSTEM_FAIL_JOBS            Optional. Comma-separated job names that
//	                                always answer 500.
//	TESTSYSTEM_SLOW_JOBS            Optional. Comma-separated name=duration
//	                                pairs, for example slow=30s. Those jobs
//	                                wait that long, then answer 200 (or 500 if
//	                                the job is also in TESTSYSTEM_FAIL_JOBS).
//
// Invalid settings are all reported at once and the test system refuses to
// start.
//
// Routes:
//
//	POST /jobs/{name}   Read the body (at most 1 MB, 413 above), check the
//	                    signature with sign.VerifyFromParadox (401 on failure),
//	                    decode jobcall.Call (400 if invalid or if its job
//	                    differs from the path), then fail, wait or succeed as
//	                    configured. A run_id that already succeeded answers 200
//	                    again without redoing the work, because delivery is at
//	                    least once.
//	GET /healthz        200, for the container's own health check.
//
// Other paths answer 404 and wrong methods 405. Each call is logged with its
// job, run_id and attempt; the call's data is never logged (PX-NFR-06).
package testsystem

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Rumeasiyan/paradox/sign"
)

// Setting names.
const (
	EnvListen    = "TESTSYSTEM_LISTEN"
	EnvPublicKey = "TESTSYSTEM_PARADOX_PUBLIC_KEY"
	EnvFailJobs  = "TESTSYSTEM_FAIL_JOBS"
	EnvSlowJobs  = "TESTSYSTEM_SLOW_JOBS"
)

// DefaultListen is used when TESTSYSTEM_LISTEN is unset or empty.
const DefaultListen = ":8080"

// Config holds the test system's settings.
type Config struct {
	Listen      string                   // TESTSYSTEM_LISTEN
	ParadoxKeys []ed25519.PublicKey      // TESTSYSTEM_PARADOX_PUBLIC_KEY, one or two
	FailJobs    map[string]bool          // TESTSYSTEM_FAIL_JOBS
	SlowJobs    map[string]time.Duration // TESTSYSTEM_SLOW_JOBS
}

// jobNameRE is the schedule file's job name rule: names are URL path
// segments, so anything else could never be called.
var jobNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// LoadConfig reads the settings through getenv (os.Getenv in production) and
// returns every problem at once, each naming its variable.
func LoadConfig(getenv func(string) string) (Config, error) {
	var errs []error
	fail := func(name, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s %s", name, fmt.Sprintf(format, a...)))
	}
	c := Config{
		Listen:   strings.TrimSpace(getenv(EnvListen)),
		FailJobs: map[string]bool{},
		SlowJobs: map[string]time.Duration{},
	}

	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		fail(EnvListen, "must be host:port or :port, got %q", c.Listen)
	}

	keys := strings.TrimSpace(getenv(EnvPublicKey))
	if keys == "" {
		fail(EnvPublicKey, "is required: PARADOX's public key (print it with paradox pubkey)")
	} else {
		parts := strings.Split(keys, ",")
		if len(parts) > 2 {
			fail(EnvPublicKey, "holds %d keys; at most two are allowed, the current one and the next", len(parts))
		}
		for i, part := range parts {
			k, err := sign.ParsePublicKey(part)
			if err != nil {
				// The error text never includes the key itself.
				fail(EnvPublicKey, "key %d: %v", i+1, err)
				continue
			}
			c.ParadoxKeys = append(c.ParadoxKeys, k)
		}
	}

	for _, name := range list(getenv(EnvFailJobs)) {
		switch {
		case !jobNameRE.MatchString(name):
			fail(EnvFailJobs, "job %q must be lower-case letters, digits and hyphens, at most 63 characters", name)
		case c.FailJobs[name]:
			fail(EnvFailJobs, "job %q appears twice", name)
		default:
			c.FailJobs[name] = true
		}
	}

	for _, pair := range list(getenv(EnvSlowJobs)) {
		name, value, ok := strings.Cut(pair, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok {
			fail(EnvSlowJobs, "entry %q must be name=duration, for example slow=30s", pair)
			continue
		}
		if !jobNameRE.MatchString(name) {
			fail(EnvSlowJobs, "job %q must be lower-case letters, digits and hyphens, at most 63 characters", name)
			continue
		}
		if _, dup := c.SlowJobs[name]; dup {
			fail(EnvSlowJobs, "job %q appears twice", name)
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			fail(EnvSlowJobs, "job %q needs a positive duration such as 500ms, 30s or 2m, got %q", name, value)
			continue
		}
		c.SlowJobs[name] = d
	}

	return c, errors.Join(errs...)
}

// list splits a comma-separated setting, trimming spaces and skipping empty
// entries so a trailing comma is harmless.
func list(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// names returns the keys of a set in a stable order, for the start-up log.
func names[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
