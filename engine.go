package main

import (
	"math"
	"strings"
	"sync"

	"github.com/gordonklaus/portaudio"
	"morse-decoder/audio"
	"morse-decoder/dsp"
	"morse-decoder/morse"
)

// Capture settings. Match SampleRate/band to whatever your radio interface and
// decoder expect. specFmin/specFmax must match the constants in main.js.
const (
	sampleRate      = 48000
	framesPerBuffer = 2048
	specFmin        = 300.0
	specFmax        = 1100.0
)

type emitFunc func(event string, data interface{})

// ---- Event payloads (JSON-serialised to the frontend) ----

type SpectrumFrame struct {
	Bins []float64 `json:"bins"` // normalised 0..1, mapped across specFmin..specFmax
}

type DecodedChunk struct {
	Text  string `json:"text"`
	Morse string `json:"morse"`
}

type Status struct {
	Freq    int `json:"freq"`    // detected tone frequency, Hz
	WPM     int `json:"wpm"`     // detected / effective speed
	LevelDB int `json:"levelDb"` // input level
}

// Engine owns the portaudio capture stream and feeds samples into the decoder.
type Engine struct {
	emit emitFunc

	mu           sync.Mutex
	running      bool
	stream       *portaudio.Stream
	wg           sync.WaitGroup
	devices      []*portaudio.DeviceInfo
	selected     *portaudio.DeviceInfo
	selectedName string // the name requested via SetSource; "" means use default
	srcKind      string // "mic" or "file"
	filePath     string

	filter     FilterConfig
	speed      SpeedConfig
	wantClear  bool
	speedDirty bool // set by SetSpeed; applied by process() to avoid racing e.est

	// Live DSP state — owned by the capture goroutine; never touched under mu
	// except during initialisation (before the goroutine starts) or reset.
	// bp is stored as a pointer so SetFilter can replace it with a single atomic
	// write; nil means the filter type is "None" (passthrough).
	bp      *dsp.BandpassChain
	lp      *dsp.Biquad
	schmitt *dsp.SchmittTrigger
	est     *morse.SpeedEstimator
	dec     *morse.Decoder
	agcPeak float64

	// Pulse merger: combines partial pulses split at buffer boundaries.
	liveIsTone bool
	liveMS     float64

	// Tone/silence durations collected for bootstrap and gap calibration.
	toneDurMs []float64
	silDurMs  []float64

	// Tracks how much of dec.Flush()'s output has already been emitted so the
	// frontend (which appends chunks) never receives duplicate text.
	lastEmitLen int
	morseSyms   strings.Builder
	lastMorseLen int
}

func NewEngine(emit emitFunc) *Engine {
	return &Engine{
		emit:    emit,
		srcKind: "mic",
		filter:  FilterConfig{Type: "Bandpass", Center: 700, Bandwidth: 200, Squelch: 3, NoiseRed: true},
		speed:   SpeedConfig{WPM: 20, Auto: false},
	}
}

// Init must be called once before any device calls (from app.startup).
func (e *Engine) Init() error { return portaudio.Initialize() }

// Close stops capture and tears down portaudio (from app.shutdown).
func (e *Engine) Close() {
	e.Stop()
	portaudio.Terminate()
}

