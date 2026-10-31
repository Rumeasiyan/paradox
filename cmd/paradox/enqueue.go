package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Rumeasiyan/paradox/sign"
)

// enqueueTimeout bounds one call to the queue API.
const enqueueTimeout = 30 * time.Second

// enqueue is a signed client for the queue API (contract section 6). It
// lets a deployment prove a queued job end to end without writing signing
// code (PX-CON-03): queue a job, read a run or cancel a run, as one system,
// using that system's private key file.
func enqueue(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enqueue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", "", "PARADOX base URL, for example http://paradox:8080")
	system := fs.String("system", "", "system to act as (its name in the systems folder)")
	keyFile := fs.String("key", "", "file holding that system's private key")
	job := fs.String("job", "", "job to queue")
	data := fs.String("data", "", "job data as JSON (optional)")
	runAt := fs.String("run-at", "", "when to run, RFC 3339 with an offset (optional; default now)")
	requestID := fs.String("request-id", "", "request ID (optional; default random, so every call queues a new run)")
	read := fs.String("read", "", "read this run ID instead of queueing")
	cancelID := fs.String("cancel", "", "cancel this run ID instead of queueing")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	modes := 0
	for _, set := range []bool{*job != "", *read != "", *cancelID != ""} {
		if set {
			modes++
		}
	}
	if *base == "" || *system == "" || *keyFile == "" || modes != 1 {
		fmt.Fprintln(stderr, "enqueue needs -url, -system, -key and exactly one of -job, -read or -cancel")
		return errUsage
	}
	u, err := url.Parse(strings.TrimRight(*base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("-url must be an http or https base URL")
	}
	keyText, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("-key: %w", err)
	}
	key, err := sign.ParsePrivateKey(string(keyText))
	if err != nil {
		return fmt.Errorf("-key %s: %w", *keyFile, err)
	}

	method, path := http.MethodPost, "/api/jobs"
	var body []byte
	switch {
	case *read != "":
		method, path = http.MethodGet, "/api/jobs/"+url.PathEscape(*read)
	case *cancelID != "":
		path = "/api/jobs/" + url.PathEscape(*cancelID) + "/cancel"
	default:
		req := map[string]any{"job": *job}
		if *data != "" {
			if !json.Valid([]byte(*data)) {
				return errors.New("-data is not valid JSON")
			}
			req["data"] = json.RawMessage(*data)
		}
		if *runAt != "" {
			if _, err := time.Parse(time.RFC3339, *runAt); err != nil {
				return errors.New("-run-at must be RFC 3339 with an offset, for example 2026-11-01T22:00:00+05:30")
			}
			req["run_at"] = *runAt
		}
		id := *requestID
		if id == "" {
			b := make([]byte, 12)
			rand.Read(b)
			id = "enqueue-" + hex.EncodeToString(b)
		}
		req["request_id"] = id
		body, _ = json.Marshal(req)
	}

	ctx, cancel := context.WithTimeout(context.Background(), enqueueTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, method, u.String()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if err := sign.SignForParadox(httpReq, body, *system, key); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("calling PARADOX: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var pretty bytes.Buffer
	if json.Indent(&pretty, answer, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(answer)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("PARADOX answered %d: %s", resp.StatusCode, strings.TrimSpace(pretty.String()))
	}
	fmt.Fprintln(stdout, strings.TrimSpace(pretty.String()))
	return nil
}
