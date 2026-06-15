package morse

import (
	"fmt"
	"math"
	"sort"
)

// SpeedEstimator tracks the dot length (in milliseconds) of a Morse transmission.
// It supports an initial hint (from --wpm), batch bootstrap via k-means, and
// per-pulse EMA adaptation for operators who speed up or slow down mid-message.
type SpeedEstimator struct {
	DotMs      float64 // current dot length estimate
	Farnsworth bool    // sender uses stretched inter-character gaps

	priorMs      float64 // dot length from --wpm hint (0 if none)
	baseAlpha    float64 // normal EMA weight
	alpha        float64 // current EMA weight (boosted during speed changes)
	adaptive     bool
	bootstrapped bool

	// Gap thresholds set by BootstrapGaps; zero means fall back to dot-ratio rules.
	charGapThreshMs float64 // boundary between intra-char and char-gap
	wordGapThreshMs float64 // boundary between char-gap and word-gap
}

// NewEstimator creates a SpeedEstimator.
// wpmHint > 0 sets a prior; set adaptive=true to allow EMA drift after bootstrap.
func NewEstimator(wpmHint float64, adaptive bool) *SpeedEstimator {
	e := &SpeedEstimator{
		baseAlpha: 0.08,
		alpha:     0.08,
		adaptive:  adaptive,
	}
	if wpmHint > 0 {
		e.DotMs = 1200.0 / wpmHint
		e.priorMs = e.DotMs
		e.bootstrapped = true
	}
	return e
}

// IsBootstrapped reports whether the estimator has an initial dot length.
func (e *SpeedEstimator) IsBootstrapped() bool { return e.bootstrapped }

// CurrentWPM returns the estimated speed in words per minute.
func (e *SpeedEstimator) CurrentWPM() float64 {
	if e.DotMs <= 0 {
		return 0
	}
	return 1200.0 / e.DotMs
}

func (e *SpeedEstimator) String() string {
	s := fmt.Sprintf("%.1f WPM (dot=%.1f ms)", e.CurrentWPM(), e.DotMs)
	if e.charGapThreshMs > 0 {
		s += fmt.Sprintf(" | gaps char<%.0fms word<%.0fms", e.charGapThreshMs, e.wordGapThreshMs)
	}
	return s
}

// BootstrapGaps calibrates the char-gap / word-gap boundary from observed
// silence durations. This handles Farnsworth-style recordings where gaps are
// stretched far beyond what dot-ratio rules expect.
//
// Strategy: discard intra-char silences (< 2× dot), then run k-means(k=2) on
// the remaining outer gaps to find the two clusters. The midpoint becomes the
// char/word boundary. If the char-gap cluster itself is >> 3× dot, Farnsworth
// mode is auto-enabled.
func (e *SpeedEstimator) BootstrapGaps(silenceDurationsMs []float64) {
	var outer []float64
	for _, d := range silenceDurationsMs {
		if d > 2.0*e.DotMs {
			outer = append(outer, d)
		}
	}
	if len(outer) < 6 {
		return
	}

	c1, c2 := kmeans2(outer, 0)
	if c1 <= 0 || c2 <= 0 {
		return
	}

	// Only trust the split if the two clusters are meaningfully distinct.
	if c2/c1 < 1.8 {
		return
	}

	// Only calibrate when the smaller cluster already exceeds what the default
	// 5×dot word-gap rule would catch. Below this, c1 represents normal word
	// gaps and c2 represents inter-transmission pauses — the default rule
	// handles them fine and recalibrating would collapse words together.
	if c1 <= 5.0*e.DotMs {
		return
	}

	e.charGapThreshMs = (c1 + c2) / 2
	e.wordGapThreshMs = e.charGapThreshMs

	// Auto-detect Farnsworth: char-gap cluster is well above the standard 3×dot.
	if c1 > 4.0*e.DotMs {
		e.Farnsworth = true
	}
}

// Bootstrap estimates the initial dot length from a slice of tone durations (ms).
// Uses 1D k-means (k=2) to separate dots from dashes, then takes the dot cluster
// center. If a WPM prior exists it is blended in with equal weight.
func (e *SpeedEstimator) Bootstrap(toneDurationsMs []float64) {
	if len(toneDurationsMs) < 8 {
		if e.priorMs > 0 {
			e.bootstrapped = true
		}
		return
	}

	dot, _ := kmeans2(toneDurationsMs, e.priorMs)

	if e.priorMs > 0 {
		// blend: prior constrains bootstrap against noise-only bursts
		e.DotMs = 0.5*dot + 0.5*e.priorMs
	} else {
		e.DotMs = dot
	}
	e.bootstrapped = true
}

