package analyze

import "time"

// windowByMonths keeps only the items within the last `months` months of the
// most recent item present. Items must be sorted ascending by the time
// returned by at. months <= 0 (or empty input) returns the slice unchanged.
// The anchor is the latest item rather than time.Now so a deck built from
// slightly stale data still shows its most recent window instead of nothing.
func windowByMonths[T any](items []T, months int, at func(T) time.Time) []T {
	if months <= 0 || len(items) == 0 {
		return items
	}
	cutoff := at(items[len(items)-1]).AddDate(0, -months, 0)
	for i := range items {
		if !at(items[i]).Before(cutoff) {
			return items[i:]
		}
	}
	return items[len(items):]
}

// WindowWeeks restricts weekly stats to the last `months` months — the
// default deck range, so charts stay readable instead of plotting years of
// weekly buckets.
func WindowWeeks(weeks []WeekStats, months int) []WeekStats {
	return windowByMonths(weeks, months, func(w WeekStats) time.Time { return w.WeekStart })
}

// WindowCycles restricts weekly cycle-time stats to the last `months` months.
func WindowCycles(cycles []WeekCycle, months int) []WeekCycle {
	return windowByMonths(cycles, months, func(c WeekCycle) time.Time { return c.WeekStart })
}

// WindowRepoAdoption restricts weekly repo-adoption stats to the last
// `months` months.
func WindowRepoAdoption(rows []RepoAdoptionWeek, months int) []RepoAdoptionWeek {
	return windowByMonths(rows, months, func(r RepoAdoptionWeek) time.Time { return r.WeekStart })
}

// WindowWIP restricts weekly work-in-progress stats to the last `months`
// months.
func WindowWIP(rows []WIPWeek, months int) []WIPWeek {
	return windowByMonths(rows, months, func(r WIPWeek) time.Time { return r.WeekStart })
}

// WindowDefects restricts weekly defect stats to the last `months` months.
func WindowDefects(rows []DefectWeek, months int) []DefectWeek {
	return windowByMonths(rows, months, func(r DefectWeek) time.Time { return r.WeekStart })
}

// WindowBugFix restricts weekly bug-fix lead-time stats to the last `months`
// months.
func WindowBugFix(rows []BugFixWeek, months int) []BugFixWeek {
	return windowByMonths(rows, months, func(r BugFixWeek) time.Time { return r.WeekStart })
}
