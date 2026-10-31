package wake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sablier's blocking strategy (PRD Annex B link 7): GET this path under the
// base URL with group, session_duration and timeout. Sablier holds the
// request until every container in the group is ready or the timeout runs
// out. It answers 200 with the status header below set to "ready" or
// "not-ready", 504 when the group was not ready before the timeout, 503 when
// it gave up because the request was cancelled, 404 for an unknown group,
// 400 for a bad request and 500 for its own errors.
const (
	sablierBlockingPath = "/api/strategies/blocking"
	sablierStatusHeader = "X-Sablier-Session-Status"
	sablierStatusReady  = "ready"
)

const (
	// keepAwakeTimeout is the timeout sent with a keep-awake request. The
	// system is already up, so Sablier should answer at once; this only
	// bounds a Sablier that has to start it again.
	keepAwakeTimeout = 5 * time.Second

	// keepAwakeGrace is how much longer than keepAwakeTimeout PARADOX waits
	// for the answer, so Sablier's own timeout answer can arrive.
	keepAwakeGrace = 2 * time.Second

	// maxTimeoutMargin caps how much of the caller's remaining time is kept
	// back from the timeout sent to Sablier, so Sablier answers before ctx
	// ends and the answer can be read.
	maxTimeoutMargin = time.Second

	// maxBodyRead is how much of an answer is read, for its problem detail
	// and so the connection can be reused.
	maxBodyRead = 64 << 10

	// maxDetail is the longest problem detail quoted in an error.
	maxDetail = 200
)

// Sablier is the Waker that asks a Sablier server to start a system and keep
// it running (PX-RUN-01, PX-RUN-02). Each system is one Sablier group, named
// after the system. A Sablier is safe for concurrent use; make one with
// NewSablier.
type Sablier struct {
	endpoint url.URL       // blocking strategy URL, without credentials
	user     *url.Userinfo // credentials from the base URL, if any
	session  time.Duration
	client   *http.Client
}

var _ Waker = (*Sablier)(nil)

// NewSablier returns a Sablier that talks to the Sablier API at baseURL
// (PARADOX_SABLIER_URL, for example http://sablier.example:10000). baseURL
// must use http or https, name a host and carry no query or fragment; a path
// is kept as Sablier's base path. Credentials in baseURL are sent as basic
// authentication and never appear in errors.
//
// session is the session_duration sent with every request: how long Sablier
// keeps the system running after the last wake or keep awake. It must be
// positive, and longer than the keep-awake interval so a running job is not
// stopped. A nil client means a new http.Client; no client timeout is needed,
// since every request is bounded by its context.
func NewSablier(baseURL string, session time.Duration, client *http.Client) (*Sablier, error) {
	// The parse error would quote the URL, credentials included, so it is
	// not wrapped.
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.New("sablier URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("sablier URL must use http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("sablier URL must name a host")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("sablier URL must not have a query")
	}
	if u.Fragment != "" {
		return nil, errors.New("sablier URL must not have a fragment")
	}
	if session <= 0 {
		return nil, fmt.Errorf("sablier session must be positive, got %s", session)
	}
	if client == nil {
		client = &http.Client{}
	}

	s := &Sablier{user: u.User, session: session, client: client}
	u.User = nil
	u.Path = strings.TrimSuffix(u.Path, "/") + sablierBlockingPath
	u.RawPath = ""
	s.endpoint = *u
	return s, nil
}

// Wake asks Sablier to start the system's group and blocks until Sablier
// says it is ready. The timeout sent to Sablier is taken from ctx's deadline
// (WakeWithin sets one), less a small margin so Sablier's answer arrives in
// time; with no deadline, Sablier's default timeout applies.
//
// It returns nil only when Sablier answers ready and ctx has not ended. A
// Sablier timeout or not-ready answer wraps ErrTimeout; ctx ending returns
// ctx's error. Any other failure is a plain error naming the system.
func (s *Sablier) Wake(ctx context.Context, system string) error {
	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			<-ctx.Done()
			return ctx.Err()
		}
		timeout = remaining - min(remaining/20, maxTimeoutMargin)
	}
	return s.request(ctx, system, timeout)
}

// KeepAwake renews the system's session for another session duration
// (PX-RUN-02). The system is already up, so Sablier should answer at once;
// the request is bounded by a short timeout of its own whatever ctx allows.
func (s *Sablier) KeepAwake(ctx context.Context, system string) error {
	kctx, cancel := context.WithTimeout(ctx, keepAwakeTimeout+keepAwakeGrace)
	defer cancel()
	if err := s.request(kctx, system, keepAwakeTimeout); err != nil {
		return fmt.Errorf("keep %s awake: %w", system, err)
	}
	return nil
}

// request makes one blocking strategy call for the system's group. A
// timeout of zero is left out, so Sablier uses its default.
func (s *Sablier) request(ctx context.Context, system string, timeout time.Duration) error {
	q := url.Values{}
	q.Set("group", system)
	q.Set("session_duration", s.session.String())
	if timeout > 0 {
		// Sablier reads Go durations; whole milliseconds keep it short.
		q.Set("timeout", max(timeout.Truncate(time.Millisecond), time.Millisecond).String())
	}
	u := s.endpoint
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("sablier request for %s: %w", system, err)
	}
	req.Header.Set("Accept", "application/json")
	if s.user != nil {
		password, _ := s.user.Password()
		req.SetBasicAuth(s.user.Username(), password)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A *url.Error quotes the request URL; keep only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("sablier unreachable for %s: %w", system, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))

	// Never report ready once ctx has ended (PX-RUN-01).
	if ctx.Err() != nil {
		return ctx.Err()
	}

	switch resp.StatusCode {
	case http.StatusOK:
		switch status := resp.Header.Get(sablierStatusHeader); status {
		case sablierStatusReady:
			return nil
		case "":
			return fmt.Errorf("sablier answered %s for %s without %s",
				resp.Status, system, sablierStatusHeader)
		default:
			return fmt.Errorf("sablier says %s is %q: %w", system, status, ErrTimeout)
		}
	case http.StatusGatewayTimeout:
		return fmt.Errorf("sablier timed out waiting for %s: %w", system, ErrTimeout)
	default:
		msg := fmt.Sprintf("sablier answered %s for %s", resp.Status, system)
		if d := problemDetail(body); d != "" {
			msg += ": " + d
		}
		return errors.New(msg)
	}
}

// problemDetail returns the detail (or failing that the title) of an RFC
// 7807 problem body, shortened, or "" if the body is not one. The problem's
// instance field echoes the request path and query, so it is not used.
func problemDetail(body []byte) string {
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &p) != nil {
		return ""
	}
	d := strings.TrimSpace(p.Detail)
	if d == "" {
		d = strings.TrimSpace(p.Title)
	}
	d = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, d)
	if r := []rune(d); len(r) > maxDetail {
		d = string(r[:maxDetail]) + "..."
	}
	return d
}
