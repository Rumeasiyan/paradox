package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adminEnv(srv *httptest.Server) func(string) string {
	host := strings.TrimPrefix(srv.URL, "http://")
	return func(k string) string {
		if k == "PARADOX_ADMIN_LISTEN" {
			return host
		}
		return ""
	}
}

func TestRunsPrintsATableAndPassesFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`[{"run_id":"r1","system":"citadel","job":"send-reminders","source":"schedule","state":"failed","due_at":"2026-11-02T07:00:00Z","attempts":4,"finished_at":null,"last_error":"answered 500:\n boom"}]`))
	}))
	defer srv.Close()
	code, out, errOut := runArgs(t, adminEnv(srv), "runs", "-failed", "-system", "citadel", "-since", "2h")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"state=failed", "system=citadel", "since=2h0m0s", "limit=50"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q lacks %s", gotQuery, want)
		}
	}
	if !strings.Contains(out, "DUE (UTC)") || !strings.Contains(out, "2026-11-02 07:00:00") || !strings.Contains(out, "answered 500: boom") {
		t.Fatalf("table:\n%s", out)
	}
}

func TestRunsJSONAndEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) }))
	defer srv.Close()
	if _, out, _ := runArgs(t, adminEnv(srv), "runs"); !strings.Contains(out, "no runs match") {
		t.Fatalf("empty table: %q", out)
	}
	if _, out, _ := runArgs(t, adminEnv(srv), "runs", "-json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty json: %q", out)
	}
}

func TestRunsReportsAServiceThatIsDownOrRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"limit must be 1 to 1000"}`))
	}))
	code, _, errOut := runArgs(t, adminEnv(srv), "runs", "-limit", "0")
	if code != 1 || !strings.Contains(errOut, "limit must be 1 to 1000") {
		t.Fatalf("refused: exit %d %s", code, errOut)
	}
	srv.Close()
	code, _, errOut = runArgs(t, adminEnv(srv), "runs")
	if code != 1 || !strings.Contains(errOut, "is `paradox serve` running") {
		t.Fatalf("down: exit %d %s", code, errOut)
	}
	if code, _, _ := runArgs(t, noEnv, "runs", "-failed", "-state", "queued"); code != 2 {
		t.Fatalf("conflicting flags: exit %d", code)
	}
}