// update adjusts the dot estimate via EMA using a newly observed dot-classified tone.
// Only called when adaptive mode is on.
func (e *SpeedEstimator) update(toneMs float64) {
	residual := math.Abs(toneMs-e.DotMs) / e.DotMs
	if residual > 0.30 {
		e.alpha = 0.15 // temporary boost: sender changed speed
	} else {
		e.alpha = e.baseAlpha
	}
	e.DotMs = e.alpha*toneMs + (1-e.alpha)*e.DotMs
}

// SymbolType categorises a Morse pulse.
type SymbolType int

const (
	SymDot SymbolType = iota
	SymDash
	SymIntraGap // silence within a character (between dot/dash elements)
	SymCharGap  // silence between characters
	SymWordGap  // silence between words
)

func (s SymbolType) String() string {
	switch s {
	case SymDot:
		return "."
	case SymDash:
		return "-"
	case SymIntraGap:
		return ""
	case SymCharGap:
		return "|"
	case SymWordGap:
		return " "
	}
	return "?"
}

// Symbol is a classified Morse element with its measured duration.
type Symbol struct {
	Type SymbolType
	Ms   float64
}

// Classify converts a single pulse (tone or silence) into a Symbol and,
// for dot-classified tones in adaptive mode, updates the speed estimate.
func (e *SpeedEstimator) Classify(isTone bool, ms float64) Symbol {
	if isTone {
		return e.classifyTone(ms)
	}
	return e.classifyGap(ms)
}

func (e *SpeedEstimator) classifyTone(ms float64) Symbol {
	d := e.DotMs
	var t SymbolType
	if ms < 2.0*d {
		t = SymDot
		// confident dot: use for adaptive update — require lower bound so
		// noise bursts (short relative to the current estimate) don't pull
		// DotMs down and trigger the noise-amplification feedback loop.
		if e.adaptive && ms >= 0.5*d && ms < 1.8*d {
			e.update(ms)
		}
	} else {
		t = SymDash
	}
	return Symbol{Type: t, Ms: ms}
}

func (e *SpeedEstimator) classifyGap(ms float64) Symbol {
	d := e.DotMs

	// Intra-char silence is always < 2× dot regardless of mode.
	if ms < 2.0*d {
		return Symbol{Type: SymIntraGap, Ms: ms}
	}

	// Use data-driven thresholds when BootstrapGaps has calibrated them.
	if e.charGapThreshMs > 0 {
		if ms < e.charGapThreshMs {
			return Symbol{Type: SymCharGap, Ms: ms}
		}
		return Symbol{Type: SymWordGap, Ms: ms}
	}

	// Fall back to dot-ratio rules.
	// Farnsworth: use 4× instead of 5× so stretched char gaps don't all
	// collapse into word gaps before BootstrapGaps has a chance to calibrate.
	if e.Farnsworth {
		if ms < 4.0*d {
			return Symbol{Type: SymCharGap, Ms: ms}
		}
		return Symbol{Type: SymWordGap, Ms: ms}
	}
	if ms < 5.0*d {
		return Symbol{Type: SymCharGap, Ms: ms}
	}
	return Symbol{Type: SymWordGap, Ms: ms}
}

// kmeans2 partitions data into two clusters and returns (smallerCenter, largerCenter).
// priorMs biases the initial centroid placement when non-zero.
func kmeans2(data []float64, priorMs float64) (float64, float64) {
	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)

	// initialise centroids
	var c1, c2 float64
	if priorMs > 0 {
		c1 = priorMs           // dot cluster near prior
		c2 = priorMs * 3.0    // dash cluster at 3× prior
	} else {
		c1 = sorted[len(sorted)/4]      // 25th percentile → likely dots
		c2 = sorted[3*len(sorted)/4]    // 75th percentile → likely dashes
	}

	for range 50 {
		var sum1, sum2 float64
		var n1, n2 int
		for _, v := range data {
			if math.Abs(v-c1) <= math.Abs(v-c2) {
				sum1 += v
				n1++
			} else {
				sum2 += v
				n2++
			}
		}
		newC1, newC2 := c1, c2
		if n1 > 0 {
			newC1 = sum1 / float64(n1)
		}
		if n2 > 0 {
			newC2 = sum2 / float64(n2)
		}
		if math.Abs(newC1-c1) < 0.01 && math.Abs(newC2-c2) < 0.01 {
			break
		}
		c1, c2 = newC1, newC2
	}

	if c1 < c2 {
		return c1, c2
	}
	return c2, c1
}
