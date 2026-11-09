// engine_quality_test.go measures decode quality against synthetic signals
// with a known transcript. It drives the real Engine (process() for live
// capture, decodeFile() for recordings) and reports errors rather than
// asserting on them, so it's a yardstick for comparing DSP changes.
//
// Skipped by default because it takes a while. To run it:
//
//	MORSE_EVAL=1 go test -v -run TestDecodeQuality -count=1 -timeout 30m .
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"morse-decoder/audio"
)

const (
	evalCarrier  = 700.0
	evalQRMFreq  = 950.0
	evalNextFreq = 480.0 // the station that answers in the handover scenario
	evalPadSec   = 4.0   // noise-only lead-in and tail around each message
	evalRampMs   = 5.0   // key click shaping on each tone edge
)

// evalSNRs are in dB relative to noise in a 2.5 kHz bandwidth, the usual
// convention for weak-signal reporting, so they don't depend on sample rate.
var evalSNRs = []float64{10, 0, -5, -10, -15}

var evalWPMs = []float64{15, 25}

const evalSeeds = 3

var evalCodes = map[rune]string{
	'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'E': ".", 'F': "..-.",
	'G': "--.", 'H': "....", 'I': "..", 'J': ".---", 'K': "-.-", 'L': ".-..",
	'M': "--", 'N': "-.", 'O': "---", 'P': ".--.", 'Q': "--.-", 'R': ".-.",
	'S': "...", 'T': "-", 'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-",
	'Y': "-.--", 'Z': "--..",
	'0': "-----", '1': ".----", '2': "..---", '3': "...--", '4': "....-",
	'5': ".....", '6': "-....", '7': "--...", '8': "---..", '9': "----.",
}

var evalWords = []string{
	"CQ", "DE", "K1ABC", "W7XYZ", "VE3QRP", "G4FON", "DL2MK", "RST", "599", "579",
	"NAME", "BOB", "ANNA", "QTH", "BOSTON", "DENVER", "RIG", "ANT", "DIPOLE",
	"WX", "SUNNY", "COLD", "TNX", "FER", "QSO", "HW", "CPY", "73", "ES", "GL",
	"THE", "AND", "WITH", "VERY", "GOOD", "SIGNAL", "HERE", "POWER", "100W",
}

type evalScenario struct {
	name     string
	jitter   float64 // relative timing error of hand keying, 0 = perfect
	fading   bool
	crashes  bool
	qrm      bool
	handover bool // a second, weaker and faster station follows on another pitch
}

var evalScenarios = []evalScenario{
	{name: "white noise"},
	{name: "fading", fading: true},
	{name: "static crashes", crashes: true},
	{name: "nearby station", qrm: true},
	{name: "hand keying", jitter: 0.2},
	{name: "two stations", handover: true},
}

// evalPath is one way audio enters the decoder.
type evalPath struct {
	name string
	sr   int
	file bool
}

var evalPaths = []evalPath{
	{name: "live 48 kHz (mic)", sr: 48000},
	{name: "live 8 kHz (WebSDR)", sr: 8000},
	{name: "file 48 kHz", sr: 48000, file: true},
}

// evalConfig is a set of user-facing settings, starting from the app defaults.
type evalConfig struct {
	name   string
	auto   bool
	follow bool
}

var evalConfigs = []evalConfig{
	{name: "manual WPM"},
	{name: "auto WPM", auto: true},
	{name: "auto WPM + follow", auto: true, follow: true},
}

func (c evalConfig) filter() FilterConfig {
	return FilterConfig{Type: "Bandpass", Center: int(evalCarrier), Bandwidth: 200, Squelch: 3, NoiseRed: true, Follow: c.follow}
}

// speed uses the true speed in manual mode, the best case for an operator who
// has set the slider correctly. Auto mode starts from the app's default hint.
func (c evalConfig) speed(wpm float64) SpeedConfig {
	if c.auto {
		return SpeedConfig{WPM: 20, Auto: true}
	}
	return SpeedConfig{WPM: int(wpm)}
}

func evalText(r *rand.Rand) string {
	var words []string
	n := 0
	for n < 45 {
		w := evalWords[r.Intn(len(evalWords))]
		words = append(words, w)
		n += len(w) + 1
	}
	return strings.Join(words, " ")
}

