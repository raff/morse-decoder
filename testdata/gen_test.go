// gen_test.go generates a synthetic Morse WAV and runs the full decode pipeline.
package testdata

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"morse-decoder/audio"
	"morse-decoder/dsp"
	"morse-decoder/morse"
)

const (
	sampleRate = 8000
	carrier    = 700.0 // Hz
	wpm        = 20.0
)

func dotMs() float64  { return 1200.0 / wpm }
func dashMs() float64 { return dotMs() * 3 }


// writeTone appends samples for a tone of durationMs at given carrier frequency.
func writeTone(samples *[]float64, durationMs float64, freq float64) {
	n := int(durationMs / 1000.0 * sampleRate)
	offset := len(*samples)
	*samples = append(*samples, make([]float64, n)...)
	for i := range n {
		t := float64(offset+i) / float64(sampleRate)
		(*samples)[offset+i] = math.Sin(2 * math.Pi * freq * t)
	}
}

// writeSilence appends silent samples.
func writeSilence(samples *[]float64, durationMs float64) {
	n := int(durationMs / 1000.0 * sampleRate)
	*samples = append(*samples, make([]float64, n)...)
}

// encodeString generates samples for the given ASCII string in Morse at wpm.
func encodeString(text string) []float64 {
	sequences := map[rune]string{
		'S': "...", 'O': "---", 'E': ".", 'H': "....", 'L': ".-..",
		'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'F': "..-.",
		'G': "--.", 'I': "..", 'J': ".---", 'K': "-.-", 'M': "--",
		'N': "-.", 'P': ".--.", 'Q': "--.-", 'R': ".-.", 'T': "-",
		'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-", 'Y': "-.--",
		'Z': "--..",
	}

	dot := dotMs()
	dash := dashMs()
	var samples []float64

	// leading silence
	writeSilence(&samples, dot*3)

	for wi, word := range splitWords(text) {
		if wi > 0 {
			writeSilence(&samples, dot*7)
		}
		for ci, ch := range word {
			if ci > 0 {
				writeSilence(&samples, dot*3)
			}
			seq, ok := sequences[ch]
			if !ok {
				continue
			}
			for ei, elem := range seq {
				if ei > 0 {
					writeSilence(&samples, dot)
				}
				if elem == '.' {
					writeTone(&samples, dot, carrier)
				} else {
					writeTone(&samples, dash, carrier)
				}
			}
		}
	}

	writeSilence(&samples, dot*7)
	return samples
}

func splitWords(s string) [][]rune {
	var words [][]rune
	var cur []rune
	for _, ch := range s {
		if ch == ' ' {
			if len(cur) > 0 {
				words = append(words, cur)
				cur = nil
			}
		} else {
			cur = append(cur, ch)
		}
	}
	if len(cur) > 0 {
		words = append(words, cur)
	}
	return words
}

func writeWAV(path string, samples []float64, sr int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	numSamples := len(samples)
	dataSize := numSamples * 2 // 16-bit

	write := func(v any) { binary.Write(f, binary.LittleEndian, v) }
	f.Write([]byte("RIFF"))
	write(uint32(36 + dataSize))
	f.Write([]byte("WAVE"))
	f.Write([]byte("fmt "))
	write(uint32(16))
	write(uint16(1)) // PCM
	write(uint16(1)) // mono
	write(uint32(sr))
	write(uint32(sr * 2))
	write(uint16(2))
	write(uint16(16))
	f.Write([]byte("data"))
	write(uint32(dataSize))
	for _, s := range samples {
		v := int16(s * 32767)
		write(v)
	}
	return nil
}

func addNoise(samples []float64, snrDB float64) []float64 {
	// compute signal RMS
	var rms float64
	for _, s := range samples {
		rms += s * s
	}
	rms = math.Sqrt(rms / float64(len(samples)))

	noiseRMS := rms / math.Pow(10, snrDB/20)
	out := make([]float64, len(samples))
	// simple pseudo-random noise (deterministic for reproducibility)
	seed := uint64(12345)
	for i, s := range samples {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		noise := (float64(seed&0xFFFF)/0xFFFF*2 - 1) * noiseRMS * math.Sqrt(2)
		out[i] = s + noise
	}
	return out
}

