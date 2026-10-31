// Package timetable turns the loaded systems folder into the timetable
// stored in the database: one row per system and per job, each repeating
// job with the UTC time it is next due (PX-SCH-03, PX-SYS-05; how-it-works
// Flow A, steps 1 to 3).
package timetable

import (
	"time"

	"github.com/robfig/cron/v3"
)

// Next returns the first time the schedule is due strictly after after,
// with the schedule read as wall-clock times in loc, as a UTC time. A nil
// loc means UTC. It returns the zero time if the schedule is never due
// again (for example 30 February).
//
// Daylight-saving changes (PX-SCH-03: a time skipped or repeated runs once):
//
//   - A skipped time runs once, at the first real instant after the gap.
//     In Europe/London on 2026-03-29 the clocks go from 01:00 to 02:00, so
//     "30 1 * * *" runs at 02:00 BST (01:00 UTC). Several times inside one
//     gap, and a time that falls exactly at the end of it, share that one
//     run: "*/30 * * * *" runs at 00:30 GMT and then 02:00 BST, and next at
//     02:30 BST.
//   - A repeated time runs only at its first occurrence. In Europe/London on
//     2026-10-25 the clocks go from 02:00 back to 01:00, so "30 1 * * *"
//     runs at 01:30 BST (00:30 UTC) and not again at 01:30 GMT. This holds
//     for every field, so "0 * * * *" runs at 01:00 BST (00:00 UTC) and next
//     at 02:00 GMT (02:00 UTC): the repeated hour has no run of its own.
//
// robfig/cron alone does the opposite in both cases: it never runs a daily
// job whose time is skipped, and runs it twice when its time is repeated.
// So Next works on wall-clock times and maps each one to a real instant
// itself.
//
// "@every" schedules are a fixed interval, not a wall-clock time, so they
// are counted in real time and daylight saving does not affect them.
func Next(s cron.Schedule, loc *time.Location, after time.Time) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	spec, ok := s.(*cron.SpecSchedule)
	if !ok {
		return s.Next(after).UTC()
	}
	// Read the fields in plain wall-clock time: a copy pinned to UTC never
	// sees a daylight-saving change.
	wallSpec := *spec
	wallSpec.Location = time.UTC

	cursor := wall(after, loc)
	for {
		n := wallSpec.Next(cursor)
		if n.IsZero() {
			return time.Time{}
		}
		at := instant(n, loc)
		if at.After(after) {
			return at.UTC()
		}
		// Only in a repeated hour: this wall-clock time first happened at or
		// before after, so it has had its run.
		cursor = n
	}
}

// wall returns t's wall-clock time in loc, labelled as UTC.
func wall(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
}

// instant returns the first real instant whose wall clock in loc reads w
// (w is labelled UTC). If w falls in a gap it returns the end of the gap.
func instant(w time.Time, loc *time.Location) time.Time {
	guess := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), loc)
	start, end := guess.ZoneBounds()

	// The instants that read w use the offset of the zone around guess or
	// of a neighbouring one; keep the earliest that really reads w.
	offsets := []int{offset(guess)}
	if !start.IsZero() {
		offsets = append(offsets, offset(start.Add(-time.Nanosecond)))
	}
	if !end.IsZero() {
		offsets = append(offsets, offset(end))
	}
	var first time.Time
	for _, o := range offsets {
		x := w.Add(-time.Duration(o) * time.Second)
		if wall(x, loc).Equal(w) && (first.IsZero() || x.Before(first)) {
			first = x
		}
	}
	if !first.IsZero() {
		return first
	}

	// No instant reads w: it was skipped. The gap ends at the change whose
	// wall clock jumps over w.
	for _, change := range []time.Time{start, end} {
		if change.IsZero() {
			continue
		}
		if wall(change.Add(-time.Nanosecond), loc).Before(w) && wall(change, loc).After(w) {
			return change
		}
	}
	return guess // not reached for real timezone data
}

func offset(t time.Time) int {
	_, o := t.Zone()
	return o
}
