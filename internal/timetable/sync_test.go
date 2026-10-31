package timetable

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rumeasiyan/paradox/internal/db"
	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/internal/systems"
)

// freshDB opens the test database with PARADOX's tables dropped and
// migrated again, so each test starts empty. River's tables are left alone.
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PARADOX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PARADOX_TEST_DATABASE_URL not set; start dev/compose.yaml to run database tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS run_action, queue_request, attempt, run, job, system, paradox_migration CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func job(t *testing.T, name, every string) schedule.Job {
	t.Helper()
	j := schedule.Job{Name: name, Every: every, Timeout: 10 * time.Minute, Retries: 3}
	if every != "" {
		j.Repeat = parse(t, every)
	}
	return j
}

func system(name, wake, address string, jobs ...schedule.Job) systems.System {
	return systems.System{System: schedule.System{Name: name, Wake: wake, Address: address, Jobs: jobs}}
}

type jobRow struct {
	repeat   *string
	timeout  int
	retries  int
	nextDue  *time.Time
	systemID int64
}

func readJob(t *testing.T, pool *pgxpool.Pool, sys, name string) jobRow {
	t.Helper()
	var r jobRow
	err := pool.QueryRow(context.Background(), `SELECT j.repeat, j.timeout_s, j.retries, j.next_due_at, j.system_id
		FROM job j JOIN system s ON s.id = j.system_id WHERE s.name = $1 AND j.name = $2`, sys, name).
		Scan(&r.repeat, &r.timeout, &r.retries, &r.nextDue, &r.systemID)
	if err != nil {
		t.Fatalf("job %s/%s: %v", sys, name, err)
	}
	return r
}

func removedAt(t *testing.T, pool *pgxpool.Pool, sys string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT removed_at FROM system WHERE name = $1`, sys).Scan(&at); err != nil {
		t.Fatalf("system %s: %v", sys, err)
	}
	return at
}

func wantDue(t *testing.T, what string, got *time.Time, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: next_due_at is NULL, want %s", what, want)
	}
	if !got.Equal(utc(want)) {
		t.Fatalf("%s: next_due_at %v, want %s", what, got.UTC(), want)
	}
}

func wantNoDue(t *testing.T, what string, got *time.Time) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s: next_due_at %v, want NULL", what, got.UTC())
	}
}

var (
	colombo = func(t *testing.T) *time.Location { return zone(t, "Asia/Colombo") }
	day1    = utc("2026-10-31T00:00:00Z")
)

func TestSyncFirstLoad(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	list := []systems.System{
		system("citadel", schedule.WakeSablier, "",
			job(t, "send-reminders", "0 7 * * *"),
			job(t, "generate-certificates", "")),
		system("outpost", schedule.WakeNone, "https://outpost.example.com", job(t, "tidy", "@hourly")),
	}
	if err := Sync(ctx, pool, list, colombo(t), day1); err != nil {
		t.Fatal(err)
	}

	var wake string
	var address *string
	if err := pool.QueryRow(ctx, `SELECT wake_mode, address FROM system WHERE name = 'citadel'`).Scan(&wake, &address); err != nil {
		t.Fatal(err)
	}
	if wake != "sablier" || address != nil {
		t.Fatalf("citadel: wake %q, address %v; want sablier and NULL", wake, address)
	}
	if err := pool.QueryRow(ctx, `SELECT wake_mode, address FROM system WHERE name = 'outpost'`).Scan(&wake, &address); err != nil {
		t.Fatal(err)
	}
	if wake != "none" || address == nil || *address != "https://outpost.example.com" {
		t.Fatalf("outpost: wake %q, address %v", wake, address)
	}

	r := readJob(t, pool, "citadel", "send-reminders")
	if r.repeat == nil || *r.repeat != "0 7 * * *" || r.timeout != 600 || r.retries != 3 {
		t.Fatalf("send-reminders row: %+v", r)
	}
	// 07:00 in Colombo is 01:30 UTC (how-it-works Flow A).
	wantDue(t, "send-reminders", r.nextDue, "2026-10-31T01:30:00Z")

	q := readJob(t, pool, "citadel", "generate-certificates")
	if q.repeat != nil {
		t.Fatalf("queue-only job has repeat %q, want NULL", *q.repeat)
	}
	wantNoDue(t, "queue-only job", q.nextDue)

	// @hourly is read in Colombo too: on the hour there is 30 minutes past in UTC.
	wantDue(t, "tidy", readJob(t, pool, "outpost", "tidy").nextDue, "2026-10-31T00:30:00Z")
	if removedAt(t, pool, "citadel") != nil || removedAt(t, pool, "outpost") != nil {
		t.Fatal("a listed system is marked removed")
	}
}

func TestSyncUnchangedKeepsNextDue(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	list := []systems.System{system("citadel", schedule.WakeSablier, "", job(t, "send-reminders", "0 7 * * *"))}
	if err := Sync(ctx, pool, list, colombo(t), day1); err != nil {
		t.Fatal(err)
	}
	// Restart three days later: the stored time stays so story 25 can
	// catch up the runs missed meanwhile.
	list[0].Jobs[0].Timeout = 90 * time.Second
	list[0].Jobs[0].Retries = 0
	if err := Sync(ctx, pool, list, colombo(t), day1.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := readJob(t, pool, "citadel", "send-reminders")
	wantDue(t, "unchanged repeat", r.nextDue, "2026-10-31T01:30:00Z")
	if r.timeout != 90 || r.retries != 0 {
		t.Fatalf("timeout %d, retries %d; want the new 90 and 0", r.timeout, r.retries)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM job`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d job rows after a second load, want 1", n)
	}
}