func (e *Engine) ListInputDevices() []string {
	devs, err := portaudio.Devices()
	if err != nil {
		e.emit("error", err.Error())
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.devices = e.devices[:0]
	var names []string
	for _, d := range devs {
		if d.MaxInputChannels > 0 {
			e.devices = append(e.devices, d)
			names = append(names, d.Name)
		}
	}
	// Re-resolve a pending device name now that the device list is fresh.
	if e.selectedName != "" {
		e.selected = nil
		for _, d := range e.devices {
			if d.Name == e.selectedName {
				e.selected = d
				break
			}
		}
	}
	return names
}

func (e *Engine) SetSource(kind, device string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.srcKind = kind
	if kind == "file" {
		e.filePath = device
		return
	}
	e.selectedName = device
	e.selected = nil
	for _, d := range e.devices {
		if d.Name == device {
			e.selected = d
			return
		}
	}
}

func (e *Engine) SetFilter(cfg FilterConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.filter = cfg
	// Rebuild the filter chain if the live pipeline is active. The capture
	// goroutine picks up the new pointer on its next buffer (pointer write is
	// atomic on 64-bit; the old chain is simply abandoned).
	if e.lp != nil {
		chain := filterChain(cfg, sampleRate)
		e.bp = &chain
		e.schmitt.High, e.schmitt.Low = schmittThresholds(cfg.Squelch)
	}
}

// filterChain builds a BandpassChain appropriate for the filter config.
// Returns an empty chain (passthrough) when Type is "None".
func filterChain(cfg FilterConfig, sr int) dsp.BandpassChain {
	if cfg.Type == "None" {
		return dsp.BandpassChain{}
	}
	stages := 1
	if cfg.Type == "Narrow CW" {
		stages = 2
	}
	if cfg.NoiseRed {
		stages++
	}
	return dsp.NewBandpassChain(float64(cfg.Center), float64(cfg.Bandwidth), sr, stages)
}

// schmittThresholds maps the squelch knob (0–9) to Schmitt trigger levels.
// squelch=0 is most sensitive (triggers on faint signals),
// squelch=9 requires a strong signal.
func schmittThresholds(squelch int) (high, low float64) {
	frac := float64(squelch) / 9.0
	high = 0.25 + frac*0.55 // 0.25 → 0.80
	low = high * 0.58        // ~same hysteresis ratio as original defaults
	return
}

func (e *Engine) SetSpeed(cfg SpeedConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.speed = cfg
	e.speedDirty = true
}

// Clear signals the capture goroutine to reset decoder state on its next tick.
func (e *Engine) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wantClear = true
}

// initLiveDecoder (re-)initialises the incremental DSP pipeline.
// Called before the capture goroutine starts (lock held) or from within
// process() after reading wantClear (no lock needed there).
func (e *Engine) initLiveDecoder(filter FilterConfig, speed SpeedConfig) {
	chain := filterChain(filter, sampleRate)
	e.bp = &chain
	e.lp = dsp.NewLowpass(100.0, sampleRate)
	e.schmitt = dsp.NewSchmittTrigger()
	e.schmitt.High, e.schmitt.Low = schmittThresholds(filter.Squelch)
	wpmHint := float64(speed.WPM)
	e.est = morse.NewEstimator(wpmHint, speed.Auto || wpmHint == 0)
	e.dec = &morse.Decoder{}
	e.agcPeak = 1e-6
	e.liveIsTone = false
	e.liveMS = 0
	e.lastEmitLen = 0
	e.morseSyms.Reset()
	e.lastMorseLen = 0
	e.toneDurMs = nil
	e.silDurMs = nil
}

// emitDecoded flushes any new text and morse content to the frontend.
// Safe to call speculatively — emits nothing when there is no new content.
func (e *Engine) emitDecoded() {
	full := e.dec.Flush()
	morseFull := e.morseSyms.String()
	text, morseChunk := "", ""
	if len(full) > e.lastEmitLen {
		text = full[e.lastEmitLen:]
		e.lastEmitLen = len(full)
	}
	if len(morseFull) > e.lastMorseLen {
		morseChunk = morseFull[e.lastMorseLen:]
		e.lastMorseLen = len(morseFull)
	}
	if text != "" || morseChunk != "" {
		e.emit("decoded", DecodedChunk{Text: text, Morse: morseChunk})
	}
}

func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}

	if e.srcKind == "file" {
		path := e.filePath
		filter := e.filter
		speed := e.speed
		e.running = true
		e.mu.Unlock()
		e.wg.Add(1)
		go e.decodeFile(path, filter, speed)
		return
	}

	dev := e.selected
	name := e.selectedName
	filter := e.filter
	speed := e.speed
	e.initLiveDecoder(filter, speed)
	e.mu.Unlock()

	if dev == nil {
		if name != "" {
			e.emit("error", "device not available: "+name)
			return
		}
		d, err := portaudio.DefaultInputDevice()
		if err != nil {
			e.emit("error", err.Error())
			return
		}
		dev = d
	}

	params := portaudio.StreamParameters{
		Input: portaudio.StreamDeviceParameters{
			Device:   dev,
			Channels: 1,
			Latency:  dev.DefaultLowInputLatency,
		},
		SampleRate:      sampleRate,
		FramesPerBuffer: framesPerBuffer,
	}

	in := make([]float32, framesPerBuffer)
	stream, err := portaudio.OpenStream(params, in)
	if err != nil {
		e.emit("error", err.Error())
		return
	}
	if err := stream.Start(); err != nil {
		stream.Close()
		e.emit("error", err.Error())
		return
	}

	e.mu.Lock()
	e.running = true
	e.stream = stream
	e.mu.Unlock()

	e.wg.Add(1)
	go e.capture(stream, in)
}

