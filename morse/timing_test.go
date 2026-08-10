package morse

import "testing"

// TestUpdateIgnoresSingleOutlier is a regression test for NOTES.md #7: one
// noise-tainted dot that happens to slip past the noise gate should not be
// enough to boost the EMA learning rate and yank DotMs around.
func TestUpdateIgnoresSingleOutlier(t *testing.T) {
	e := NewEstimator(20, true) // DotMs = 60ms
	e.bootstrapped = true

	before := e.DotMs
	e.update(40) // single outlier, 33% fast — crosses the 30% deviation threshold once
	afterOneOutlier := e.DotMs

	moved := (before - afterOneOutlier) / before
	if moved > 0.05 {
		t.Errorf("DotMs moved %.1f%% from a single outlier (want boosted alpha only after %d consecutive), before=%.2f after=%.2f",
			moved*100, speedChangeStreak, before, afterOneOutlier)
	}

	// A normal dot in between should reset the streak, not extend it.
	e.update(60)
	if e.devStreak != 0 {
		t.Errorf("devStreak = %d after a non-deviating dot, want 0", e.devStreak)
	}
}

// TestUpdateBoostsOnSustainedSpeedChange verifies that several consecutive
// dots deviating the same direction do trigger the boosted EMA, so a genuine
// speed change is still tracked responsively.
func TestUpdateBoostsOnSustainedSpeedChange(t *testing.T) {
	e := NewEstimator(20, true) // DotMs = 60ms
	e.bootstrapped = true

	for range speedChangeStreak {
		e.update(30) // sender doubled speed: dot halved from 60ms to 30ms
	}
	if e.devStreak < speedChangeStreak {
		t.Fatalf("devStreak = %d, want >= %d after %d consecutive same-direction outliers", e.devStreak, speedChangeStreak, speedChangeStreak)
	}
	if e.alpha != 0.15 {
		t.Errorf("alpha = %.2f after sustained deviation, want boosted 0.15", e.alpha)
	}
}
