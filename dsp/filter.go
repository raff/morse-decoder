package dsp

import "math"

// Biquad is a second-order IIR filter (direct form I).
// Coefficients follow the Audio EQ Cookbook (RBJ) convention.
type Biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

func (f *Biquad) tick(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// Apply processes samples through the filter and returns a new slice.
func (f *Biquad) Apply(samples []float64) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = f.tick(s)
	}
	return out
}

// NewBandpass returns a bandpass biquad (constant 0 dB peak gain).
// centerHz: carrier frequency; bwHz: -3 dB bandwidth.
func NewBandpass(centerHz, bwHz float64, sampleRate int) *Biquad {
	w0 := 2 * math.Pi * centerHz / float64(sampleRate)
	q := centerHz / bwHz
	alpha := math.Sin(w0) / (2 * q)
	a0 := 1 + alpha
	return &Biquad{
		b0: (math.Sin(w0) / 2) / a0,
		b1: 0,
		b2: -(math.Sin(w0) / 2) / a0,
		a1: (-2 * math.Cos(w0)) / a0,
		a2: (1 - alpha) / a0,
	}
}

// BandpassChain cascades n identical bandpass biquads for steeper roll-off.
// Each additional stage multiplies the filter order by 2; effective -3 dB
// bandwidth narrows to roughly BW × sqrt(2^(1/n) − 1):
//
//	n=1  → original BW (single 2nd-order section)
//	n=2  → ~0.64 × BW
//	n=3  → ~0.51 × BW
type BandpassChain []*Biquad

// NewBandpassChain builds n cascaded bandpass sections around centerHz.
// bwHz is the declared -3 dB bandwidth of each individual section.
func NewBandpassChain(centerHz, bwHz float64, sampleRate, n int) BandpassChain {
	if n < 1 {
		n = 1
	}
	c := make(BandpassChain, n)
	for i := range n {
		c[i] = NewBandpass(centerHz, bwHz, sampleRate)
	}
	return c
}

// Apply passes samples through every stage in sequence.
func (c BandpassChain) Apply(samples []float64) []float64 {
	out := samples
	for _, b := range c {
		out = b.Apply(out)
	}
	return out
}

// NewLowpass returns a Butterworth lowpass biquad.
func NewLowpass(cutoffHz float64, sampleRate int) *Biquad {
	w0 := 2 * math.Pi * cutoffHz / float64(sampleRate)
	// Q = 1/√2 for maximally flat Butterworth
	alpha := math.Sin(w0) / (2 * (1.0 / math.Sqrt2))
	a0 := 1 + alpha
	cosW := math.Cos(w0)
	return &Biquad{
		b0: ((1 - cosW) / 2) / a0,
		b1: (1 - cosW) / a0,
		b2: ((1 - cosW) / 2) / a0,
		a1: (-2 * cosW) / a0,
		a2: (1 - alpha) / a0,
	}
}
