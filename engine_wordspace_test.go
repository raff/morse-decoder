// engine_wordspace_test.go is a regression test for NOTES.md #2: the
// inter-word space must show up once the pause is long enough, not only
// once (or if) the next word's first tone eventually arrives.
package main

import (
	"strings"
	"testing"
)

func TestLiveWordSpaceEmittedDuringPause(t *testing.T) {
	var decoded strings.Builder
	emit := func(event string, data interface{}) {
		if event == "decoded" {
			if c, ok := data.(DecodedChunk); ok {
				decoded.WriteString(c.Text)
			}
		}
	}

	e := NewEngine(emit)
	filter := FilterConfig{Type: "Bandpass", Center: 700, Bandwidth: 200, Squelch: 3, NoiseRed: true}
	speed := SpeedConfig{WPM: 20, Auto: false}
	e.initLiveDecoder(filter, speed, sampleRate)

	const wpm = 20.0
	dot := 1200.0 / wpm

	// "SOS" followed by a full word-gap pause and then more silence — but no
	// next word. Nothing here ever completes the pulse via a following tone,
	// so only the live peek (not decodePulse's real completion event) can
	// produce the trailing space.
	samples := encodeAtWPM("SOS", wpm, sampleRate)
	wpmTestWriteSilence(&samples, dot*9, sampleRate)
	feed(e, samples)

	got := decoded.String()
	if !strings.HasSuffix(got, " ") {
		t.Errorf("expected the inter-word space to be emitted during the pause, got %q", got)
	}
	if strings.TrimSpace(got) != "SOS" {
		t.Errorf("expected decoded text to trim to SOS, got %q", got)
	}
}
