package dsp

import (
	"math"
	"sort"
)

// Internal processing rate and frame layout. Everything below this point is
// independent of the input sample rate.
const (
	detRate   = 4000 // Hz, input is resampled to this
	detHopMs  = 5.0  // one decision per hop
	detBinHz  = 10.0 // spacing of the frequency bins
	detMaxWin = 512  // samples at detRate, must be a power of two
)

// Noise tracking. noiseQ is the quantile each bin's noise estimate follows: a
// low quantile stays on the key-up level of a keyed station, and only a
// carrier that's on most of the time drags it up, which is how steady
// birdies get rejected. noiseQdB converts that quantile to the mean of
// exponentially distributed noise power, -10*log10(-ln(1-noiseQ)).
const (
	noiseQ         = 0.10
	noiseQdB       = 9.77
	noiseStepSlow  = 0.25 // dB per frame
	noiseSettleSec = 1.0  // audio measured directly to initialise the noise floor
	powerFloor     = 1e-7 // -70 dBFS: keeps digital silence from reading as an extreme ratio
	medianOfExp    = math.Ln2

	// A bin this far above the band median is a real tone, not a crash: a
	// strong tone's own key clicks lift the median, and correcting for those
	// would trim its pulses.
	narrowPeakRatio = 40.0
)

// Station presence. Activity is a slow average of each bin's capped
// signal-to-noise ratio. Noise alone averages 1.
const (
	activityTauSec = 1.2
	activityCap    = 30.0
	acquireLevel   = 2.0  // activity needed to start following a bin
	holdLevel      = 1.6  // below this the station is considered gone
	acquireSec     = 0.15 // how long a candidate must persist
	switchSec      = 0.30 // how long a new station must persist to take over
	holdSec        = 1.0  // grace period before going idle
	replaySec      = 3.0  // history decoded when a station is acquired
	driftBins      = 3    // retune this far without calling it a new station
	driftMarginDB  = 0.5  // a neighbouring bin must be this much stronger to retune
	sameBins       = 5    // re-acquiring this close is the same station
)

// Level tracking of the followed station.
const (
	levelRise    = 0.2  // per frame when key-down above the tracked level
	levelFall    = 0.02 // per frame when key-down below it
	levelDecayDB = 0.02 // dB per frame while key-up and active, follows fades
	belowPeakOn  = 5.0  // key-down threshold relative to the tone level, dB
	belowPeakOff = 8.0  // key-up threshold; -6 dB is the true edge of a pulse
	hysteresisDB = 3.0
	debounceDots = 0.2 // runs shorter than this fraction of a dot are glitches
)

const (
	defaultDotMs = 40.0
	windowDots   = 0.85 // shorter than a dot, or one-dot gaps would smear shut
	minWindowMs  = 16.0
	maxWindowMs  = 80.0
)

// DetectorConfig selects which part of the audio band may be followed.
type DetectorConfig struct {
	BandMinHz, BandMaxHz float64 // whole band analysed and used as noise reference
	CenterHz, WidthHz    float64 // allowed range when Follow is false
	Follow               bool    // follow the active station anywhere in the band
	ThresholdDB          float64 // minimum tone-to-noise ratio for key-down
}

// DetectorEvent is either a pulse or, when NewStation is set, a marker that
// following pulses come from a different station at FreqHz.
type DetectorEvent struct {
	Pulse      PulseEvent
	NewStation bool
	FreqHz     float64
}

