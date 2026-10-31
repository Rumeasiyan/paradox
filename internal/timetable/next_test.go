package timetable

import (
	"testing"
	"time"
	_ "time/tzdata" // the tests must not depend on the machine's zone files

	"github.com/robfig/cron/v3"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func parse(t *testing.T, spec string) cron.Schedule {
	t.Helper()
	s, err := cron.ParseStandard(spec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// series returns the next count due times after start.
func series(t *testing.T, spec string, loc *time.Location, start string, count int) []string {
	t.Helper()
	s := parse(t, spec)
	at := utc(start)
	var out []string
	for range count {
		n := Next(s, loc, at)
		if n.Location() != time.UTC {
			t.Fatalf("%s: result %v is not in UTC", spec, n)
		}
		if !n.After(at) {
			t.Fatalf("%s: %v is not after %v", spec, n, at)
		}
		out = append(out, n.Format(time.RFC3339))
		at = n
	}
	return out
}

func equal(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

func TestRobfigAloneMishandlesDaylightSaving(t *testing.T) {
	// Records why Next does its own mapping: if robfig/cron ever changes,
	// this test says so and Next's comment must be revisited.
	ldn := zone(t, "Europe/London")
	s := parse(t, "30 1 * * *")

	spring := s.Next(utc("2026-03-28T23:00:00Z").In(ldn)).UTC()
	if spring.Format(time.RFC3339) != "2026-03-30T00:30:00Z" {
		t.Errorf("robfig spring: %v, expected it to skip 29 March", spring)
	}
	first := s.Next(utc("2026-10-24T23:00:00Z").In(ldn))
	second := s.Next(first).UTC()
	if second.Format(time.RFC3339) != "2026-10-25T01:30:00Z" {
		t.Errorf("robfig autumn: second run %v, expected the repeated 01:30", second)
	}
}

func TestNextOnNormalDays(t *testing.T) {
	equal(t, "daily in UTC", series(t, "30 1 * * *", time.UTC, "2026-06-01T00:00:00Z", 3),
		[]string{"2026-06-01T01:30:00Z", "2026-06-02T01:30:00Z", "2026-06-03T01:30:00Z"})
	// British Summer Time is UTC+1.
	equal(t, "daily in London summer", series(t, "30 1 * * *", zone(t, "Europe/London"), "2026-06-01T01:00:00Z", 2),
		[]string{"2026-06-02T00:30:00Z", "2026-06-03T00:30:00Z"})
	equal(t, "hourly in London summer", series(t, "0 * * * *", zone(t, "Europe/London"), "2026-06-01T10:15:00Z", 3),
		[]string{"2026-06-01T11:00:00Z", "2026-06-01T12:00:00Z", "2026-06-01T13:00:00Z"})
	equal(t, "nil location is UTC", series(t, "0 7 * * *", nil, "2026-06-01T08:00:00Z", 1),
		[]string{"2026-06-02T07:00:00Z"})
}

func TestNextIsStrictlyAfter(t *testing.T) {
	got := Next(parse(t, "30 1 * * *"), time.UTC, utc("2026-06-01T01:30:00Z"))
	if want := utc("2026-06-02T01:30:00Z"); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextInOtherTimezones(t *testing.T) {
	// The how-it-works example: 07:00 in Colombo (UTC+05:30) is 01:30 UTC.
	equal(t, "Colombo", series(t, "0 7 * * *", zone(t, "Asia/Colombo"), "2026-10-31T00:00:00Z", 2),
		[]string{"2026-10-31T01:30:00Z", "2026-11-01T01:30:00Z"})
	equal(t, "Tokyo weekly", series(t, "0 9 * * 1", zone(t, "Asia/Tokyo"), "2026-10-31T00:00:00Z", 1),
		[]string{"2026-11-02T00:00:00Z"})
	// New York skips 02:00 to 03:00 on 2026-03-08; 02:30 runs at 03:00 EDT.
	equal(t, "New York spring", series(t, "30 2 * * *", zone(t, "America/New_York"), "2026-03-07T00:00:00Z", 3),
		[]string{"2026-03-07T07:30:00Z", "2026-03-08T07:00:00Z", "2026-03-09T06:30:00Z"})
}

func TestNextSkippedTimeRunsOnce(t *testing.T) {
	ldn := zone(t, "Europe/London")
	// 2026-03-29: 01:00 GMT becomes 02:00 BST, at 01:00 UTC.
	equal(t, "daily 01:30", series(t, "30 1 * * *", ldn, "2026-03-28T12:00:00Z", 3),
		[]string{"2026-03-29T01:00:00Z", "2026-03-30T00:30:00Z", "2026-03-31T00:30:00Z"})
	equal(t, "hourly", series(t, "0 * * * *", ldn, "2026-03-28T23:30:00Z", 4),
		[]string{"2026-03-29T00:00:00Z", "2026-03-29T01:00:00Z", "2026-03-29T02:00:00Z", "2026-03-29T03:00:00Z"})
	// 01:00, 01:30 and 02:00 share the one run at the end of the gap.
	equal(t, "every half hour", series(t, "*/30 * * * *", ldn, "2026-03-29T00:00:00Z", 3),
		[]string{"2026-03-29T00:30:00Z", "2026-03-29T01:00:00Z", "2026-03-29T01:30:00Z"})
	// Starting inside the day, after the gap, does not run it again.
	equal(t, "after the gap", series(t, "30 1 * * *", ldn, "2026-03-29T01:00:00Z", 1),
		[]string{"2026-03-30T00:30:00Z"})
}

func TestNextRepeatedTimeRunsOnce(t *testing.T) {
	ldn := zone(t, "Europe/London")
	// 2026-10-25: 02:00 BST becomes 01:00 GMT, at 01:00 UTC.
	equal(t, "daily 01:30", series(t, "30 1 * * *", ldn, "2026-10-24T12:00:00Z", 3),
		[]string{"2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z", "2026-10-27T01:30:00Z"})
	equal(t, "hourly", series(t, "0 * * * *", ldn, "2026-10-24T23:30:00Z", 4),
		[]string{"2026-10-25T00:00:00Z", "2026-10-25T02:00:00Z", "2026-10-25T03:00:00Z", "2026-10-25T04:00:00Z"})
	// Starting in the second 01:00 to 02:00 hour: 01:30 has already run.
	equal(t, "inside the repeat", series(t, "30 1 * * *", ldn, "2026-10-25T01:10:00Z", 1),
		[]string{"2026-10-26T01:30:00Z"})
	equal(t, "half hour inside the repeat", series(t, "*/30 * * * *", ldn, "2026-10-25T01:10:00Z", 2),
		[]string{"2026-10-25T02:00:00Z", "2026-10-25T02:30:00Z"})
	// Starting in the first 01:00 to 02:00 hour: 01:30 BST is still to come.
	equal(t, "before the repeat", series(t, "30 1 * * *", ldn, "2026-10-25T00:10:00Z", 1),
		[]string{"2026-10-25T00:30:00Z"})
}

func TestNextEveryIsRealTime(t *testing.T) {
	// A fixed interval keeps its spacing across the autumn change.
	equal(t, "@every 1h", series(t, "@every 1h", zone(t, "Europe/London"), "2026-10-24T23:30:00Z", 3),
		[]string{"2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z", "2026-10-25T02:30:00Z"})
}

func TestNextNeverDue(t *testing.T) {
	if got := Next(parse(t, "0 0 30 2 *"), time.UTC, utc("2026-01-01T00:00:00Z")); !got.IsZero() {
		t.Fatalf("30 February: got %v, want the zero time", got)
	}
}
