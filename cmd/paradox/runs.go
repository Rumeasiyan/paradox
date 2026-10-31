package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Rumeasiyan/paradox/internal/admin"
	"github.com/Rumeasiyan/paradox/internal/config"
)

// adminTimeout bounds one operator command.
const adminTimeout = 30 * time.Second

// runs lists runs for the operator (PX-REC-03), asking the running
// service's loopback admin endpoint, since the service holds the database
// pool.
func runs(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	system := fs.String("system", "", "only this system")
	job := fs.String("job", "", "only this job")
	state := fs.String("state", "", "only this state: queued, running, succeeded, failed, cancelled, skipped or missed")
	failed := fs.Bool("failed", false, "only failed runs (same as -state failed)")
	since := fs.Duration("since", 24*time.Hour, "how far back to look by due time")
	limit := fs.Int("limit", 50, fmt.Sprintf("most runs to show, 1 to %d", admin.MaxLimit))
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	if *failed {
		if *state != "" && *state != "failed" {
			fmt.Fprintln(stderr, "-failed and -state disagree")
			return errUsage
		}
		*state = "failed"
	}
	q := url.Values{}
	for k, v := range map[string]string{"system": *system, "job": *job, "state": *state} {
		if v != "" {
			q.Set(k, v)
		}
	}
	q.Set("since", since.String())
	q.Set("limit", strconv.Itoa(*limit))

	var list []admin.Run
	if err := adminCall(getenv, http.MethodGet, "/admin/runs?"+q.Encode(), &list); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no runs match")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DUE (UTC)\tSYSTEM\tJOB\tSOURCE\tSTATE\tATTEMPTS\tRUN ID\tLAST ERROR")
	for _, r := range list {
		lastErr := ""
		if r.LastError != nil {
			lastErr = oneLine(*r.LastError, 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.DueAt.Format("2006-01-02 15:04:05"), r.System, r.Job, r.Source, r.State, r.Attempts, r.RunID, lastErr)
	}
	return tw.Flush()
}

// adminCall sends one request to the loopback admin endpoint and decodes
// the answer into out.
func adminCall(getenv func(string) string, method, path string, out any) error {
	cfgListen := getenv("PARADOX_ADMIN_LISTEN")
	if cfgListen == "" {
		cfgListen = config.Defaults["PARADOX_ADMIN_LISTEN"]
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+cfgListen+path, nil)
	if err != nil {
		return fmt.Errorf("PARADOX_ADMIN_LISTEN %q: %w", cfgListen, err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PARADOX is not answering on %s (is `paradox serve` running in this container?): %w", cfgListen, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("PARADOX answered %d: %s", resp.StatusCode, e.Error)
	}
	return json.Unmarshal(body, out)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
