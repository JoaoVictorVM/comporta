package limits

import (
	"sync"
	"time"
)

type window struct{ start, end time.Time }

var locations sync.Map

func loadLocation(name string) (*time.Location, error) {
	if cached, ok := locations.Load(name); ok {
		return cached.(*time.Location), nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	locations.Store(name, location)
	return location, nil
}

// firstInstant is the first valid instant of a local date. Midnight may not
// exist on a day with a DST transition, so the boundary is searched instead of
// built with time.Date, whose choice inside a gap is unspecified.
func firstInstant(year int, month time.Month, day int, location *time.Location) time.Time {
	target := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	before := func(t time.Time) bool {
		y, m, d := t.In(location).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Before(target)
	}
	low := target.Add(-36 * time.Hour).Unix()
	high := target.Add(36 * time.Hour).Unix()
	for low < high {
		middle := low + (high-low)/2
		if before(time.Unix(middle, 0)) {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return time.Unix(low, 0).UTC()
}

// calendarWindow is the local day or month containing an instant, as UTC bounds.
func calendarWindow(at time.Time, monthly bool, location *time.Location) window {
	local := at.In(location)
	year, month, day := local.Date()
	if monthly {
		return window{firstInstant(year, month, 1, location), firstInstant(year, month+1, 1, location)}
	}
	start := firstInstant(year, month, day, location)
	end := firstInstant(year, month, day+1, location)
	if !end.After(at) {
		// A backwards transition across midnight repeats the previous date after
		// the next one began; the instant then belongs to the later window.
		start, end = end, firstInstant(year, month, day+2, location)
	}
	return window{start, end}
}

func windowFor(at time.Time, d Dimension, timezone string) (window, error) {
	location, err := loadLocation(timezone)
	if err != nil {
		return window{}, err
	}
	return calendarWindow(at, d.monthly(), location), nil
}

func overlaps(a, b window) bool { return a.start.Before(b.end) && b.start.Before(a.end) }
