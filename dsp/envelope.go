package dsp

import (
	"math"
	"sort"
)

// PulseEvent is one continuous period of tone-on or tone-off.
type PulseEvent struct {
	IsTone          bool
	DurationSamples int
}

// ExtractEnvelope rectifies the bandpass-filtered signal and smooths it
// with a lowpass filter, returning the amplitude envelope.
func ExtractEnvelope(samples []float64, sampleRate int) []float64 {
	rectified := make([]float64, len(samples))
	for i, s := range samples {
		rectified[i] = math.Abs(s)
	}
	// Cutoff at 100 Hz: fast enough to track 60 WPM dots (20 ms),
	// slow enough to suppress carrier ripple.
	lp := NewLowpass(100.0, sampleRate)
	return lp.Apply(rectified)
}

// NormalizeEnvelope scales the envelope so the 95th-percentile amplitude
// equals 1.0, clipping outliers. Robust to occasional loud bursts.
func NormalizeEnvelope(envelope []float64) []float64 {
	sorted := make([]float64, len(envelope))
	copy(sorted, envelope)
	sort.Float64s(sorted)

	p95 := sorted[int(0.95*float64(len(sorted)))]
	if p95 < 1e-9 {
		return envelope // silent file
	}

	out := make([]float64, len(envelope))
	for i, v := range envelope {
		n := v / p95
		if n > 1.0 {
			n = 1.0
		}
		out[i] = n
	}
	return out
}

// SchmittTrigger converts a normalized envelope into binary PulseEvents
// using hysteresis to avoid jitter near the threshold boundary.
type SchmittTrigger struct {
	// Rising edge fires when envelope exceeds High.
	// Falling edge fires when envelope drops below Low.
	High float64
	Low  float64

	state bool
	count int
}

func NewSchmittTrigger() *SchmittTrigger {
	return &SchmittTrigger{High: 0.60, Low: 0.35}
}

// Process returns pulse events for the entire envelope slice.
func (s *SchmittTrigger) Process(envelope []float64) []PulseEvent {
	var events []PulseEvent

	emit := func() {
		if s.count > 0 {
			events = append(events, PulseEvent{IsTone: s.state, DurationSamples: s.count})
		}
	}

	for _, v := range envelope {
		next := s.state
		if !s.state && v > s.High {
			next = true
		} else if s.state && v < s.Low {
			next = false
		}

		if next != s.state {
			emit()
			s.state = next
			s.count = 1
		} else {
			s.count++
		}
	}
	emit()
	return events
}

// FilterShortPulses removes pulses shorter than minSamples.
// Used to suppress noise glitches before bootstrap and decoding.
func FilterShortPulses(events []PulseEvent, minSamples int) []PulseEvent {
	out := make([]PulseEvent, 0, len(events))
	for _, e := range events {
		if e.DurationSamples >= minSamples {
			out = append(out, e)
		}
	}
	return out
}