func runDecode(t *testing.T, samples []float64, sr int, label string) string {
	t.Helper()

	// Same pipeline as main.go
	bp := dsp.NewBandpass(carrier, 150, sr)
	filtered := bp.Apply(samples)
	envelope := dsp.ExtractEnvelope(filtered, sr)
	normalized := dsp.NormalizeEnvelope(envelope)
	schmitt := dsp.NewSchmittTrigger()
	events := schmitt.Process(normalized)
	minSamples := int(10.0 / 1000.0 * float64(sr))
	events = dsp.FilterShortPulses(events, minSamples)

	var toneDurationsMs []float64
	type pulse struct {
		isTone bool
		ms     float64
	}
	pulses := make([]pulse, len(events))
	for i, e := range events {
		ms := float64(e.DurationSamples) / float64(sr) * 1000.0
		pulses[i] = pulse{e.IsTone, ms}
		if e.IsTone {
			toneDurationsMs = append(toneDurationsMs, ms)
		}
	}

	est := morse.NewEstimator(0, true)
	est.Bootstrap(toneDurationsMs)
	if !est.IsBootstrapped() {
		t.Fatalf("%s: bootstrap failed", label)
	}

	dec := &morse.Decoder{}
	for _, p := range pulses {
		dec.Feed(est.Classify(p.isTone, p.ms))
	}
	result := dec.Flush()
	t.Logf("%s: WPM=%.1f decoded=%q", label, est.CurrentWPM(), result)
	return result
}

func TestDecodeClean(t *testing.T) {
	samples := encodeString("SOS HELLO")
	result := runDecode(t, samples, sampleRate, "clean")
	if result != "SOS HELLO" {
		t.Errorf("expected %q, got %q", "SOS HELLO", result)
	}
}

func TestDecodeNoisy20dB(t *testing.T) {
	samples := encodeString("SOS")
	noisy := addNoise(samples, 20) // 20 dB SNR — clearly audible but noisy
	result := runDecode(t, noisy, sampleRate, "noisy-20dB")
	if result != "SOS" {
		t.Errorf("expected %q, got %q", "SOS", result)
	}
}

func TestDecodeNoisy10dB(t *testing.T) {
	samples := encodeString("SOS")
	noisy := addNoise(samples, 10) // 10 dB SNR — significant noise
	result := runDecode(t, noisy, sampleRate, "noisy-10dB")
	// at 10 dB we allow partial matches
	if len(result) == 0 {
		t.Errorf("expected non-empty result, got %q", result)
	}
}

func TestWPMAutoDetect(t *testing.T) {
	for _, speed := range []float64{10, 20, 30} {
		samples := encodeStringAtWPM("SOS", speed)
		bp := dsp.NewBandpass(carrier, 150, sampleRate)
		filtered := bp.Apply(samples)
		envelope := dsp.ExtractEnvelope(filtered, sampleRate)
		normalized := dsp.NormalizeEnvelope(envelope)
		schmitt := dsp.NewSchmittTrigger()
		events := schmitt.Process(normalized)
		events = dsp.FilterShortPulses(events, 10*sampleRate/1000)

		var tones []float64
		for _, e := range events {
			if e.IsTone {
				tones = append(tones, float64(e.DurationSamples)/float64(sampleRate)*1000)
			}
		}
		est := morse.NewEstimator(0, false)
		est.Bootstrap(tones)

		got := est.CurrentWPM()
		if math.Abs(got-speed)/speed > 0.15 {
			t.Errorf("WPM %.0f: estimated %.1f (>15%% error)", speed, got)
		} else {
			t.Logf("WPM %.0f: estimated %.1f ✓", speed, got)
		}
	}
}