// evalKey renders text as a keying envelope in 0..1, surrounded by padSec of
// key-up on each side.
func evalKey(text string, wpm float64, sr int, jitter, padSec float64, r *rand.Rand) []float64 {
	dotSamples := 1.2 / wpm * float64(sr)
	vary := func(units float64) int {
		v := units * dotSamples
		if jitter > 0 {
			f := 1 + jitter*r.NormFloat64()
			f = math.Max(0.5, math.Min(1.5, f))
			v *= f
		}
		return int(v)
	}

	key := make([]float64, int(padSec*float64(sr)))
	emit := func(on bool, n int) {
		v := 0.0
		if on {
			v = 1
		}
		for i := 0; i < n; i++ {
			key = append(key, v)
		}
	}
	for wi, word := range strings.Fields(text) {
		if wi > 0 {
			emit(false, vary(7))
		}
		for ci, ch := range word {
			if ci > 0 {
				emit(false, vary(3))
			}
			for ei, elem := range evalCodes[ch] {
				if ei > 0 {
					emit(false, vary(1))
				}
				if elem == '.' {
					emit(true, vary(1))
				} else {
					emit(true, vary(3))
				}
			}
		}
	}
	emit(false, int(padSec*float64(sr)))

	// A moving average gives each edge a linear ramp instead of a hard click.
	w := int(evalRampMs / 1000 * float64(sr))
	if w < 1 {
		return key
	}
	out := make([]float64, len(key))
	var sum float64
	for i, v := range key {
		sum += v
		if i >= w {
			sum -= key[i-w]
		}
		out[i] = sum / float64(w)
	}
	return out
}

// evalNoiseSigma returns the white-noise standard deviation that puts a
// unit-amplitude tone at snrDB relative to the noise in 2.5 kHz.
func evalNoiseSigma(snrDB float64, sr int) float64 {
	return math.Sqrt(0.5 / math.Pow(10, snrDB/10) * (float64(sr) / 2 / 2500))
}

// evalRender builds the audio for one test case: a unit-amplitude keyed tone
// plus noise and the scenario's impairment, scaled to fit in [-0.8, 0.8].
//
// The returned string is the full transcript, which includes the second
// station when the scenario has one.
func evalRender(sc evalScenario, text string, wpm, snrDB float64, sr int, r *rand.Rand) ([]float32, string) {
	key := evalKey(text, wpm, sr, sc.jitter, evalPadSec, r)
	if !sc.handover {
		return evalMix(sc, key, nil, wpm, snrDB, sr, r), text
	}
	second := evalText(r)
	next := evalKey(second, wpm*1.4, sr, sc.jitter, evalPadSec/2, r)
	first := append(key, make([]float64, len(next))...)
	answer := append(make([]float64, len(key)), next...)
	for i := range answer {
		answer[i] *= 0.5
	}
	return evalMix(sc, first, answer, wpm, snrDB, sr, r), text + " " + second
}

func evalMix(sc evalScenario, key, answer []float64, wpm, snrDB float64, sr int, r *rand.Rand) []float32 {
	fs := float64(sr)
	mix := make([]float64, len(key))
	phase := r.Float64() * 2 * math.Pi
	fadeHz := 0.1 + 0.15*r.Float64()
	fadePhase := r.Float64() * 2 * math.Pi
	const fadeDepthDB = 18.0
	for i, k := range key {
		t := float64(i) / fs
		g := 1.0
		if sc.fading {
			g = math.Pow(10, -fadeDepthDB/20*(1+math.Sin(2*math.Pi*fadeHz*t+fadePhase))/2)
		}
		mix[i] = k * g * math.Sin(2*math.Pi*evalCarrier*t+phase)
		if answer != nil {
			mix[i] += answer[i] * math.Sin(2*math.Pi*evalNextFreq*t+phase)
		}
	}

	sigma := evalNoiseSigma(snrDB, sr)
	for i := range mix {
		mix[i] += sigma * r.NormFloat64()
	}

	if sc.qrm {
		other := evalKey(evalText(r), wpm*0.9, sr, 0, evalPadSec/2, r)
		p := r.Float64() * 2 * math.Pi
		for i := range mix {
			if i >= len(other) {
				break
			}
			t := float64(i) / fs
			mix[i] += other[i] * math.Sin(2*math.Pi*evalQRMFreq*t+p)
		}
	}

	if sc.crashes {
		const crashesPerSec = 1.5
		wideband := math.Sqrt(fs / 2 / 2500) // keeps in-band crash level independent of sr
		i := 0
		for {
			i += int(r.ExpFloat64() / crashesPerSec * fs)
			if i >= len(mix) {
				break
			}
			amp := wideband * math.Pow(10, r.Float64()*26/20) // 0..+26 dB vs the tone, in 2.5 kHz
			tau := (0.010 + 0.020*r.Float64()) * fs
			for j := i; j < len(mix) && float64(j-i) < 6*tau; j++ {
				mix[j] += amp * math.Exp(-float64(j-i)/tau) * r.NormFloat64()
			}
		}
	}

	var peak float64
	for _, v := range mix {
		peak = math.Max(peak, math.Abs(v))
	}
	out := make([]float32, len(mix))
	for i, v := range mix {
		out[i] = float32(v / peak * 0.8)
	}
	return out
}