func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	e.mu.Unlock()
	e.wg.Wait()
}

// capture is the live audio loop. The goroutine owns stream teardown.
func (e *Engine) capture(stream *portaudio.Stream, in []float32) {
	defer e.wg.Done()
	defer func() {
		stream.Stop()
		stream.Close()
		e.mu.Lock()
		e.stream = nil
		e.mu.Unlock()
		// Flush any partial word that never got a trailing word-gap.
		if e.dec != nil {
			e.emitDecoded()
		}
	}()

	for {
		e.mu.Lock()
		running := e.running
		e.mu.Unlock()
		if !running {
			return
		}
		if err := stream.Read(); err != nil {
			if err == portaudio.InputOverflowed {
				continue
			}
			e.emit("error", err.Error())
			return
		}
		e.process(in)
	}
}

// process turns one buffer of samples into UI updates.
func (e *Engine) process(in []float32) {
	// Handle deferred Clear() requests.
	e.mu.Lock()
	if e.wantClear {
		filter := e.filter
		speed := e.speed
		e.wantClear = false
		e.speedDirty = false
		e.mu.Unlock()
		e.initLiveDecoder(filter, speed)
	} else if e.speedDirty {
		speed := e.speed
		e.speedDirty = false
		e.mu.Unlock()
		// Rebuild the estimator with the new auto/manual setting.
		// When switching to auto, seed from the current dot-length so the
		// estimate doesn't reset; when switching to manual, use the configured WPM.
		adaptive := speed.Auto || speed.WPM == 0
		var wpmHint float64
		if adaptive && e.est != nil && e.est.IsBootstrapped() && e.est.DotMs > 0 {
			wpmHint = 1200.0 / e.est.DotMs
		} else {
			wpmHint = float64(speed.WPM)
		}
		e.est = morse.NewEstimator(wpmHint, adaptive)
	} else {
		e.mu.Unlock()
	}

	// ── Level and spectrum (Goertzel filterbank for the display) ────────────
	var sum float64
	for _, x := range in {
		sum += float64(x) * float64(x)
	}
	rms := math.Sqrt(sum / float64(len(in)))
	level := 20 * math.Log10(rms+1e-9)

	const bins = 64
	mags := make([]float64, bins)
	var maxMag, peakFreq float64
	for i := 0; i < bins; i++ {
		f := specFmin + (float64(i)+0.5)/bins*(specFmax-specFmin)
		m := goertzelMag(in, f, sampleRate)
		mags[i] = m
		if m > maxMag {
			maxMag, peakFreq = m, f
		}
	}
	normSpec := make([]float64, bins)
	if maxMag > 0 {
		for i, m := range mags {
			normSpec[i] = m / maxMag
		}
	}
	e.emit("spectrum", SpectrumFrame{Bins: normSpec})
	e.emit("status", Status{Freq: int(peakFreq), WPM: e.currentWPM(), LevelDB: int(level)})

	// ── DSP decode pipeline ──────────────────────────────────────────────────
	// Convert to float64 and optionally apply bandpass filter around the carrier.
	f64 := make([]float64, len(in))
	for i, x := range in {
		f64[i] = float64(x)
	}
	var filtered []float64
	if bp := e.bp; bp != nil {
		filtered = bp.Apply(f64)
	} else {
		filtered = f64
	}

	// Rectify + lowpass to extract amplitude envelope.
	for i, s := range filtered {
		f64[i] = math.Abs(s)
	}
	env := e.lp.Apply(f64)

	// Running AGC: track peak with slow exponential decay (~2 s time constant at 48 kHz).
	// decay = exp(-1 / (sampleRate * τ)) = exp(-1 / (48000 * 2)) ≈ 0.999990
	for _, v := range env {
		if v > e.agcPeak {
			e.agcPeak = v
		} else {
			e.agcPeak *= 0.999990
		}
	}
	normEnv := make([]float64, len(env))
	if e.agcPeak > 1e-9 {
		for i, v := range env {
			n := v / e.agcPeak
			if n > 1.0 {
				n = 1.0
			}
			normEnv[i] = n
		}
	}

	// Schmitt trigger → pulse events for this buffer.
	events := e.schmitt.Process(normEnv)

	// Merge pulse events across buffer boundaries and decode completed pulses.
	for _, evt := range events {
		ms := float64(evt.DurationSamples) / float64(sampleRate) * 1000.0
		if e.liveMS > 0 && evt.IsTone == e.liveIsTone {
			// Same polarity as the pending pulse — it was split at the buffer edge.
			e.liveMS += ms
			continue
		}
		if e.liveMS > 0 {
			e.decodePulse(e.liveIsTone, e.liveMS)
		}
		e.liveIsTone = evt.IsTone
		e.liveMS = ms
	}

	// Peek at the pending silence: if it has grown to at least char-gap length,
	// flush the current character to the decoder output and emit now rather than
	// waiting for the next incoming tone to complete the silence pulse.
	// We only peek for char gaps (not word gaps) to avoid accumulating extra
	// spaces in the decoder output on each successive buffer.
	// Feeding SymCharGap is idempotent: once current is empty, flushChar is a no-op.
	if !e.liveIsTone && e.liveMS > 0 && e.est != nil && e.est.IsBootstrapped() {
		if sym := e.est.Classify(false, e.liveMS); sym.Type == morse.SymCharGap {
			e.dec.Feed(sym)
			e.emitDecoded()
		}
	}
}