// Detector turns audio into keying pulses. It measures a narrow spectrogram,
// compares every bin against its own noise floor corrected by the band-wide
// level of each frame (so wideband static cancels out), finds the station
// that's actually keying, and tracks that station's level so fades don't lose
// it. Like SchmittTrigger, the run still in progress at the end of each
// Process call is reported with its cumulative duration.
type Detector struct {
	cfg        DetectorConfig
	sr         int
	sampleStep float64 // input samples per detRate sample
	hop        int     // detRate samples per frame
	frameSec   float64

	antiAlias BandpassChain
	prev      float64
	nextOut   float64
	inCount   float64

	ring    [detMaxWin]float64
	head    int
	sinceFr int

	window    []float64
	windowPow float64
	scratch   []float64
	dotMs     float64
	debounce  int

	bins     []float64 // bin centre frequencies
	coeffs   []float64 // Goertzel coefficients
	noiseDB  []float64
	powerDB  []float64
	activity []float64
	strength []float64 // slow average of each bin's level in dB, finds the exact pitch
	rho      []float64
	sorted   []float64
	frames   int
	seen     int  // detRate samples counted until the first full window
	settled  bool // noise floor has been initialised

	hist     [][]float32 // recent frames, power in dB per bin
	histHead int
	histLen  int

	allowLo, allowHi int

	locked    bool
	bin       int
	lastBin   int
	quiet     int
	candBin   int
	candCount int

	levelDB float64
	state   bool
	count   int
	pending int
	events  []DetectorEvent
}

// NewDetector creates a detector for audio at sampleRate.
func NewDetector(cfg DetectorConfig, sampleRate int) *Detector {
	rate := float64(detRate)
	step := 1.0
	if float64(sampleRate) > rate {
		step = float64(sampleRate) / rate
	} else {
		rate = float64(sampleRate)
	}
	d := &Detector{
		sr:         sampleRate,
		sampleStep: step,
		hop:        int(math.Round(detHopMs / 1000 * rate)),
		lastBin:    -1,
	}
	if step > 1 {
		d.antiAlias = BandpassChain{NewLowpass(rate*0.375, sampleRate), NewLowpass(rate*0.375, sampleRate)}
	}
	d.frameSec = float64(d.hop) / rate

	for f := cfg.BandMinHz; f <= cfg.BandMaxHz+1e-9; f += detBinHz {
		d.bins = append(d.bins, f)
		d.coeffs = append(d.coeffs, 2*math.Cos(2*math.Pi*f/rate))
	}
	n := len(d.bins)
	d.noiseDB = make([]float64, n)
	d.powerDB = make([]float64, n)
	d.activity = make([]float64, n)
	for i := range d.activity {
		d.activity[i] = 1
	}
	d.strength = make([]float64, n)
	d.rho = make([]float64, n)
	d.sorted = make([]float64, n)

	d.hist = make([][]float32, int(replaySec/d.frameSec))
	for i := range d.hist {
		d.hist[i] = make([]float32, n)
	}

	d.Configure(cfg)
	d.setWindow(defaultDotMs, rate)
	return d
}

func (d *Detector) rate() float64 {
	return float64(d.sr) / d.sampleStep
}

// Configure changes the followed range and sensitivity without losing the
// noise estimates or history. If the followed station falls outside the new
// range it's dropped, and the next one in range is acquired normally.
func (d *Detector) Configure(cfg DetectorConfig) {
	d.cfg = cfg
	d.allowLo, d.allowHi = 0, len(d.bins)-1
	if !cfg.Follow {
		d.allowLo = d.binFor(cfg.CenterHz - cfg.WidthHz/2)
		d.allowHi = d.binFor(cfg.CenterHz + cfg.WidthHz/2)
	}
	if d.locked && (d.bin < d.allowLo || d.bin > d.allowHi) {
		d.locked = false
		d.candCount = 0
	}
}

func (d *Detector) binFor(hz float64) int {
	i := int(math.Round((hz - d.cfg.BandMinHz) / detBinHz))
	return max(0, min(i, len(d.bins)-1))
}

