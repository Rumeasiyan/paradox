package report

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records decodes each JSON log line in the buffer.
func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func newLogger(level slog.Level) (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})), buf
}

func TestLogHeartbeatAtDebug(t *testing.T) {
	logger, buf := newLogger(slog.LevelDebug)
	if err := (Log{Logger: logger}).Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	recs := buf.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0]["level"] != "DEBUG" || recs[0]["msg"] != "heartbeat" {
		t.Errorf("got %v, want a DEBUG heartbeat", recs[0])
	}
}

func TestLogHeartbeatHiddenAtInfo(t *testing.T) {
	logger, buf := newLogger(slog.LevelInfo)
	if err := (Log{Logger: logger}).Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("heartbeat appeared at Info level: %q", got)
	}
}

func TestLogFailureAtErrorWithFields(t *testing.T) {
	logger, buf := newLogger(slog.LevelInfo)
	f := NewFailure("citadel", "nightly-report", "run-42", 4, "connection refused")
	if err := (Log{Logger: logger}).Failure(context.Background(), f); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	recs := buf.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	want := map[string]any{
		"level":      "ERROR",
		"system":     "citadel",
		"job":        "nightly-report",
		"run_id":     "run-42",
		"attempts":   float64(4),
		"last_error": "connection refused",
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %v, want %v", k, r[k], v)
		}
	}
	// time, level, msg and the five fields; nothing else.
	if len(r) != 8 {
		t.Errorf("record has %d keys, want 8: %v", len(r), r)
	}
}

func TestLogFailureTruncatesHandBuiltFailure(t *testing.T) {
	logger, buf := newLogger(slog.LevelInfo)
	f := Failure{System: "citadel", Job: "j", RunID: "r", Attempts: 1, LastError: strings.Repeat("x", 5000)}
	if err := (Log{Logger: logger}).Failure(context.Background(), f); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	got, _ := buf.records(t)[0]["last_error"].(string)
	if len(got) != MaxErrorBytes {
		t.Errorf("logged last_error is %d bytes, want %d", len(got), MaxErrorBytes)
	}
}

func TestLogNilLoggerUsesDefault(t *testing.T) {
	logger, buf := newLogger(slog.LevelInfo)
	old := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(old) })
	if err := (Log{}).Failure(context.Background(), NewFailure("citadel", "j", "r", 1, "boom")); err != nil {
		t.Fatalf("Failure: %v", err)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("nil Logger did not log to the default logger: %q", buf.String())
	}
}

func TestTruncateError(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"empty", "", 0},
		{"short", "boom", 4},
		{"exactly the limit", strings.Repeat("a", MaxErrorBytes), MaxErrorBytes},
		{"one over", strings.Repeat("a", MaxErrorBytes+1), MaxErrorBytes},
		// "é" is 2 bytes; byte 2048 would fall in the middle of one.
		{"two-byte char across the limit", strings.Repeat("a", MaxErrorBytes-1) + "é", MaxErrorBytes - 1},
		// The Sinhala letter "අ" is 3 bytes; the cut drops the whole letter.
		{"three-byte char across the limit", strings.Repeat("a", MaxErrorBytes-2) + "අ", MaxErrorBytes - 2},
		// An emoji is 4 bytes.
		{"four-byte char across the limit", strings.Repeat("a", MaxErrorBytes-3) + "\U0001F600", MaxErrorBytes - 3},
		{"four-byte char ending at the limit", strings.Repeat("a", MaxErrorBytes-4) + "\U0001F600z", MaxErrorBytes},
		{"all multi-byte", strings.Repeat("අ", 1000), 2046},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateError(c.in)
			if len(got) != c.wantLen {
				t.Errorf("len = %d, want %d", len(got), c.wantLen)
			}
			if !strings.HasPrefix(c.in, got) {
				t.Errorf("result is not a prefix of the input")
			}
			if utf8.ValidString(c.in) && !utf8.ValidString(got) {
				t.Errorf("result splits a UTF-8 character")
			}
		})
	}
}

func TestNewFailureTruncates(t *testing.T) {
	f := NewFailure("citadel", "j", "r", 3, strings.Repeat("é", 2000))
	if len(f.LastError) > MaxErrorBytes || !utf8.ValidString(f.LastError) {
		t.Errorf("LastError is %d bytes, valid UTF-8 %v", len(f.LastError), utf8.ValidString(f.LastError))
	}
	if f.System != "citadel" || f.Job != "j" || f.RunID != "r" || f.Attempts != 3 {
		t.Errorf("fields not kept: %+v", f)
	}
}

func TestTruncatedLeavesOriginal(t *testing.T) {
	long := strings.Repeat("x", 3000)
	f := Failure{LastError: long}
	got := f.Truncated()
	if len(got.LastError) != MaxErrorBytes || f.LastError != long {
		t.Errorf("Truncated: got %d bytes, original changed %v", len(got.LastError), f.LastError != long)
	}
}

// TestFailureCarriesNoJobData pins the Failure fields. A new field could let
// job data (a payload) reach the monitor, which PX-NFR-06 forbids; adding
// one must be a deliberate change to this test.
func TestFailureCarriesNoJobData(t *testing.T) {
	want := []struct {
		name string
		kind reflect.Kind
	}{
		{"System", reflect.String},
		{"Job", reflect.String},
		{"RunID", reflect.String},
		{"Attempts", reflect.Int},
		{"LastError", reflect.String},
	}
	typ := reflect.TypeOf(Failure{})
	if typ.NumField() != len(want) {
		t.Fatalf("Failure has %d fields, want %d", typ.NumField(), len(want))
	}
	for i, w := range want {
		f := typ.Field(i)
		if f.Name != w.name || f.Type.Kind() != w.kind || f.Type.PkgPath() != "" {
			t.Errorf("field %d is %s %s, want %s %s", i, f.Name, f.Type, w.name, w.kind)
		}
	}
}