// maxAutoWPM is the ceiling applied to auto speed detection.
// Anything faster is almost certainly noise rather than a real CW signal.
const maxAutoWPM = 50.0
const minDotMs = 1200.0 / maxAutoWPM // 24 ms

// decodePulse classifies one completed pulse and feeds it to the Morse decoder.
func (e *Engine) decodePulse(isTone bool, ms float64) {
	// ── Noise gate ───────────────────────────────────────────────────────────
	// Drop tone pulses that are implausibly short before they touch the speed
	// estimator. The threshold is the larger of an absolute 10 ms floor and
	// 40 % of the current dot estimate — so it scales with the detected speed
	// and stays effective whether the operator is running at 5 or 50 WPM.
	if isTone {
		threshold := 10.0
		if e.est.IsBootstrapped() {
			if rel := e.est.DotMs * 0.40; rel > threshold {
				threshold = rel
			}
		}
		if ms < threshold {
			return
		}
	}

	const maxDurs = 500 // cap to keep BootstrapGaps O(1) amortised
	if isTone {
		e.toneDurMs = append(e.toneDurMs, ms)
		if len(e.toneDurMs) > maxDurs {
			e.toneDurMs = e.toneDurMs[len(e.toneDurMs)-maxDurs:]
		}
	} else {
		e.silDurMs = append(e.silDurMs, ms)
		if len(e.silDurMs) > maxDurs {
			e.silDurMs = e.silDurMs[len(e.silDurMs)-maxDurs:]
		}
	}

	// Bootstrap speed estimator once enough tone samples are available.
	if !e.est.IsBootstrapped() {
		if len(e.toneDurMs) >= 8 {
			e.est.Bootstrap(e.toneDurMs)
			clampDotMs(e.est)
		}
		return
	}

	// Recalibrate gap thresholds periodically.
	if len(e.silDurMs) > 0 && len(e.silDurMs)%20 == 0 {
		e.est.BootstrapGaps(e.silDurMs)
	}

	sym := e.est.Classify(isTone, ms)
	clampDotMs(e.est) // keep WPM ≤ maxAutoWPM after any adaptive update

	e.morseSyms.WriteString(sym.Type.String())
	e.dec.Feed(sym)

	// Emit on every character or word boundary. The peek in process() may have
	// already fed this char gap and updated lastEmitLen, in which case emitDecoded
	// finds no new text but may still flush a pending morse separator.
	if sym.Type == morse.SymCharGap || sym.Type == morse.SymWordGap {
		e.emitDecoded()
	}
}

// clampDotMs ensures the estimator never drifts above maxAutoWPM.
func clampDotMs(est *morse.SpeedEstimator) {
	if est.DotMs < minDotMs {
		est.DotMs = minDotMs
	}
}

