// engine_wpm_test.go drives the real Engine (not a reimplementation of its
// logic) through process() exactly as live capture does, to reproduce a
// report that Auto mode gets stuck reading ~20 WPM against a 30+ WPM signal:
// characters decode fine but words run together with no spaces, and manual
// 30 WPM decodes spacing correctly. This is a regression test for whatever
// that turns out to be.
package main

import (
	"math"
	"strings"
	"testing"
)

const wpmTestCarrier = 700.0

var wpmTestSequences = map[rune]string{
	'S': "...", 'O': "---", 'E': ".", 'H': "....", 'L': ".-..",
	'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'F': "..-.",
	'G': "--.", 'I': "..", 'J': ".---", 'K': "-.-", 'M': "--",
	'N': "-.", 'P': ".--.", 'Q': "--.-", 'R': ".-.", 'T': "-",
	'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-", 'Y': "-.--",
	'Z': "--..",
}

func wpmTestWriteTone(samples *[]float32, durationMs float64, freq float64, sr int) {
	n := int(durationMs / 1000.0 * float64(sr))
	offset := len(*samples)
	*samples = append(*samples, make([]float32, n)...)
	for i := range n {
		t := float64(offset+i) / float64(sr)
		(*samples)[offset+i] = float32(math.Sin(2 * math.Pi * freq * t))
	}
}

func wpmTestWriteSilence(samples *[]float32, durationMs float64, sr int) {
	n := int(durationMs / 1000.0 * float64(sr))
	*samples = append(*samples, make([]float32, n)...)
}

// encodeAtWPM renders text as Morse audio at targetWPM, sample rate sr.
func encodeAtWPM(text string, targetWPM float64, sr int) []float32 {
	dot := 1200.0 / targetWPM
	dash := dot * 3

	var samples []float32
	for wi, word := range strings.Fields(text) {
		if wi > 0 {
			wpmTestWriteSilence(&samples, dot*7, sr)
		}
		for ci, ch := range word {
			if ci > 0 {
				wpmTestWriteSilence(&samples, dot*3, sr)
			}
			seq, ok := wpmTestSequences[ch]
			if !ok {
				continue
			}
			for ei, elem := range seq {
				if ei > 0 {
					wpmTestWriteSilence(&samples, dot, sr)
				}
				if elem == '.' {
					wpmTestWriteTone(&samples, dot, wpmTestCarrier, sr)
				} else {
					wpmTestWriteTone(&samples, dash, wpmTestCarrier, sr)
				}
			}
		}
	}
	wpmTestWriteSilence(&samples, dot*7, sr)
	return samples
}

// feed pumps samples through the engine framesPerBuffer at a time, exactly
// as the real portaudio capture callback does.
func feed(e *Engine, samples []float32) {
	for len(samples) > 0 {
		n := framesPerBuffer
		if n > len(samples) {
			n = len(samples)
		}
		e.process(samples[:n])
		samples = samples[n:]
	}
}

// TestAutoWPMAfterManualModeMismatch reproduces decoding for a while in fixed
// manual WPM (the app's default: 20 WPM) against a signal that's actually
// running much faster, then flipping Auto on mid-stream — exactly what
// switching to Auto after starting a session does, and different from
// TestAutoWPMAcrossSpeeds/TestAutoWPMTracksAcceleratingTransmission in
// testdata/gen_test.go, which both start the estimator fresh. Here the
// estimator carries over toneDurMs/silDurMs already collected under the
// wrong manual assumption.
func TestAutoWPMAfterManualModeMismatch(t *testing.T) {
	const text = "THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG"
	const actualWPM = 35.0

	var decoded strings.Builder
	var lastWPM int
	emit := func(event string, data interface{}) {
		switch event {
		case "decoded":
			if c, ok := data.(DecodedChunk); ok {
				decoded.WriteString(c.Text)
			}
		case "status":
			if s, ok := data.(Status); ok {
				lastWPM = s.WPM
			}
		}
	}

	e := NewEngine(emit)
	filter := FilterConfig{Type: "Bandpass", Center: 700, Bandwidth: 200, Squelch: 3, NoiseRed: true}
	manualSpeed := SpeedConfig{WPM: 20, Auto: false}
	e.initLiveDecoder(filter, manualSpeed, sampleRate)

	// Phase 1: decoding already running in manual 20 WPM mode against the
	// real 35 WPM signal for a while, as it would before the user notices
	// and switches to Auto.
	feed(e, encodeAtWPM(text+" "+text, actualWPM, sampleRate))
	t.Logf("after manual-mismatch phase: displayed=%d WPM, decoded so far=%q", lastWPM, decoded.String())

	// User presses Auto mid-session — WPM slider still reads 20 at this point.
	e.SetSpeed(SpeedConfig{WPM: 20, Auto: true})

	// Phase 2: same real speed continues, now with Auto engaged.
	feed(e, encodeAtWPM(text, actualWPM, sampleRate))

	// Flush trailing partial state the same way emitDecoded would on the next tick.
	feed(e, encodeAtWPM(text, actualWPM, sampleRate))

	t.Logf("final: displayed=%d WPM, full decoded=%q", lastWPM, decoded.String())

	if errPct := math.Abs(float64(lastWPM)-actualWPM) / actualWPM; errPct > 0.20 {
		t.Errorf("displayed WPM %d, want within 20%% of %.0f", lastWPM, actualWPM)
	}

	full := decoded.String()
	if !strings.Contains(full, text) {
		t.Errorf("post-Auto decode missing a clean copy of %q; got %q", text, full)
	}

	// The specific symptom reported: words run together with no spaces.
	// A decode of the phase-2/3 text with zero spaces means every word
	// boundary was misread as a mere character gap.
	tail := full
	if idx := strings.LastIndex(full, text); idx >= 0 {
		tail = full[idx:]
	}
	if !strings.Contains(tail, " ") {
		t.Errorf("post-Auto decode has no word spaces at all: %q", tail)
	}
}