func TestSyncFillsMissingNextDue(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	list := []systems.System{system("citadel", schedule.WakeSablier, "", job(t, "send-reminders", "0 7 * * *"))}
	if err := Sync(ctx, pool, list, colombo(t), day1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE job SET next_due_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := Sync(ctx, pool, list, colombo(t), day1.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantDue(t, "repeat with no stored time", readJob(t, pool, "citadel", "send-reminders").nextDue, "2026-11-01T01:30:00Z")
}

func TestSyncRepeatChangeRecomputes(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	list := []systems.System{system("citadel", schedule.WakeSablier, "", job(t, "send-reminders", "0 7 * * *"))}
	if err := Sync(ctx, pool, list, colombo(t), day1); err != nil {
		t.Fatal(err)
	}
	list[0].Jobs[0] = job(t, "send-reminders", "0 9 * * *")
	if err := Sync(ctx, pool, list, colombo(t), day1.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := readJob(t, pool, "citadel", "send-reminders")
	if *r.repeat != "0 9 * * *" {
		t.Fatalf("repeat %q, want the new text", *r.repeat)
	}
	wantDue(t, "changed repeat", r.nextDue, "2026-11-03T03:30:00Z")

	// Turning a repeating job into a queue-only one clears its time.
	list[0].Jobs[0] = job(t, "send-reminders", "")
	if err := Sync(ctx, pool, list, colombo(t), day1.Add(96*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r = readJob(t, pool, "citadel", "send-reminders")
	if r.repeat != nil {
		t.Fatalf("repeat %q, want NULL", *r.repeat)
	}
	wantNoDue(t, "now queue-only", r.nextDue)
}

func TestSyncRemovedSystem(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	citadel := system("citadel", schedule.WakeSablier, "", job(t, "send-reminders", "0 7 * * *"))
	outpost := system("outpost", schedule.WakeNone, "", job(t, "tidy", "@hourly"))
	if err := Sync(ctx, pool, []systems.System{citadel, outpost}, colombo(t), day1); err != nil {
		t.Fatal(err)
	}
	before := readJob(t, pool, "outpost", "tidy")
	// A run of the removed system's job must still resolve afterwards.
	var runID string
	if err := pool.QueryRow(ctx, `INSERT INTO run (job_id, source, due_at, state, max_attempts)
		SELECT j.id, 'schedule', now(), 'succeeded', 1 FROM job j WHERE j.name = 'tidy' RETURNING id`).Scan(&runID); err != nil {
		t.Fatal(err)
	}

	gone := day1.Add(time.Hour)
	if err := Sync(ctx, pool, []systems.System{citadel}, colombo(t), gone); err != nil {
		t.Fatalf("removing a system: %v", err)
	}
	at := removedAt(t, pool, "outpost")
	if at == nil || !at.Equal(gone) {
		t.Fatalf("outpost removed_at %v, want %v", at, gone)
	}
	after := readJob(t, pool, "outpost", "tidy")
	wantNoDue(t, "job of removed system", after.nextDue)
	if after.systemID != before.systemID {
		t.Fatal("removed system's row was replaced")
	}
	var sys string
	if err := pool.QueryRow(ctx, `SELECT s.name FROM run r JOIN job j ON j.id = r.job_id JOIN system s ON s.id = j.system_id WHERE r.id = $1`, runID).Scan(&sys); err != nil || sys != "outpost" {
		t.Fatalf("old run resolves to %q (%v), want outpost", sys, err)
	}
	if removedAt(t, pool, "citadel") != nil {
		t.Fatal("citadel marked removed")
	}

	// A later load without it keeps the first removal time.
	if err := Sync(ctx, pool, []systems.System{citadel}, colombo(t), gone.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if at := removedAt(t, pool, "outpost"); at == nil || !at.Equal(gone) {
		t.Fatalf("outpost removed_at moved to %v", at)
	}

	// Coming back clears the mark and schedules it again.
	back := day1.Add(3 * time.Hour)
	if err := Sync(ctx, pool, []systems.System{citadel, outpost}, colombo(t), back); err != nil {
		t.Fatal(err)
	}
	if removedAt(t, pool, "outpost") != nil {
		t.Fatal("returned system still marked removed")
	}
	wantDue(t, "returned system's job", readJob(t, pool, "outpost", "tidy").nextDue, "2026-10-31T03:30:00Z")
	if readJob(t, pool, "outpost", "tidy").systemID != before.systemID {
		t.Fatal("returned system got a new row")
	}
}

func TestSyncRemovedJob(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	list := []systems.System{system("citadel", schedule.WakeSablier, "",
		job(t, "send-reminders", "0 7 * * *"),
		job(t, "weekly-report", "0 8 * * 1"))}
	if err := Sync(ctx, pool, list, colombo(t), day1); err != nil {
		t.Fatal(err)
	}
	list[0].Jobs = list[0].Jobs[:1]
	if err := Sync(ctx, pool, list, colombo(t), day1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := readJob(t, pool, "citadel", "weekly-report") // the row is kept
	wantNoDue(t, "removed job", r.nextDue)
	wantDue(t, "remaining job", readJob(t, pool, "citadel", "send-reminders").nextDue, "2026-10-31T01:30:00Z")
}

func TestSyncFailureChangesNothing(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	good := system("citadel", schedule.WakeSablier, "", job(t, "send-reminders", "0 7 * * *"))
	if err := Sync(ctx, pool, []systems.System{good}, colombo(t), day1); err != nil {
		t.Fatal(err)
	}

	// The database refuses retries over 100, so the whole load is undone,
	// including the update to citadel that came before it.
	changed := good
	changed.Jobs = []schedule.Job{job(t, "send-reminders", "0 9 * * *")}
	bad := system("outpost", schedule.WakeNone, "", job(t, "tidy", "@hourly"))
	bad.Jobs[0].Retries = 101
	err := Sync(ctx, pool, []systems.System{changed, bad}, colombo(t), day1.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "outpost/tidy") {
		t.Fatalf("got %v, want an error naming outpost/tidy", err)
	}
	r := readJob(t, pool, "citadel", "send-reminders")
	if *r.repeat != "0 7 * * *" {
		t.Fatalf("repeat %q after a failed load, want the old text", *r.repeat)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM system`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d systems after a failed load, want 1", n)
	}
}

func TestSyncRefusesBadInput(t *testing.T) {
	pool := freshDB(t)
	ctx := context.Background()
	unparsed := job(t, "send-reminders", "")
	unparsed.Every = "0 7 * * *"
	noTimeout := job(t, "send-reminders", "")
	noTimeout.Timeout = 0
	cases := []struct {
		name string
		list []systems.System
		want string
	}{
		{"empty list", nil, "no systems"},
		{"duplicate system", []systems.System{system("citadel", schedule.WakeNone, ""), system("citadel", schedule.WakeNone, "")}, "citadel appears twice"},
		{"every without a parsed schedule", []systems.System{system("citadel", schedule.WakeNone, "", unparsed)}, "was not parsed"},
		{"zero timeout", []systems.System{system("citadel", schedule.WakeNone, "", noTimeout)}, "timeout must be positive"},
		{"unknown wake mode", []systems.System{system("citadel", "always", "")}, "system citadel"},
	}
	for _, c := range cases {
		err := Sync(ctx, pool, c.list, time.UTC, day1)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM system`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d systems stored after refused loads, want 0", n)
	}
}

func TestSyncRoundsTimeoutUp(t *testing.T) {
	pool := freshDB(t)
	j := job(t, "quick", "")
	j.Timeout = 1500 * time.Millisecond
	if err := Sync(context.Background(), pool, []systems.System{system("citadel", schedule.WakeNone, "", j)}, time.UTC, day1); err != nil {
		t.Fatal(err)
	}
	if got := readJob(t, pool, "citadel", "quick").timeout; got != 2 {
		t.Fatalf("timeout_s %d, want 2", got)
	}
}
