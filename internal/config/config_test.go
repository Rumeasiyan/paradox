package config

import (
	"strings"
	"testing"
	"time"
)

// env builds a getenv from pairs; the private key is set unless overridden,
// since it is the one setting with no default.
func env(pairs ...string) func(string) string {
	m := map[string]string{"PARADOX_PRIVATE_KEY": "test-key"}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestDefaultsMatchThePRD(t *testing.T) {
	c, err := Load(env())
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"systems dir", c.SystemsDir, "./systems"},
		{"timezone", c.Timezone.String(), "UTC"},
		{"listen", c.Listen, ":8080"},
		{"system port (PX-SYS-04)", c.SystemPort, 8080},
		{"heartbeat (PX-REP-03)", c.HeartbeatEvery, 60 * time.Second},
		{"signature window (PX-SIG-03)", c.SignatureWindow, 5 * time.Minute},
		{"wake timeout (PX-RUN-01)", c.WakeTimeout, 2 * time.Minute},
		{"keep awake (PX-RUN-02)", c.KeepAwakeEvery, 60 * time.Second},
		{"max running (PX-RUN-04)", c.MaxRunning, 10},
		{"shutdown grace (PX-RUN-05)", c.ShutdownGrace, 30 * time.Second},
		{"job timeout (PX-SCH-02)", c.JobTimeout, 10 * time.Minute},
		{"job retries (PX-SCH-02)", c.JobRetries, 3},
		{"catch up (PX-SCH-05)", c.CatchUpWindow, time.Hour},
		{"max data (PX-JOB-01)", c.MaxDataBytes, 64 * 1024},
		{"max ahead (PX-JOB-02)", c.MaxAhead, 30 * 24 * time.Hour},
		{"request ID keep (PX-JOB-03)", c.RequestIDKeep, 24 * time.Hour},
		{"record keep (PX-REC-02)", c.RecordKeep, 90 * 24 * time.Hour},
		{"empty sablier URL", c.SablierURL, ""},
		{"empty heartbeat URL", c.HeartbeatURL, ""},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s: got %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	if len(c.RetryDelays) != 3 || c.RetryDelays[0] != want[0] || c.RetryDelays[1] != want[1] || c.RetryDelays[2] != want[2] {
		t.Errorf("retry delays (PX-REP-01): got %v, want %v", c.RetryDelays, want)
	}
}

func TestOverridesAreRead(t *testing.T) {
	c, err := Load(env(
		"PARADOX_TIMEZONE", "Asia/Tokyo",
		"PARADOX_RETRY_DELAYS", "30s, 2m",
		"PARADOX_RECORD_KEEP", "7d",
		"PARADOX_SABLIER_URL", "http://waker.example:10000",
	))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timezone.String() != "Asia/Tokyo" || len(c.RetryDelays) != 2 || c.RecordKeep != 7*24*time.Hour || c.SablierURL == "" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestMissingPrivateKeyRefusesStart(t *testing.T) {
	_, err := Load(env("PARADOX_PRIVATE_KEY", ""))
	if err == nil || !strings.Contains(err.Error(), "PARADOX_PRIVATE_KEY") {
		t.Fatalf("got %v, want an error naming PARADOX_PRIVATE_KEY", err)
	}
}

func TestEveryBadValueIsReportedAtOnce(t *testing.T) {
	_, err := Load(env(
		"PARADOX_TIMEZONE", "Mars/Olympus",
		"PARADOX_MAX_RUNNING", "0",
		"PARADOX_WAKE_TIMEOUT", "soon",
		"PARADOX_RETRY_DELAYS", "1m,,5m",
		"PARADOX_SABLIER_URL", "ftp://waker",
		"PARADOX_DATABASE_URL", "mysql://db/x",
	))
	if err == nil {
		t.Fatal("bad settings were accepted")
	}
	for _, name := range []string{"PARADOX_TIMEZONE", "PARADOX_MAX_RUNNING", "PARADOX_WAKE_TIMEOUT", "PARADOX_RETRY_DELAYS", "PARADOX_SABLIER_URL", "PARADOX_DATABASE_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
}

func TestKeepAwakeMustBeShorterThanWakeLimit(t *testing.T) {
	_, err := Load(env("PARADOX_KEEP_AWAKE_EVERY", "3m"))
	if err == nil || !strings.Contains(err.Error(), "PARADOX_KEEP_AWAKE_EVERY") {
		t.Fatalf("got %v, want a keep-awake error", err)
	}
}

func TestErrorsNeverEchoURLs(t *testing.T) {
	// URLs can carry passwords (the database URL usually does), so their
	// errors must not repeat the value.
	_, err := Load(env("PARADOX_DATABASE_URL", "mysql://user:s3cret@db/x"))
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("got %v, want an error without the password", err)
	}
}