func encodeStringAtWPM(text string, targetWPM float64) []float64 {
	// temporarily adjust global wpm constant via closure
	dot := 1200.0 / targetWPM
	dash := dot * 3

	sequences := map[rune]string{
		'S': "...", 'O': "---", 'E': ".", 'H': "....", 'L': ".-..",
		'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'F': "..-.",
		'G': "--.", 'I': "..", 'J': ".---", 'K': "-.-", 'M': "--",
		'N': "-.", 'P': ".--.", 'Q': "--.-", 'R': ".-.", 'T': "-",
		'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-", 'Y': "-.--",
		'Z': "--..",
	}

	var samples []float64
	writeSilence(&samples, dot*3)

	for wi, word := range splitWords(text) {
		if wi > 0 {
			writeSilence(&samples, dot*7)
		}
		for ci, ch := range word {
			if ci > 0 {
				writeSilence(&samples, dot*3)
			}
			seq, ok := sequences[ch]
			if !ok {
				continue
			}
			for ei, elem := range seq {
				if ei > 0 {
					writeSilence(&samples, dot)
				}
				if elem == '.' {
					writeTone(&samples, dot, carrier)
				} else {
					writeTone(&samples, dash, carrier)
				}
			}
		}
	}
	writeSilence(&samples, dot*7)
	return samples
}

// runAutoDetect mirrors the engine's decodePulse gating in Auto mode: it seeds
// the estimator with hintWPM (as the manual WPM slider would before Auto is
// enabled) but relies on Bootstrap — not the hint — to lock in the real speed,
// then classifies every subsequent pulse adaptively. This is a regression test
// for a bug where a wrong hint permanently pinned the estimate because
// NewEstimator marked the estimator "bootstrapped" from the hint alone,
// starving Bootstrap of the chance to run.
func runAutoDetect(t *testing.T, samples []float64, sr int, hintWPM float64, label string) (string, float64) {
	t.Helper()

	bp := dsp.NewBandpass(carrier, 150, sr)
	filtered := bp.Apply(samples)
	envelope := dsp.ExtractEnvelope(filtered, sr)
	normalized := dsp.NormalizeEnvelope(envelope)
	schmitt := dsp.NewSchmittTrigger()
	events := schmitt.Process(normalized)
	minSamples := int(10.0 / 1000.0 * float64(sr))
	events = dsp.FilterShortPulses(events, minSamples)

	est := morse.NewEstimator(hintWPM, true) // adaptive=true mirrors Auto mode
	dec := &morse.Decoder{}
	var toneDurMs []float64
	for _, e := range events {
		ms := float64(e.DurationSamples) / float64(sr) * 1000.0
		if e.IsTone {
			toneDurMs = append(toneDurMs, ms)
		}
		if !est.IsBootstrapped() {
			if len(toneDurMs) >= 8 {
				est.Bootstrap(toneDurMs)
			}
			continue
		}
		dec.Feed(est.Classify(e.IsTone, ms))
	}
	result := dec.Flush()
	t.Logf("%s: hint=%.0f WPM estimated=%.1f WPM decoded=%q", label, hintWPM, est.CurrentWPM(), result)
	return result, est.CurrentWPM()
}

// TestAutoWPMAcrossSpeeds verifies Auto mode converges to the real transmission
// speed — and decodes correctly — across a range of WPMs, all starting from
// the same fixed hint (20 WPM, the app's default manual speed). Before the fix
// this passed only for hint-adjacent speeds; 10 and 30 WPM (2x hint in either
// direction) used to get stuck at 20 WPM forever.
//
// A "VVV VVV" sync preamble precedes the real text, standard CW practice for
// letting a receiver settle on the sender's speed. It also absorbs the live
// decoder's brief resync window (it doesn't feed the decoder until it has
// gathered enough tone samples to re-bootstrap), so the pangram itself decodes
// cleanly rather than losing its first couple of characters to that gap.
func TestAutoWPMAcrossSpeeds(t *testing.T) {
	const text = "THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG"
	const hintWPM = 20.0

	for _, speed := range []float64{10, 15, 20, 25, 30} {
		samples := encodeStringAtWPM("VVV VVV "+text, speed)
		label := fmt.Sprintf("%.0fWPM", speed)
		result, gotWPM := runAutoDetect(t, samples, sampleRate, hintWPM, label)

		if !strings.HasSuffix(result, text) {
			t.Errorf("%s: decoded %q, want suffix %q", label, result, text)
		}
		if errPct := math.Abs(gotWPM-speed) / speed; errPct > 0.15 {
			t.Errorf("%s: estimated %.1f WPM (%.0f%% error, want <=15%%)", label, gotWPM, errPct*100)
		}
	}
}