func evalWriteWAV(path string, samples []float32, sr int) error {
	buf := make([]byte, 44+2*len(samples))
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+2*len(samples)))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:], uint32(sr))
	binary.LittleEndian.PutUint32(buf[28:], uint32(sr*2))
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(2*len(samples)))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(int16(s*32767)))
	}
	return os.WriteFile(path, buf, 0o644)
}

// evalDecode runs samples through the real engine and returns the transcript.
func evalDecode(p evalPath, cfg evalConfig, wpm float64, samples []float32, dir string, id int) (string, error) {
	var text strings.Builder
	e := NewEngine(func(event string, data interface{}) {
		if c, ok := data.(DecodedChunk); ok && event == "decoded" {
			text.WriteString(c.Text)
		}
	})
	if p.file {
		path := filepath.Join(dir, fmt.Sprintf("eval%d.wav", id))
		if err := evalWriteWAV(path, samples, p.sr); err != nil {
			return "", err
		}
		defer os.Remove(path)
		e.running = true
		e.wg.Add(1)
		e.decodeFile(path, cfg.filter(), cfg.speed(wpm))
		return text.String(), nil
	}
	e.initLiveDecoder(cfg.filter(), cfg.speed(wpm), p.sr)
	feed(e, samples)
	e.emitDecoded()
	return text.String(), nil
}

// evalErrors counts edit operations turning ref into hyp. Any operation that
// touches a space is counted under space rather than sub/ins/del, so word
// boundary mistakes can be told apart from wrong characters.
type evalErrors struct {
	refLen, sub, ins, del, space int
}

func (a *evalErrors) add(b evalErrors) {
	a.refLen += b.refLen
	a.sub += b.sub
	a.ins += b.ins
	a.del += b.del
	a.space += b.space
}

func (a evalErrors) cer() float64 {
	if a.refLen == 0 {
		return 0
	}
	return float64(a.sub+a.ins+a.del+a.space) / float64(a.refLen) * 100
}

func (a evalErrors) pct(n int) float64 {
	if a.refLen == 0 {
		return 0
	}
	return float64(n) / float64(a.refLen) * 100
}

func evalNormalize(s string) []rune {
	return []rune(strings.Join(strings.Fields(strings.ToUpper(s)), " "))
}

func evalCompare(refText, hypText string) evalErrors {
	ref, hyp := evalNormalize(refText), evalNormalize(hypText)
	d := make([][]int, len(ref)+1)
	for i := range d {
		d[i] = make([]int, len(hyp)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ref); i++ {
		for j := 1; j <= len(hyp); j++ {
			cost := 1
			if ref[i-1] == hyp[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j-1]+cost, d[i-1][j]+1, d[i][j-1]+1)
		}
	}

	res := evalErrors{refLen: len(ref)}
	for i, j := len(ref), len(hyp); i > 0 || j > 0; {
		switch {
		case i > 0 && j > 0 && ref[i-1] == hyp[j-1] && d[i][j] == d[i-1][j-1]:
			i, j = i-1, j-1
		case i > 0 && j > 0 && d[i][j] == d[i-1][j-1]+1:
			if ref[i-1] == ' ' || hyp[j-1] == ' ' {
				res.space++
			} else {
				res.sub++
			}
			i, j = i-1, j-1
		case i > 0 && d[i][j] == d[i-1][j]+1:
			if ref[i-1] == ' ' {
				res.space++
			} else {
				res.del++
			}
			i--
		default:
			if hyp[j-1] == ' ' {
				res.space++
			} else {
				res.ins++
			}
			j--
		}
	}
	return res
}

