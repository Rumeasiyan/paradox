package report

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PushTimeout is the most time one push request may take.
const PushTimeout = 10 * time.Second

// MaxPushMessage is the most characters (not bytes) of the msg sent with a
// failure push, so the monitor's alert stays one short line.
const MaxPushMessage = 250

// Push is the Reporter for a push monitor such as Uptime Kuma (link 10 in PRD
// Annex B). Each report is one GET to a push URL with status, msg and the
// URL's own query (for example a token) kept:
//
//	heartbeat: GET <heartbeat URL>?status=up&msg=OK
//	failure:   GET <failure URL>?status=down&msg=<one plain line>
//
// The failure monitor is pushed "up" on every heartbeat as well. So it
// recovers one heartbeat after a failure, and every new failure is a fresh
// down after an up, which the monitor alerts on again. Without this the
// monitor would stay down after the first failure and stay silent for the
// next ones.
//
// Either URL may be empty: that half then behaves like Log (log only). The
// failure monitor is still pushed up on heartbeats when only the heartbeat
// URL is empty.
//
// Push URLs carry secret tokens, so no error or log line from Push contains
// a URL or any part of its query. Push is safe for concurrent use: it never
// changes after NewPush.
type Push struct {
	heartbeat *url.URL // nil means log only
	failure   *url.URL // nil means log only
	client    *http.Client
	log       Log
}

var _ Reporter = (*Push)(nil)

// NewPush builds a Push. Each non-empty URL must be http or https with a
// host. A nil client means a plain http.Client; each request is limited to
// PushTimeout either way. A nil logger means slog.Default().
func NewPush(heartbeatURL, failureURL string, client *http.Client, logger *slog.Logger) (*Push, error) {
	hb, err := parsePushURL("heartbeat", heartbeatURL)
	if err != nil {
		return nil, err
	}
	fl, err := parsePushURL("failure", failureURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: PushTimeout}
	}
	return &Push{heartbeat: hb, failure: fl, client: client, log: Log{Logger: logger}}, nil
}

// parsePushURL checks one push URL. Its errors never include the URL:
// url.Parse errors quote it, so they are replaced, not wrapped.
func parsePushURL(which, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s push URL is not a valid URL", which)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s push URL must start with http:// or https://", which)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%s push URL has no host", which)
	}
	return u, nil
}

// Heartbeat pushes status=up&msg=OK to the heartbeat URL, then status=up to
// the failure URL if one is set (see Push). With no heartbeat URL it logs as
// Log does. Both pushes are tried; the error joins whichever failed.
func (p *Push) Heartbeat(ctx context.Context) error {
	var errs []error
	if p.heartbeat == nil {
		_ = p.log.Heartbeat(ctx)
	} else if err := p.send(ctx, "heartbeat", p.heartbeat, "up", "OK"); err != nil {
		errs = append(errs, err)
	}
	if p.failure != nil {
		if err := p.send(ctx, "failure recovery", p.failure, "up", "OK"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Failure pushes status=down to the failure URL with a one-line msg built
// only from f's fields (PX-REP-02, PX-NFR-06). With no failure URL, or if
// the push fails, the report is also logged as Log does, so it is not lost.
func (p *Push) Failure(ctx context.Context, f Failure) error {
	f = f.Truncated()
	if p.failure == nil {
		return p.log.Failure(ctx, f)
	}
	if err := p.send(ctx, "failure", p.failure, "down", FailureMessage(f)); err != nil {
		_ = p.log.Failure(ctx, f)
		return err
	}
	return nil
}

// FailureMessage is the msg sent with a failure push: one plain line of at
// most MaxPushMessage characters, from the Failure's fields only. Control
// characters (such as new lines in an error) become spaces.
func FailureMessage(f Failure) string {
	f = f.Truncated()
	s := fmt.Sprintf("%s/%s run %s failed after %d attempts: %s",
		f.System, f.Job, f.RunID, f.Attempts, f.LastError)
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > MaxPushMessage {
		s = string([]rune(s)[:MaxPushMessage])
	}
	return s
}

// pushURL returns base with status and msg set, keeping its other query
// parameters. Spaces are sent as %20 rather than +, which every server
// decodes as a space.
func pushURL(base *url.URL, status, msg string) string {
	u := *base
	q := u.Query()
	q.Set("status", status)
	q.Set("msg", msg)
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String()
}

// send makes one push. Any 2xx answer is success. Errors name the push and
// the status code or the kind of transport failure, never the URL.
func (p *Push) send(ctx context.Context, which string, base *url.URL, status, msg string) error {
	ctx, cancel := context.WithTimeout(ctx, PushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pushURL(base, status, msg), nil)
	if err != nil {
		return fmt.Errorf("%s push: could not build the request", which)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s push: %s", which, transportFailure(ctx, err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s push: monitor answered %d", which, resp.StatusCode)
	}
	return nil
}

// transportFailure describes a failed request without its URL. A *url.Error
// quotes the full URL and a dial error names the address, so neither is
// kept; only the kind of failure is.
func transportFailure(ctx context.Context, err error) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return "timed out"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.As(err, &dnsErr):
		return "monitor host name not found"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "could not connect to the monitor"
	default:
		return "request failed"
	}
}
