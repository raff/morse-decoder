// engine_durwindow_test.go is a regression test for NOTES.md #8: toneDurMs/
// silDurMs must be windowed by cumulative duration, not a fixed entry count,
// so stale data from a different WPM rotates out in a bounded amount of time.
package main

import (
	"math"
	"testing"
)

func TestAppendDurTrimsByDuration(t *testing.T) {
	var durs []float64
	var sum float64
	for range 400 {
		durs = appendDur(durs, &sum, 100) // 400 * 100ms = 40s of data
	}

	var want float64
	for _, d := range durs {
		want += d
	}
	if math.Abs(sum-want) > 1e-9 {
		t.Errorf("sum %.1f out of sync with actual slice total %.1f", sum, want)
	}
	if sum > durationWindowMs {
		t.Errorf("windowed sum %.1f exceeds durationWindowMs %d", sum, durationWindowMs)
	}
	if len(durs) >= 400 {
		t.Errorf("len(durs) = %d, want trimmed below the full 400 appended", len(durs))
	}
}

func TestAppendDurKeepsAtLeastOneEntry(t *testing.T) {
	var durs []float64
	var sum float64
	durs = appendDur(durs, &sum, 2*durationWindowMs) // one pulse alone exceeds the window
	if len(durs) != 1 {
		t.Errorf("len(durs) = %d, want 1 (a single oversized entry must not be trimmed away)", len(durs))
	}
}
