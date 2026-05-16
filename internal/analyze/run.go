package analyze

import (
	"context"
	"fmt"

	"github.com/StephanSchmidt/loupe/internal/store"
)

// DetectionConfig is the analyze-side mirror of config.DetectionConfig —
// kept here to avoid importing the config package from analyze (the
// import direction is one-way: cmd → analyze, cmd → config).
type DetectionConfig struct {
	PRLabels            []string
	BranchPrefixes      []string
	SquashMergeRecovery bool
	SeatInference       bool
}

// RunAllDetectors executes every active detector against the local store
// and returns the total signal count written. Each individual detector is
// idempotent, so calling RunAllDetectors twice doesn't duplicate rows.
//
// Order matters for seat-holder inference: it must run last because it
// depends on every other high-confidence signal already being persisted.
func RunAllDetectors(ctx context.Context, s *store.Store, cfg DetectionConfig) (int, error) {
	total := 0

	// Link commits to tickets first — cycle-time computation and the
	// ticket_commits-joined PR signals both depend on it. Idempotent.
	if _, err := LinkCommitsToTickets(ctx, s); err != nil {
		return total, fmt.Errorf("ticket linking: %w", err)
	}

	n, err := DetectAndStore(ctx, s)
	if err != nil {
		return total, fmt.Errorf("commit-level signals: %w", err)
	}
	total += n

	prCfg := PRSignalConfig{
		Labels:         cfg.PRLabels,
		BranchPrefixes: cfg.BranchPrefixes,
	}
	if len(prCfg.Labels) == 0 && len(prCfg.BranchPrefixes) == 0 {
		prCfg = DefaultPRSignalConfig()
	}
	n, err = DetectPRSignals(ctx, s, prCfg)
	if err != nil {
		return total, fmt.Errorf("PR-level signals: %w", err)
	}
	total += n

	if cfg.SquashMergeRecovery {
		n, err = DetectSquashRecovery(ctx, s)
		if err != nil {
			return total, fmt.Errorf("squash recovery: %w", err)
		}
		total += n
	}

	if cfg.SeatInference {
		n, err = InferFromSeatHolders(ctx, s)
		if err != nil {
			return total, fmt.Errorf("seat inference: %w", err)
		}
		total += n
	}
	return total, nil
}