// TestAutoWPMTracksAcceleratingTransmission models a single continuous QSO
// where the sender speeds up between messages — 10, then 15, then 20, then
// 25, then 30 WPM — and Auto mode is never reset in between, exactly as the
// engine behaves in live capture (one SpeedEstimator and one Decoder persist
// for the whole session; only SetSpeed/Clear rebuild them, and neither
// happens here). It's a deliberately different failure mode from
// TestAutoWPMAcrossSpeeds: instead of one bad initial hint, this checks that
// per-pulse EMA drift (the only mechanism active once bootstrapped) can keep
// tracking a real operator gradually ramping up speed.
//
// Each segment gets a short "VV" lead-in, same reasoning as the sync
// preamble in TestAutoWPMAcrossSpeeds: right at a speed change the gap
// thresholds are still calibrated to the old speed, which can misjudge the
// first character-gap or two. A couple of throwaway dits lets that settle
// before the real text starts, same as an operator wouldn't ramp up speed
// mid-word.
func TestAutoWPMTracksAcceleratingTransmission(t *testing.T) {
	const text = "THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG"
	speeds := []float64{10, 15, 20, 25, 30}

	est := morse.NewEstimator(speeds[0], true) // hint matches the true starting speed
	dec := &morse.Decoder{}
	var toneDurMs []float64

	for _, speed := range speeds {
		samples := encodeStringAtWPM("VV "+text, speed)
		bp := dsp.NewBandpass(carrier, 150, sampleRate)
		filtered := bp.Apply(samples)
		envelope := dsp.ExtractEnvelope(filtered, sampleRate)
		normalized := dsp.NormalizeEnvelope(envelope)
		schmitt := dsp.NewSchmittTrigger()
		events := schmitt.Process(normalized)
		minSamples := int(10.0 / 1000.0 * float64(sampleRate))
		events = dsp.FilterShortPulses(events, minSamples)

		for _, e := range events {
			ms := float64(e.DurationSamples) / float64(sampleRate) * 1000.0
			if e.IsTone {
				toneDurMs = append(toneDurMs, ms)
			}
			if !est.IsBootstrapped() {
				if len(toneDurMs) >= 8 {
					est.Bootstrap(toneDurMs)
				}
				continue
			}
			dec.Feed(est.Classify(e.IsTone, ms))
		}

		gotWPM := est.CurrentWPM()
		t.Logf("end of %.0f WPM segment: estimated=%.1f WPM", speed, gotWPM)
		if errPct := math.Abs(gotWPM-speed) / speed; errPct > 0.20 {
			t.Errorf("end of %.0f WPM segment: estimated %.1f WPM (%.0f%% error, want <=20%%)", speed, gotWPM, errPct*100)
		}
	}

	// The "VV" lead-ins may decode as noise around each segment boundary, so
	// check the pangram appears once per segment, in order, rather than an
	// exact match on the whole (noisy-prefix-included) transcript.
	got := dec.Flush()
	pos := 0
	for i := range speeds {
		idx := strings.Index(got[pos:], text)
		if idx < 0 {
			t.Fatalf("segment %d: %q not found in decoded output after position %d: %q", i, text, pos, got)
		}
		pos += idx + len(text)
	}
}

// TestRoundTrip writes a WAV file and reads it back, verifying the audio stack.
func TestRoundTrip(t *testing.T) {
	samples := encodeString("SOS")
	path := t.TempDir() + "/sos.wav"
	if err := writeWAV(path, samples, sampleRate); err != nil {
		t.Fatal(err)
	}
	wav, err := audio.LoadWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	if wav.SampleRate != sampleRate {
		t.Errorf("sample rate: got %d want %d", wav.SampleRate, sampleRate)
	}
	if len(wav.Samples) != len(samples) {
		t.Errorf("sample count: got %d want %d", len(wav.Samples), len(samples))
	}
	result := runDecode(t, wav.Samples, wav.SampleRate, "round-trip")
	if result != "SOS" {
		t.Errorf("expected %q got %q", "SOS", result)
	}
}
