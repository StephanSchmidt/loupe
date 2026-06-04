package analyze

import (
	"math"
	"math/rand"
	"sort"
)

// SigLevel labels a before↔after delta by how distinguishable it is from
// noise, using bootstrap confidence intervals on the per-week samples. (Named
// SigLevel rather than Confidence to avoid clashing with the AI-signal
// confidence tiers in signals.go.)
type SigLevel string

const (
	// SigHigh — the 95% CI of the difference excludes zero.
	SigHigh SigLevel = "high"
	// SigMedium — the 90% CI excludes zero but the 95% does not.
	SigMedium SigLevel = "medium"
	// SigNoise — the 90% CI includes zero; the window can't tell the change
	// apart from random week-to-week variation.
	SigNoise SigLevel = "within-noise"
	// SigInsufficient — a cohort has too few weeks to test.
	SigInsufficient SigLevel = "insufficient"
)

// bootstrapIters is the number of resamples. 2000 is plenty for a stable
// percentile CI at the ~6-52 week cohort sizes loupe deals with.
const bootstrapIters = 2000

// bootstrapSeed fixes the RNG so a given store always yields the same verdict
// — decks must be reproducible run to run.
const bootstrapSeed = 0x10515

// minCohortWeeks is the floor below which a bootstrap is not meaningful.
const minCohortWeeks = 3

// Delta is a before↔after comparison of a single metric with a significance
// verdict. Before/After are the per-week means of each cohort; the verdict
// comes from a bootstrap CI of their difference.
type Delta struct {
	Before     float64
	After      float64
	AbsChange  float64 // After − Before (raw, not orientation-adjusted)
	PctChange  float64 // relative to Before; 0 when Before == 0
	Confidence SigLevel
	Better     bool // the change is in the good direction (per lowerIsBetter)
	Flat       bool // confidence < medium → treat as no real change
	NBefore    int
	NAfter     int
}

// Meaningful reports whether the delta cleared at least the medium bar.
func (d Delta) Meaningful() bool {
	return d.Confidence == SigHigh || d.Confidence == SigMedium
}

// BootstrapDelta tests the after cohort against the before cohort on their
// per-week sample values. lowerIsBetter flips the "good direction" for metrics
// where down is better (revert rate, lead time, bug-fix days). The CI test
// itself (does the difference exclude zero?) is orientation-independent;
// orientation only sets the Better flag. Deterministic.
func BootstrapDelta(before, after []float64, lowerIsBetter bool) Delta {
	d := Delta{
		Before:  mean(before),
		After:   mean(after),
		NBefore: len(before),
		NAfter:  len(after),
	}
	d.AbsChange = d.After - d.Before
	if d.Before != 0 {
		d.PctChange = d.AbsChange / math.Abs(d.Before) * 100
	}
	oriented := d.AbsChange
	if lowerIsBetter {
		oriented = -oriented
	}
	d.Better = oriented > 0

	if len(before) < minCohortWeeks || len(after) < minCohortWeeks {
		d.Confidence = SigInsufficient
		d.Flat = true
		return d
	}

	rng := rand.New(rand.NewSource(bootstrapSeed)) // #nosec G404 -- not security-sensitive; fixed seed for reproducibility
	diffs := make([]float64, bootstrapIters)
	for i := 0; i < bootstrapIters; i++ {
		diffs[i] = resampleMean(rng, after) - resampleMean(rng, before)
	}
	sort.Float64s(diffs)

	excludesZero := func(loPct, hiPct float64) bool {
		lo := percentileSorted(diffs, loPct)
		hi := percentileSorted(diffs, hiPct)
		return lo > 0 || hi < 0
	}
	switch {
	case excludesZero(2.5, 97.5):
		d.Confidence = SigHigh
	case excludesZero(5, 95):
		d.Confidence = SigMedium
	default:
		d.Confidence = SigNoise
	}
	d.Flat = !d.Meaningful()
	return d
}

// resampleMean draws len(xs) values from xs with replacement and returns their
// mean.
func resampleMean(rng *rand.Rand, xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < len(xs); i++ {
		sum += xs[rng.Intn(len(xs))]
	}
	return sum / float64(len(xs))
}

// percentileSorted returns the p-th percentile (0-100) of an already-sorted
// slice via nearest-rank.
func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// --- per-metric per-week sample extractors -------------------------------
// Each turns a cohort slice into the []float64 of weekly values a bootstrap
// runs over, reusing the metrics' existing derived methods.

// ProductivitySeries is commits per active developer, per week (0-author weeks
// skipped).
func ProductivitySeries(ws []WeekStats) []float64 {
	out := make([]float64, 0, len(ws))
	for _, w := range ws {
		if w.DistinctAuthors > 0 {
			out = append(out, float64(w.TotalCommits)/float64(w.DistinctAuthors))
		}
	}
	return out
}

// RevertSeries is the revert rate (%) per week (weeks with commits).
func RevertSeries(ws []DefectWeek) []float64 {
	out := make([]float64, 0, len(ws))
	for _, w := range ws {
		if w.Commits > 0 {
			out = append(out, w.RevertRate())
		}
	}
	return out
}

// BugRateSeries is the bug rate (%) per week (weeks with tickets).
func BugRateSeries(ws []DefectWeek) []float64 {
	out := make([]float64, 0, len(ws))
	for _, w := range ws {
		if w.Tickets > 0 {
			out = append(out, w.BugRate())
		}
	}
	return out
}

// BugFixSeries is the bug-class median engineering time (days) per week (weeks
// with at least one finished bug).
func BugFixSeries(ws []BugFixWeek) []float64 {
	out := make([]float64, 0, len(ws))
	for _, w := range ws {
		if w.BugCount > 0 {
			out = append(out, w.BugMedianLead.Hours()/24)
		}
	}
	return out
}

// LeadTimeSeries is the median dev→release lead time (days) per week.
func LeadTimeSeries(ws []WeekCycle) []float64 {
	out := make([]float64, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.MedianDevToRelease.Hours()/24)
	}
	return out
}
