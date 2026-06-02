package analyze

import (
	"testing"
	"time"
)

func TestWindowWeeks(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var weeks []WeekStats
	// 52 weeks ending at base, oldest first.
	for i := 51; i >= 0; i-- {
		weeks = append(weeks, WeekStats{WeekStart: base.AddDate(0, 0, -7*i)})
	}

	t.Run("six months keeps recent weeks only", func(t *testing.T) {
		got := WindowWeeks(weeks, 6)
		if len(got) == 0 || len(got) >= len(weeks) {
			t.Fatalf("windowed len = %d, want a strict subset of %d", len(got), len(weeks))
		}
		cutoff := base.AddDate(0, -6, 0)
		if got[0].WeekStart.Before(cutoff) {
			t.Errorf("first kept week %s is before cutoff %s", got[0].WeekStart, cutoff)
		}
		// The week just before the kept range must be older than cutoff.
		dropped := len(weeks) - len(got)
		if dropped > 0 && !weeks[dropped-1].WeekStart.Before(cutoff) {
			t.Errorf("week before window (%s) should be < cutoff %s", weeks[dropped-1].WeekStart, cutoff)
		}
		if last := got[len(got)-1].WeekStart; !last.Equal(base) {
			t.Errorf("last kept week = %s, want %s", last, base)
		}
	})

	t.Run("zero or negative returns all", func(t *testing.T) {
		if got := WindowWeeks(weeks, 0); len(got) != len(weeks) {
			t.Errorf("months=0 len = %d, want %d", len(got), len(weeks))
		}
	})

	t.Run("range wider than data returns all", func(t *testing.T) {
		if got := WindowWeeks(weeks, 99); len(got) != len(weeks) {
			t.Errorf("wide window len = %d, want %d", len(got), len(weeks))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := WindowWeeks(nil, 6); got != nil {
			t.Errorf("nil input should return nil, got %v", got)
		}
	})
}