// SetDotMs matches the integration window to the current element length,
// which is where most of the sensitivity comes from. Small changes are
// ignored so the window isn't rebuilt on every adaptive speed update. A
// non-positive dotMs means the speed is unknown again.
func (d *Detector) SetDotMs(dotMs float64) {
	if dotMs <= 0 {
		dotMs = defaultDotMs
	}
	if math.Abs(dotMs-d.dotMs)/d.dotMs < 0.15 {
		return
	}
	d.setWindow(dotMs, d.rate())
}

func (d *Detector) setWindow(dotMs, rate float64) {
	d.dotMs = dotMs
	winMs := math.Max(minWindowMs, math.Min(maxWindowMs, dotMs*windowDots))
	w := min(int(winMs/1000*rate), detMaxWin)
	// A tone's ratio to noise grows with window length. Keep the tracked
	// level consistent, or a shorter window leaves it too high and pulses
	// get trimmed, which reads as a faster speed and shortens the window again.
	if old := len(d.window); old > 0 {
		d.levelDB += 10 * math.Log10(float64(w)/float64(old))
	}
	d.window = make([]float64, w)
	d.scratch = make([]float64, w)
	d.windowPow = 0
	for i := range d.window {
		v := 0.5 - 0.5*math.Cos(2*math.Pi*(float64(i)+0.5)/float64(w))
		d.window[i] = v
		d.windowPow += v * v
	}
	d.debounce = max(1, int(math.Round(debounceDots*dotMs/(d.frameSec*1000))))
}

// FreqHz returns the frequency of the followed station, or 0 when idle.
func (d *Detector) FreqHz() float64 {
	if !d.locked {
		return 0
	}
	return d.bins[d.bin]
}

// Process consumes samples and returns the resulting events. The returned
// slice is only valid until the next call.
func (d *Detector) Process(samples []float64) []DetectorEvent {
	d.events = d.events[:0]
	filtered := samples
	if len(d.antiAlias) > 0 {
		filtered = d.antiAlias.Apply(samples)
	}
	for _, s := range filtered {
		for d.nextOut <= d.inCount {
			frac := 1 - (d.inCount - d.nextOut)
			d.push(d.prev + frac*(s-d.prev))
			d.nextOut += d.sampleStep
		}
		d.prev = s
		d.inCount++
	}
	if run := d.count - d.pending; run > 0 {
		d.emitRun(d.state, run)
	}
	return d.events
}

func (d *Detector) push(x float64) {
	d.ring[d.head&(detMaxWin-1)] = x
	d.head++
	d.sinceFr++
	if d.sinceFr >= d.hop {
		d.sinceFr = 0
		d.frame()
	}
}

func (d *Detector) frame() {
	d.measure()
	d.updateStation()
	if d.locked {
		d.decide(10 * math.Log10(d.rho[d.bin]+1e-12))
	} else {
		d.decide(math.Inf(-1))
	}
}