func TestEvalCompare(t *testing.T) {
	for _, c := range []struct {
		ref, hyp                string
		sub, ins, del, space, n int
	}{
		{ref: "AB CD", hyp: "AB CD", n: 5},
		{ref: "AB CD", hyp: "AX CD", sub: 1, n: 5},
		{ref: "AB CD", hyp: "ABX CD", ins: 1, n: 5},
		{ref: "AB CD", hyp: "A CD", del: 1, n: 5},
		{ref: "AB CD", hyp: "ABCD", space: 1, n: 5},
		{ref: "ABCD", hyp: "AB CD", space: 1, n: 4},
		{ref: "AB", hyp: "", del: 2, n: 2},
	} {
		got := evalCompare(c.ref, c.hyp)
		want := evalErrors{refLen: c.n, sub: c.sub, ins: c.ins, del: c.del, space: c.space}
		if got != want {
			t.Errorf("evalCompare(%q, %q) = %+v, want %+v", c.ref, c.hyp, got, want)
		}
	}
}

// TestEvalNoiseLevel pins down the SNR formula, since every number in the
// report depends on it: at 0 dB, the white-noise power falling in 2.5 kHz
// must equal the power of a unit-amplitude tone.
func TestEvalNoiseLevel(t *testing.T) {
	for _, sr := range []int{8000, 48000} {
		sigma := evalNoiseSigma(0, sr)
		// At 0 dB, noise power in 2.5 kHz equals the tone's power of 0.5.
		inBand := sigma * sigma * 2500 / (float64(sr) / 2)
		if math.Abs(inBand-0.5) > 1e-9 {
			t.Errorf("sr=%d: noise power in 2.5 kHz = %v, want 0.5", sr, inBand)
		}
	}
}

type evalKeyID struct {
	path, cfg, scenario int
	snr                 float64
}

func TestDecodeQuality(t *testing.T) {
	if os.Getenv("MORSE_EVAL") == "" {
		t.Skip("set MORSE_EVAL=1 to run the decode quality benchmark")
	}
	dir := t.TempDir()

	type job struct {
		id evalKeyID
		fn func(id int) evalErrors
	}
	var jobs []job
	for pi, p := range evalPaths {
		for ci, cfg := range evalConfigs {
			for si, sc := range evalScenarios {
				for _, snr := range evalSNRs {
					for wi, wpm := range evalWPMs {
						for seed := 0; seed < evalSeeds; seed++ {
							// The seed excludes path and config, so every
							// configuration is scored on the same message and noise.
							s := int64(si*100000 + int(snr+50)*1000 + wi*100 + seed)
							p, cfg, sc, snr, wpm := p, cfg, sc, snr, wpm
							jobs = append(jobs, job{id: evalKeyID{pi, ci, si, snr}, fn: func(id int) evalErrors {
								r := rand.New(rand.NewSource(s))
								text := evalText(r)
								samples, ref := evalRender(sc, text, wpm, snr, p.sr, r)
								hyp, err := evalDecode(p, cfg, wpm, samples, dir, id)
								if err != nil {
									t.Error(err)
								}
								return evalCompare(ref, hyp)
							}})
						}
					}
				}
			}
		}
	}

	results := make(map[evalKeyID]*evalErrors)
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				res := jobs[i].fn(i)
				mu.Lock()
				if results[jobs[i].id] == nil {
					results[jobs[i].id] = &evalErrors{}
				}
				results[jobs[i].id].add(res)
				mu.Unlock()
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()

	for pi, p := range evalPaths {
		for ci, cfg := range evalConfigs {
			fmt.Printf("\n## %s, %s: character error rate %%\n", p.name, cfg.name)
			fmt.Printf("%-16s", "SNR (2.5 kHz)")
			for _, snr := range evalSNRs {
				fmt.Printf("%8.0f", snr)
			}
			fmt.Printf("   | at 0 dB:  sub  ins  del  spc\n")
			for si, sc := range evalScenarios {
				fmt.Printf("%-16s", sc.name)
				for _, snr := range evalSNRs {
					fmt.Printf("%8.0f", results[evalKeyID{pi, ci, si, snr}].cer())
				}
				e := *results[evalKeyID{pi, ci, si, 0}]
				fmt.Printf("   |        %5.0f%4.0f%5.0f%5.0f\n", e.pct(e.sub), e.pct(e.ins), e.pct(e.del), e.pct(e.space))
			}
		}
	}

	evalReportGarbage(t, dir)
}

