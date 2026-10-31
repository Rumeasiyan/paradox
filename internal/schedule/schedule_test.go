package schedule

import (
	"strings"
	"testing"
	"time"
)

var defaults = Defaults{Timeout: 10 * time.Minute, Retries: 3}

const good = `system: citadel
wake: sablier
jobs:
  - name: send-reminders
    every: "0 7 * * *"
    timeout: 5m
    retries: 2
  - name: generate-certificates
`

func TestParsesAGoodFile(t *testing.T) {
	s, err := Parse("citadel/schedules.yaml", []byte(good), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "citadel" || s.Wake != WakeSablier || s.Address != "" || len(s.Jobs) != 2 {
		t.Fatalf("got %+v", s)
	}
	r := s.Jobs[0]
	if r.Name != "send-reminders" || r.Timeout != 5*time.Minute || r.Retries != 2 || r.Repeat == nil || r.Line != 4 {
		t.Fatalf("first job: %+v", r)
	}
	// A job with no every is queue only and takes the defaults (PX-SCH-02).
	q := s.Jobs[1]
	if q.Every != "" || q.Repeat != nil || q.Timeout != defaults.Timeout || q.Retries != defaults.Retries {
		t.Fatalf("queue-only job: %+v", q)
	}
	// The cron expression means 07:00 every day.
	next := r.Repeat.Next(time.Date(2026, 11, 1, 6, 59, 0, 0, time.UTC))
	if !next.Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("next run %v, want 07:00", next)
	}
}

func TestWakeDefaultsToSablier(t *testing.T) {
	s, err := Parse("f", []byte("system: a\njobs:\n  - name: j\n"), defaults)
	if err != nil || s.Wake != WakeSablier {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestOutsideSystem(t *testing.T) {
	s, err := Parse("f", []byte("system: site\nwake: none\naddress: https://jobs.example.com\njobs:\n  - name: j\n"), defaults)
	if err != nil || s.Address != "https://jobs.example.com" {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestFaultsNameFileLineAndField(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"empty", "", "f: file is empty"},
		{"not a mapping", "- a\n", "f:1: top level must be a mapping"},
		{"missing system", "jobs:\n  - name: j\n", "f:1: system is required"},
		{"bad system name", "system: Citadel_1\njobs:\n  - name: j\n", "f:1: system \"Citadel_1\""},
		{"unknown top field", "system: a\nowner: x\njobs:\n  - name: j\n", "f:2: unknown field owner"},
		{"bad wake", "system: a\nwake: always\njobs:\n  - name: j\n", "f:2: wake must be sablier or none"},
		{"missing jobs", "system: a\n", "f:1: jobs is required"},
		{"empty jobs", "system: a\njobs: []\n", "f:2: jobs must list at least one job"},
		{"job without name", "system: a\njobs:\n  - every: \"@daily\"\n", "f:3: jobs[0].name is required"},
		{"bad job name", "system: a\njobs:\n  - name: Send Reminders\n", "f:3: jobs[0].name"},
		{"duplicate job", "system: a\njobs:\n  - name: j\n  - name: j\n", "f:4: job j appears twice"},
		{"bad cron", "system: a\njobs:\n  - name: j\n    every: \"7am daily\"\n", "f:4: jobs[0].every must be five cron fields"},
		{"cron with timezone", "system: a\njobs:\n  - name: j\n    every: \"CRON_TZ=Asia/Tokyo 0 7 * * *\"\n", "without a timezone"},
		{"bad timeout", "system: a\njobs:\n  - name: j\n    timeout: -5m\n", "f:4: jobs[0].timeout"},
		{"bad retries", "system: a\njobs:\n  - name: j\n    retries: lots\n", "f:4: jobs[0].retries"},
		{"unknown job field", "system: a\njobs:\n  - name: j\n    owner: x\n", "f:4: jobs[0] has unknown field owner"},
		{"duplicate key", "system: a\nsystem: b\njobs:\n  - name: j\n", "f:2: system appears twice"},
		{"plain http address", "system: a\nwake: none\naddress: http://site.example\njobs:\n  - name: j\n", "address must use https"},
		{"address with path", "system: a\nwake: none\naddress: https://site.example/x\njobs:\n  - name: j\n", "must not have a path"},
		{"address but woken", "system: a\naddress: https://site.example\njobs:\n  - name: j\n", "must set wake: none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse("f", []byte(c.yaml), defaults)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestAllFaultsReportedTogether(t *testing.T) {
	_, err := Parse("f", []byte("system: a\nwake: maybe\njobs:\n  - name: j\n    retries: -1\n  - name: BAD\n"), defaults)
	if err == nil {
		t.Fatal("accepted a bad file")
	}
	for _, want := range []string{"wake must be", "retries must be", "jobs[1].name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
