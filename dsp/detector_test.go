package dsp

import (
	"math"
	"math/rand"
	"testing"
)

const detTestSR = 8000

func detCfg(follow bool, center float64) DetectorConfig {
	return DetectorConfig{BandMinHz: 300, BandMaxHz: 1100, CenterHz: center, WidthHz: 130, Follow: follow, ThresholdDB: 6}
}

// detKeyed returns a 50 ms on / 50 ms off tone at amp lasting sec.
func detKeyed(freq, amp, sec float64) []float64 {
	out := make([]float64, int(sec*detTestSR))
	for i := range out {
		t := float64(i) / detTestSR
		if math.Mod(t, 0.1) < 0.05 {
			out[i] = amp * math.Sin(2*math.Pi*freq*t)
		}
	}
	return out
}

func detAddNoise(x []float64, sigma float64, r *rand.Rand) {
	for i := range x {
		x[i] += sigma * r.NormFloat64()
	}
}

// detRun feeds x in buffers and returns the tone pulses and new-station
// frequencies. Runs split across buffers are merged the way the engine does.
func detRun(d *Detector, x []float64) (tones []float64, stations []float64) {
	liveTone, liveMS := false, 0.0
	flush := func() {
		if liveMS > 0 && liveTone {
			tones = append(tones, liveMS)
		}
		liveMS = 0
	}
	for len(x) > 0 {
		n := min(512, len(x))
		for _, ev := range d.Process(x[:n]) {
			if ev.NewStation {
				flush()
				stations = append(stations, ev.FreqHz)
				continue
			}
			ms := float64(ev.Pulse.DurationSamples) / detTestSR * 1000
			if liveMS > 0 && ev.Pulse.IsTone != liveTone {
				flush()
			}
			liveTone, liveMS = ev.Pulse.IsTone, ms
		}
		x = x[n:]
	}
	flush()
	return tones, stations
}

func TestDetectorKeyedToneInNoise(t *testing.T) {
	d := NewDetector(detCfg(false, 700), detTestSR)
	x := detKeyed(700, 1, 6)
	detAddNoise(x, 0.5, rand.New(rand.NewSource(1)))
	tones, _ := detRun(d, x)
	if len(tones) < 50 || len(tones) > 65 {
		t.Fatalf("got %d tones, want about 60", len(tones))
	}
	var sum float64
	for _, ms := range tones {
		sum += ms
	}
	if mean := sum / float64(len(tones)); math.Abs(mean-50) > 10 {
		t.Errorf("mean tone %.1f ms, want about 50", mean)
	}
}

func TestDetectorIgnoresWhiteNoise(t *testing.T) {
	d := NewDetector(detCfg(true, 700), detTestSR)
	x := make([]float64, 30*detTestSR)
	detAddNoise(x, 0.5, rand.New(rand.NewSource(2)))
	if tones, _ := detRun(d, x); len(tones) > 3 {
		t.Errorf("noise alone produced %d tones", len(tones))
	}
}

func TestDetectorIgnoresStaticCrashes(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	d := NewDetector(detCfg(true, 700), detTestSR)
	x := make([]float64, 30*detTestSR)
	detAddNoise(x, 0.1, r)
	for i := 0; i < len(x); i += 3000 + r.Intn(5000) {
		for j := i; j < len(x) && j < i+400; j++ {
			x[j] += 3 * math.Exp(-float64(j-i)/100) * r.NormFloat64()
		}
	}
	if tones, _ := detRun(d, x); len(tones) > 3 {
		t.Errorf("static crashes produced %d tones", len(tones))
	}
}

func TestDetectorIgnoresSteadyCarrier(t *testing.T) {
	d := NewDetector(detCfg(true, 700), detTestSR)
	x := make([]float64, 30*detTestSR)
	for i := range x {
		x[i] = math.Sin(2 * math.Pi * 600 * float64(i) / detTestSR)
	}
	detAddNoise(x, 0.1, rand.New(rand.NewSource(4)))
	// Allow the noise floor time to absorb the carrier, then expect silence.
	settle := 8 * detTestSR
	detRun(d, x[:settle])
	if tones, _ := detRun(d, x[settle:]); len(tones) > 3 {
		t.Errorf("steady carrier produced %d tones after settling", len(tones))
	}
}

func TestDetectorFollowsNextStation(t *testing.T) {
	first := detKeyed(770, 1, 8)
	gap := make([]float64, detTestSR)
	second := detKeyed(480, 0.5, 8)
	x := append(append(first, gap...), second...)
	detAddNoise(x, 0.2, rand.New(rand.NewSource(5)))

	d := NewDetector(detCfg(true, 700), detTestSR)
	_, stations := detRun(d, x)
	if len(stations) != 2 {
		t.Fatalf("stations = %v, want two", stations)
	}
	if math.Abs(stations[0]-770) > 20 || math.Abs(stations[1]-480) > 20 {
		t.Errorf("stations = %v, want about 770 then 480", stations)
	}
}

func TestDetectorStaysOffStationOutsideRange(t *testing.T) {
	x := detKeyed(480, 1, 6)
	detAddNoise(x, 0.2, rand.New(rand.NewSource(6)))
	d := NewDetector(detCfg(false, 770), detTestSR)
	if tones, _ := detRun(d, x); len(tones) > 3 {
		t.Errorf("station outside the passband produced %d tones", len(tones))
	}
}

func TestDetectorKeepsCurrentStationDuringOverlap(t *testing.T) {
	x := detKeyed(700, 1, 10)
	louder := detKeyed(900, 3, 10)
	for i := 3 * detTestSR; i < len(x); i++ {
		x[i] += louder[i]
	}
	detAddNoise(x, 0.2, rand.New(rand.NewSource(7)))
	d := NewDetector(detCfg(true, 700), detTestSR)
	if _, stations := detRun(d, x); len(stations) != 1 {
		t.Errorf("stations = %v, want to stay on the first", stations)
	}
}