// evalReportGarbage measures output on noise alone, where the only correct
// transcript is an empty one.
func evalReportGarbage(t *testing.T, dir string) {
	const seconds = 120.0
	fmt.Printf("\n## Garbage characters per minute with no signal present\n")
	fmt.Printf("%-22s %-24s %10s %10s\n", "path", "config", "white", "crashes")
	for pi, p := range evalPaths {
		for _, cfg := range evalConfigs {
			var counts [2]int
			var wg sync.WaitGroup
			for i, sc := range []evalScenario{{name: "white"}, {name: "crashes", crashes: true}} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewSource(int64(4242 + i)))
					key := make([]float64, int(seconds*float64(p.sr)))
					samples := evalMix(sc, key, nil, 20, 0, p.sr, r)
					hyp, err := evalDecode(p, cfg, 20, samples, dir, 1000000+pi*10+i)
					if err != nil {
						t.Error(err)
					}
					counts[i] = len([]rune(strings.ReplaceAll(hyp, " ", "")))
				}()
			}
			wg.Wait()
			fmt.Printf("%-22s %-24s %10.0f %10.0f\n", p.name, cfg.name,
				float64(counts[0])/seconds*60, float64(counts[1])/seconds*60)
		}
	}
}

// quietestBand finds the frequency in 400-1000 Hz with the least energy in
// the recording, and returns it with the white-noise variance equivalent to
// the level found there.
func quietestBand(samples []float32, sr int) (hz, variance float64) {
	const block = 4096
	bestPower := math.Inf(1)
	for f := 400.0; f <= 1000; f += 20 {
		var sum float64
		n := 0
		for off := 0; off+block <= len(samples); off += block * 8 {
			m := goertzelMag(samples[off:off+block], f, float64(sr))
			sum += m * m
			n++
		}
		if p := sum / float64(max(n, 1)); p < bestPower {
			bestPower, hz = p, f
		}
	}
	// goertzelMag is normalised by N, so a white-noise bin has m^2 = sigma^2/N.
	return hz, bestPower * block
}

// TestDecodeQualityRealNoise scores decoding against the noise in real
// recordings, adding Morse with a known transcript to the quietest part of the
// band. MORSE_EVAL_NOISE is a comma-separated list of WAV files.
func TestDecodeQualityRealNoise(t *testing.T) {
	list := os.Getenv("MORSE_EVAL_NOISE")
	if list == "" {
		t.Skip("set MORSE_EVAL_NOISE to a comma-separated list of WAV files")
	}
	snrs := []float64{10, 0, -5, -10}
	fmt.Printf("\n## Real noise: character error rate %%\n%-34s %6s", "recording", "Hz")
	for _, snr := range snrs {
		fmt.Printf("%7.0f", snr)
	}
	fmt.Println()
	for _, path := range strings.Split(list, ",") {
		wav, err := audio.LoadWAV(path)
		if err != nil {
			t.Fatal(err)
		}
		noise := make([]float32, len(wav.Samples))
		for i, v := range wav.Samples {
			noise[i] = float32(v)
		}
		sr := wav.SampleRate
		hz, variance := quietestBand(noise, sr)
		cfg := evalConfig{}
		fmt.Printf("%-34.34s %6.0f", filepath.Base(path), hz)
		for _, snr := range snrs {
			var total evalErrors
			for wi, wpm := range evalWPMs {
				for seed := 0; seed < 3; seed++ {
					r := rand.New(rand.NewSource(int64(wi*10 + seed + 77)))
					text := evalText(r)
					key := evalKey(text, wpm, sr, 0, evalPadSec, r)
					amp := math.Sqrt(2 * variance * math.Pow(10, snr/10) * 2500 / (float64(sr) / 2))
					start := r.Intn(len(noise))
					mixed := make([]float32, len(key))
					for i, k := range key {
						ts := float64(i) / float64(sr)
						mixed[i] = noise[(start+i)%len(noise)] + float32(amp*k*math.Sin(2*math.Pi*hz*ts))
					}
					filter := cfg.filter()
					filter.Center = int(hz)
					var out strings.Builder
					e := NewEngine(func(event string, data interface{}) {
						if c, ok := data.(DecodedChunk); ok && event == "decoded" {
							out.WriteString(c.Text)
						}
					})
					e.initLiveDecoder(filter, cfg.speed(wpm), sr)
					feed(e, mixed)
					e.emitDecoded()
					total.add(evalCompare(text, out.String()))
				}
			}
			fmt.Printf("%7.0f", total.cer())
		}
		fmt.Println()
	}
}