func (e *Engine) currentWPM() int {
	// e.est is only touched from the capture goroutine so no lock needed here.
	if e.est != nil && e.est.IsBootstrapped() {
		return int(e.est.CurrentWPM())
	}
	e.mu.Lock()
	wpm := e.speed.WPM
	e.mu.Unlock()
	if wpm == 0 {
		return 20
	}
	return wpm
}

// goertzelMag returns the magnitude of `freq` in `samples` (normalised by N).
func goertzelMag(samples []float32, freq, sr float64) float64 {
	w := 2 * math.Pi * freq / sr
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for _, x := range samples {
		s0 := float64(x) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	if power < 0 {
		power = 0
	}
	return math.Sqrt(power) / float64(len(samples))
}

// decodeFile runs the batch WAV decode pipeline in a goroutine.
func (e *Engine) decodeFile(path string, filter FilterConfig, speed SpeedConfig) {
	defer e.wg.Done()
	defer e.emit("done", nil)
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	wav, err := audio.LoadWAV(path)
	if err != nil {
		e.emit("error", err.Error())
		return
	}

	centerHz := float64(filter.Center)
	if centerHz <= 0 {
		centerHz = dsp.DetectCarrier(wav.Samples, wav.SampleRate, 400, 2000)
	}

	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if !running {
		return
	}

	// Build the filter chain with the (possibly auto-detected) centre frequency.
	chainCfg := filter
	chainCfg.Center = int(centerHz)
	bp := filterChain(chainCfg, wav.SampleRate)
	filtered := bp.Apply(wav.Samples)

	envelope := dsp.ExtractEnvelope(filtered, wav.SampleRate)
	normalized := dsp.NormalizeEnvelope(envelope)

	e.mu.Lock()
	running = e.running
	e.mu.Unlock()
	if !running {
		return
	}

	schmitt := dsp.NewSchmittTrigger()
	schmitt.High, schmitt.Low = schmittThresholds(filter.Squelch)
	events := schmitt.Process(normalized)
	minSamples := int(10.0 / 1000.0 * float64(wav.SampleRate))
	events = dsp.FilterShortPulses(events, minSamples)

	type pulse struct {
		isTone bool
		ms     float64
	}
	pulses := make([]pulse, len(events))
	var toneDurMs, silDurMs []float64
	for i, ev := range events {
		ms := float64(ev.DurationSamples) / float64(wav.SampleRate) * 1000.0
		pulses[i] = pulse{ev.IsTone, ms}
		if ev.IsTone {
			toneDurMs = append(toneDurMs, ms)
		} else {
			silDurMs = append(silDurMs, ms)
		}
	}

	wpmHint := float64(speed.WPM)
	est := morse.NewEstimator(wpmHint, speed.Auto || wpmHint == 0)
	if !est.IsBootstrapped() {
		est.Bootstrap(toneDurMs)
	}
	if !est.IsBootstrapped() {
		e.emit("error", "could not estimate WPM — try setting a manual WPM")
		return
	}
	est.BootstrapGaps(silDurMs)

	// Signal that decoding is underway: show the detected carrier and initial WPM.
	e.emit("status", Status{Freq: int(centerHz), WPM: int(est.CurrentWPM()), LevelDB: 0})

	// Decode pulse-by-pulse and emit text word-by-word so the display updates
	// progressively rather than all at once at the end.
	dec := &morse.Decoder{}
	var morseSyms strings.Builder
	var lastTextLen, lastMorseLen int

	emitChunk := func() {
		full := dec.Flush()
		morse := morseSyms.String()
		text := ""
		morseChunk := ""
		if len(full) > lastTextLen {
			text = full[lastTextLen:]
			lastTextLen = len(full)
		}
		if len(morse) > lastMorseLen {
			morseChunk = morse[lastMorseLen:]
			lastMorseLen = len(morse)
		}
		if text != "" || morseChunk != "" {
			e.emit("decoded", DecodedChunk{Text: text, Morse: morseChunk})
		}
	}

	for _, p := range pulses {
		sym := est.Classify(p.isTone, p.ms)
		morseSyms.WriteString(sym.Type.String())
		dec.Feed(sym)
		if sym.Type == morse.SymWordGap {
			emitChunk()
		}
	}
	emitChunk() // flush any trailing partial word

	e.emit("status", Status{Freq: int(centerHz), WPM: int(est.CurrentWPM()), LevelDB: 0})
}