// measure updates rho, each bin's power relative to its noise floor and to
// the band-wide level of this frame.
func (d *Detector) measure() {
	w := len(d.window)
	for i, v := range d.window {
		d.scratch[i] = v * d.ring[(d.head-w+i)&(detMaxWin-1)]
	}

	// Frames before the ring first fills are partly zeros, so skip them.
	if d.seen < w {
		d.seen += d.hop
		return
	}

	settling := float64(d.frames)*d.frameSec < noiseSettleSec
	if !settling && !d.settled {
		d.initNoiseFloor()
		d.settled = true
	}
	for b, coeff := range d.coeffs {
		var s1, s2 float64
		for _, x := range d.scratch {
			s0 := x + coeff*s1 - s2
			s2, s1 = s1, s0
		}
		// Normalised so white noise reads the same at any window length.
		p := (s1*s1 + s2*s2 - coeff*s1*s2) / d.windowPow
		db := 10 * math.Log10(math.Max(p, 0)+powerFloor)
		d.powerDB[b] = db
		if settling {
			d.rho[b] = 1
			continue
		}
		if db > d.noiseDB[b] {
			d.noiseDB[b] += noiseStepSlow * noiseQ
		} else {
			d.noiseDB[b] -= noiseStepSlow * (1 - noiseQ)
		}
		d.rho[b] = math.Pow(10, (db-d.noiseDB[b]-noiseQdB)/10)
	}
	d.frames++

	// A wideband crash lifts every bin alike, while a station only lifts a
	// few, so the median measures the crash and leaves stations alone.
	copy(d.sorted, d.rho)
	sort.Float64s(d.sorted)
	band := d.sorted[len(d.sorted)/2] / medianOfExp
	bandDB := 0.0
	if band > 1 && d.sorted[len(d.sorted)-1] < narrowPeakRatio*band {
		bandDB = 10 * math.Log10(band)
		for b := range d.rho {
			d.rho[b] /= band
		}
	}

	alpha := 1 - math.Exp(-d.frameSec/activityTauSec)
	row := d.hist[d.histHead]
	for b, r := range d.rho {
		// Noise estimates are unreliable until settled and would read as
		// activity. The history keeps those frames for replay either way.
		if !settling {
			d.activity[b] += alpha * (math.Min(r, activityCap) - d.activity[b])
			d.strength[b] += alpha * (10*math.Log10(r+1e-12) - d.strength[b])
		}
		// Stored against the noise floor at replay time, not this frame's:
		// early frames were measured before that floor had settled.
		row[b] = float32(d.powerDB[b] - bandDB)
	}
	d.histHead = (d.histHead + 1) % len(d.hist)
	if d.histLen < len(d.hist) {
		d.histLen++
	}
}

func (d *Detector) frames4(sec float64) int {
	return int(math.Ceil(sec / d.frameSec))
}

func (d *Detector) updateStation() {
	best := d.allowLo
	for b := d.allowLo; b <= d.allowHi; b++ {
		if d.activity[b] > d.activity[best] {
			best = b
		}
	}

	if !d.locked {
		if d.candidate(best, acquireSec) {
			d.acquire(d.refine(best, sameBins))
		}
		return
	}

	// Follow slow drift of the current station.
	if b := d.refine(d.bin, driftBins); d.strength[b] > d.strength[d.bin]+driftMarginDB {
		d.bin = b
	}
	d.lastBin = d.bin

	if d.activity[d.bin] < holdLevel {
		d.quiet++
	} else {
		d.quiet = 0
	}

	// Only take over once the current station is fading, so a louder
	// neighbour can't pull the decoder away mid-transmission.
	far := best < d.bin-sameBins || best > d.bin+sameBins
	if far && d.activity[d.bin] < acquireLevel && d.candidate(best, switchSec) {
		d.acquire(d.refine(best, sameBins))
		return
	}
	if !far {
		d.candCount = 0
	}
	if d.quiet >= d.frames4(holdSec) {
		d.locked = false
		d.candCount = 0
	}
}

// refine returns the strongest allowed bin within reach of bin. Activity is
// capped, so on a strong signal several neighbours tie and it can't tell them apart.
func (d *Detector) refine(bin, reach int) int {
	best := bin
	for b := max(d.allowLo, bin-reach); b <= min(d.allowHi, bin+reach); b++ {
		if d.strength[b] > d.strength[best] {
			best = b
		}
	}
	return best
}

