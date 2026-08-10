package main

import "testing"

func TestSnrAboveFloor(t *testing.T) {
	flatNoise := make([]float64, 64)
	for i := range flatNoise {
		flatNoise[i] = 0.1 + 0.02*float64(i%3) // flat-ish noise floor, no dominant peak
	}
	if snrAboveFloor(flatNoise, 0.14, 2.5) {
		t.Error("flat noise floor should not read as signal")
	}

	tone := make([]float64, 64)
	copy(tone, flatNoise)
	tone[10] = 1.0 // one bin standing well above the noise floor
	if !snrAboveFloor(tone, 1.0, 2.5) {
		t.Error("a clear spectral peak should read as signal")
	}

	if snrAboveFloor(tone, 0, 2.5) {
		t.Error("zero peak should never pass")
	}

	// Same peak, but a stricter ratio (higher dB) should reject it.
	if snrAboveFloor(tone, 1.0, dbToRatio(20)) {
		t.Error("a strict ratio should reject a modest peak")
	}
}
