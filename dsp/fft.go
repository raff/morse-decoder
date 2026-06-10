package dsp

import (
	"math"
	"math/cmplx"
)

// fft performs an in-place iterative Cooley-Tukey FFT.
// len(x) must be a power of two.
func fft(x []complex128) {
	n := len(x)
	if n < 2 {
		return
	}

	// bit-reversal permutation
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}

	// butterfly passes
	for length := 2; length <= n; length <<= 1 {
		half := length >> 1
		angle := -2 * math.Pi / float64(length)
		wBase := complex(math.Cos(angle), math.Sin(angle))
		for i := 0; i < n; i += length {
			w := complex128(1)
			for k := 0; k < half; k++ {
				u := x[i+k]
				v := x[i+k+half] * w
				x[i+k] = u + v
				x[i+k+half] = u - v
				w *= wBase
			}
		}
	}
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// PowerSpectrum returns the one-sided power spectrum of samples.
// Returned slice has length n/2+1, index i → frequency i*sampleRate/n Hz.
func PowerSpectrum(samples []float64) []float64 {
	n := nextPow2(len(samples))
	x := make([]complex128, n)
	for i, v := range samples {
		x[i] = complex(v, 0)
	}
	fft(x)
	out := make([]float64, n/2+1)
	for i := range out {
		out[i] = cmplx.Abs(x[i])
	}
	return out
}

// DetectCarrier finds the dominant frequency in [minHz, maxHz] using FFT.
// Falls back to 700 Hz if the signal is too short or flat.
func DetectCarrier(samples []float64, sampleRate int, minHz, maxHz float64) float64 {
	n := nextPow2(len(samples))
	// cap at 8192 for speed; enough resolution for carrier detection
	if n > 8192 {
		n = 8192
	}
	if n < 64 {
		return 700
	}

	x := make([]complex128, n)
	for i := 0; i < n && i < len(samples); i++ {
		x[i] = complex(samples[i], 0)
	}
	fft(x)

	minBin := int(math.Ceil(minHz * float64(n) / float64(sampleRate)))
	maxBin := int(math.Floor(maxHz * float64(n) / float64(sampleRate)))
	if maxBin >= n/2 {
		maxBin = n/2 - 1
	}
	if minBin < 1 {
		minBin = 1
	}

	bestPower := -1.0
	bestBin := minBin
	for i := minBin; i <= maxBin; i++ {
		p := real(x[i])*real(x[i]) + imag(x[i])*imag(x[i])
		if p > bestPower {
			bestPower = p
			bestBin = i
		}
	}
	return float64(bestBin) * float64(sampleRate) / float64(n)
}