// candidate reports whether bin has held enough activity for long enough.
func (d *Detector) candidate(bin int, sec float64) bool {
	if d.activity[bin] < acquireLevel {
		d.candCount = 0
		return false
	}
	if d.candCount > 0 && abs(bin-d.candBin) <= 2 {
		d.candCount++
	} else {
		d.candBin, d.candCount = bin, 1
	}
	return d.candCount >= d.frames4(sec)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// acquire starts following bin and decodes the recent history, so the
// characters sent while the station was being recognised aren't lost.
func (d *Detector) acquire(bin int) {
	if d.lastBin < 0 || abs(bin-d.lastBin) > sameBins {
		d.finishRun()
		d.events = append(d.events, DetectorEvent{NewStation: true, FreqHz: d.bins[bin]})
	}
	d.locked = true
	d.bin = bin
	d.lastBin = bin
	d.quiet = 0
	d.candCount = 0

	// Start the level near the top of the history, which is the station's
	// key-down level, so noise from before it appeared doesn't decode.
	levels := make([]float64, 0, d.histLen)
	for i := 0; i < d.histLen; i++ {
		levels = append(levels, d.histRhoDB(i, bin))
	}
	sort.Float64s(levels)
	d.levelDB = levels[len(levels)*98/100]

	// Replay from where the station first keys down, skipping the noise
	// before it, which would otherwise train the speed estimate on junk. The
	// newest row is decoded by the caller right after this returns.
	start := max(d.cfg.ThresholdDB, d.levelDB-belowPeakOn)
	i := 0
	for i < d.histLen-1 && d.histRhoDB(i, bin) < start {
		i++
	}
	for ; i < d.histLen-1; i++ {
		d.decide(d.histRhoDB(i, bin))
	}
}

// initNoiseFloor sets each bin's noise estimate from the stored frames. Doing
// this directly is exact, where a tracker would need a large step to converge
// quickly and then be too jittery to trust right as detection starts.
func (d *Detector) initNoiseFloor() {
	values := make([]float64, d.histLen)
	for b := range d.noiseDB {
		for i := range values {
			values[i] = float64(d.histRow(i)[b])
		}
		sort.Float64s(values)
		d.noiseDB[b] = values[int(noiseQ*float64(len(values)))]
	}
}

// histRhoDB returns the tone-to-noise ratio of bin in the i-th oldest frame.
func (d *Detector) histRhoDB(i, bin int) float64 {
	return float64(d.histRow(i)[bin]) - d.noiseDB[bin] - noiseQdB
}

// histRow returns the i-th oldest stored frame.
func (d *Detector) histRow(i int) []float32 {
	start := d.histHead - d.histLen
	if start < 0 {
		start += len(d.hist)
	}
	return d.hist[(start+i)%len(d.hist)]
}

func (d *Detector) finishRun() {
	if d.count > 0 {
		d.emitRun(d.state, d.count)
	}
	d.state, d.count, d.pending = false, 0, 0
}

func (d *Detector) emitRun(tone bool, frames int) {
	samples := int(math.Round(float64(frames) * d.frameSec * float64(d.sr)))
	d.events = append(d.events, DetectorEvent{Pulse: PulseEvent{IsTone: tone, DurationSamples: samples}})
}

// decide advances the key state by one frame. A change only takes effect once
// it has lasted d.debounce frames, and is then back-dated to when it began.
func (d *Detector) decide(rhoDB float64) {
	on := math.Max(d.cfg.ThresholdDB, d.levelDB-belowPeakOn)
	off := math.Max(d.cfg.ThresholdDB-hysteresisDB, d.levelDB-belowPeakOff)

	next := d.state
	if !d.state && rhoDB > on {
		next = true
	} else if d.state && rhoDB < off {
		next = false
	}

	if next || rhoDB > on {
		rate := levelFall
		if rhoDB > d.levelDB {
			rate = levelRise
		}
		d.levelDB += rate * (rhoDB - d.levelDB)
	} else if d.locked && d.activity[d.bin] >= acquireLevel {
		// Fall only while the station is still sending, so a stale level
		// isn't lowered into the noise after it stops.
		d.levelDB = math.Max(d.levelDB-levelDecayDB, d.cfg.ThresholdDB)
	}

	d.count++
	if next == d.state {
		d.pending = 0
		return
	}
	d.pending++
	if d.pending >= d.debounce {
		if run := d.count - d.pending; run > 0 {
			d.emitRun(d.state, run)
		}
		d.state = next
		d.count = d.pending
		d.pending = 0
	}
}
