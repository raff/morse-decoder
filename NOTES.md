# Implementation notes and review items

Working notes on the current implementation. Items marked **[review]** are
things that need a closer look before considering the feature complete.

---

## Architecture

The app has two decode paths that share the DSP packages but differ in how
they consume audio:

- **File mode**: `decodeFile()` runs the full pipeline in one goroutine,
  emitting `decoded` chunks word-by-word as it goes and a `done` event on exit.
- **Live mode**: `capture()` feeds 2048-sample buffers into `process()` every
  ~42 ms. Each buffer applies the filter chain, extracts the envelope, runs the
  Schmitt trigger, and merges partial pulses across buffer boundaries.

Both paths share `audio/`, `dsp/`, and `morse/` packages that were originally
written as a standalone CLI (`morse-decoder-engine`).

---

## Known issues

### 1. Char-gap peek feeds the decoder every buffer **[review]**

In `process()`, at the end of each buffer, if the pending silence has grown
past char-gap length, `SymCharGap` is fed to the decoder to flush the current
character immediately rather than waiting for the next tone.

This is **idempotent** (once `current` is empty, `flushChar` is a no-op), but
it fires on every single buffer for the entire duration of any silence that
exceeds `2 × dotMs`. This creates noise in the internal decoder state and
wastes CPU.

Fix: add a `charGapPeeked bool` flag that is set on the first peek for a
given silence run and reset when a new tone starts. That way the peek fires
exactly once per silence transition.

### 2. Word space only appears when the next word starts

A word gap is only decoded (and the space emitted) when the silence
*completes* — i.e. when the next tone begins and `decodePulse` is called.
During a long inter-word pause (or at end of transmission), the space isn't
shown until either the next word starts or the user clicks Stop (which
triggers the final-flush in `capture()`'s defer).

This is mostly harmless in practice, but it means text like "SOS DE W1AW"
shows "SOS DE W1AW" all at once when the next character starts, not
progressively with the inter-word space appearing in real-time.

Fix: extend the char-gap peek to also fire for `SymWordGap`, but only once
(use the same flag as item 1 above). Handling the extra decoder space cleanly
requires either tracking `lastEmitLen` against the un-trimmed output or adding
a `Peek()` method to `Decoder` that returns current output without flushing.

### 3. AGC toggle not implemented

The AGC on/off toggle in the Filters popover is stored in `FilterConfig.AGC`
and forwarded to `SetFilter`, but `engine.go` ignores it — the running-peak
tracker (`agcPeak`) always runs.

What "AGC off" should mean in practice is unclear without a calibrated input
level. Options:
- Fix the normalisation denominator to a constant representing the expected
  peak signal level (requires knowing the gain chain).
- Disable the decay and only update the peak upward — effectively an
  "auto-level set once, then hold" mode.

For now this toggle does nothing. **[review]** Decide what the expected
behaviour is and wire it up.

### 4. Auto-notch filter not implemented

Selecting "Auto-notch" in the filter type dropdown falls through to
single-stage Bandpass behaviour in `filterChain()`. The intent would be to
detect an interfering carrier nearby and null it with a notch filter while
keeping the target carrier intact.

This is non-trivial to implement robustly. For now it silently degrades to
Bandpass. **[review]** Either implement or remove from the dropdown.

### 5. No live carrier tracking

The bandpass filter is tuned to `filter.Center` (set by the UI slider, default
700 Hz) and never updated automatically during live capture. The spectrum
display shows the Goertzel peak frequency, which may not match the filter
center — if the user's signal is at 800 Hz but the slider is at 700 Hz, the
filter is 100 Hz off and decoding quality degrades.

In file mode this is handled by `dsp.DetectCarrier()` (FFT on the whole file
before filtering). For live mode there's no equivalent.

Fix options:
- Auto-update `filter.Center` based on the Goertzel peak frequency, perhaps
  with a slow EMA to avoid chasing noise.
- Add a "Lock to detected" button in the UI.
- Display a visual warning when the spectrum peak and filter center diverge
  significantly.

### 6. Bootstrap delay at start of live session

With Auto WPM enabled and WPM hint = 0 (or the estimator not yet
bootstrapped), `decodePulse` collects the first 8 tones before calling
`Bootstrap()`. These tones are consumed for speed estimation and never decoded.
At 20 WPM, 8 tones is roughly the first 2–3 characters.

With a manual WPM set (even the default of 20), the estimator is pre-seeded
and bootstrap is immediate — no characters are lost.

The practical takeaway: **always set a manual WPM if you know the operator's
approximate speed**.

### 7. Speed estimator EMA α-boost can be triggered by noise

When a tone's duration deviates from `DotMs` by more than 30%, the adaptive
EMA raises its learning rate to α = 0.25 (the "speed change" response). A
single noise burst that slips through the noise gate and is classified as a
short dot triggers this, potentially moving `DotMs` noticeably in one step,
even though the noise gate and 50 WPM cap limit the damage.

A stricter guard: only boost α if the *last N* consecutive dots all show the
same directional deviation, indicating a genuine speed change rather than a
one-off outlier.

### 8. `toneDurMs` / `silDurMs` not a true sliding window

The slices are capped at 500 entries, but `BootstrapGaps` is always called
with the full cap. If an operator changes speed mid-session, old duration data
from a different WPM remains in the buffer and pollutes the k-means until it
rotates out (500 silences × ~200 ms average ≈ 100 s). A proper sliding window
(e.g. last 30 s of data, or weighted by recency) would respond faster.

### 9. `wails dev` may still lack the microphone permission key

`build/darwin/Info.plist` (used by `wails build`) has been updated with
`NSMicrophoneUsageDescription`. The equivalent file for `wails dev` is
`build/darwin/Info.dev.plist` (generated by Wails if it exists). If mic capture
fails only in dev mode but works in a built `.app`, add the same key there.

### 10. Silence noise pulses not filtered

The noise gate in `decodePulse` drops very short *tone* pulses before they
reach the estimator. Short *silence* pulses (noise spikes alternating rapidly)
are not filtered — they pass through as `SymIntraGap` or `SymCharGap` and can
disrupt character boundaries.

Fix: apply the same minimum-duration check to silences, dropping any silence
shorter than, say, `0.2 × dotMs` before feeding it to the decoder.

### 11. Mic/USB device disconnect mid-capture cannot be auto-detected safely **[review]**

Turning off (or unplugging) a USB radio while `capture()` is actively
reading from it used to hang the app (the original bug report). Several
rounds of fixing this — read-timeout watchdogs, `stream.Abort()` to unblock
a wedged `Read()`, silence/overflow heuristics — were tried and **all
reverted**, because they led to real, reproducible app crashes (SIGSEGV)
instead of just a hang. `capture()` is now back to the simple form: a plain
loop that calls `stream.Read()` and retries on `portaudio.InputOverflowed`,
with no timeout and no `Abort()`. This means **the original hang can
recur** if a device disconnects mid-capture in a way that blocks `Read()`
forever with no error — see the trade-off note below.

**What we know from real hardware** (a USB-audio radio interface on macOS,
`github.com/gordonklaus/portaudio@v0.0.0-20260203164431-765aa7dfa631`):
turning the device off does not simply go silent. The observed sequence on
every `Read()` after power-off:
1. One `portaudio.InputOverflowed`, alongside a logged CoreAudio HAL error
   (`||PaMacCore (AUHAL)|| Error on line 2523: err='-10863', msg=Audio Unit:
   cannot do in current context`) — this is a hard Audio Unit failure, not a
   benign transient buffer overflow.
2. A few more `Read()` calls that return **instantly** (~1µs, far faster
   than the real ~42ms buffer cadence) with **stale, non-silent** data —
   the buffer just isn't being refreshed, not zeroed.
3. Then `Read()` blocks forever.

**Crash signature**: three separate real-app crashes, all `SIGSEGV` inside
`Pa_ReadStream` (via cgo) called from `capture()`'s reader — and all three
at the **identical PC** (`0x7ff8107959a9`), which lands in macOS's shared
dyld cache (system frameworks share one fixed address across all processes
on a machine, unlike our own dylib). That's strong evidence this is a
deterministic bug inside Apple's CoreAudio/AudioToolbox code itself (or in
how PortAudio's PaMacCore/AUHAL backend drives it) when reading from a
stream whose device has gone away — not memory corruption from our code.
Since the fault fires inside the C call itself, **no Go-level error
handling gets a chance to run before it happens**, and a signal arriving
during cgo execution is unconditionally fatal to the whole process — it
cannot be caught with `recover()`, on any goroutine.

**Extensive standalone reproduction attempts failed** (see `palist.go`, a
disposable `//go:build ignore` diagnostic script kept at the repo root,
*not* part of the build — reuse or extend it for any future investigation
here rather than instrumenting the app directly). Tested in isolation, none
of the following crashed, even though each is a close analogue of what the
real app was doing when it crashed:
- Calling `stream.Abort()` while a `Read()` is genuinely blocked mid-call
  (documented as the correct way to unblock it). `Abort()` itself always
  returned cleanly — but on this hardware it **did not actually unblock**
  the pending `Read()`; that goroutine just stays leaked, permanently
  blocked inside `Pa_ReadStream`.
- Leaving that leaked goroutine idle for minutes with zero further
  interaction — never crashed spontaneously.
- Calling `portaudio.Terminate()` + `Initialize()` (i.e. what
  `ListInputDevices()` does on every dialog open) while that leaked
  goroutine is still blocked inside the old stream — didn't crash, and
  correctly showed the device gone from the list, then back once
  re-enumerated.
- Opening a brand new second stream on the same physical device afterward,
  and reading several buffers from it — also didn't crash.

So the crash reproduces reliably in the full Wails app but not in a
minimal standalone binary running the same PortAudio calls in the same
sequence. The most likely explanation is that it depends on something
specific to the app's process — most plausibly concurrent cgo/Objective-C
activity from the Wails webview itself (native window/webkit machinery is
heavy on its own C calls) interacting with PortAudio's CoreAudio calls in a
way this trivial single-purpose binary never exercises. That means further
"defensive coding" attempts inside `engine.go` can't be reliably validated
without triggering a real crash in the full app to check — which is exactly
how the last three regressions happened.

**Current trade-off, accepted for now**: `capture()` stays simple. Worst
case on a mid-capture disconnect is the pre-existing hang (the goroutine
blocks in `Read()` forever, `e.stream`/`e.running` never clear, the device
stays stuck "selected" until app restart) — not a crash. `Stop()`'s
`shutdownTimeout` bound means the *UI* won't lock up even then, only the
internal state leaks.

**Real fix (future work)**: isolate the actual PortAudio `Read()` loop in a
separate OS process (or helper binary), communicating audio buffers back
over a pipe/socket. If that child process segfaults, only it dies — the
main app detects the closed connection and reports "device disconnected"
cleanly instead of taking the whole GUI down. This is the only
architecturally sound fix given a C-level crash that Go cannot recover
from; anything short of process isolation is guesswork against a bug we
can't reproduce on demand.

### 12. `srcBtn` has no click handler

There is a `<button id="srcBtn">` in the status bar (currently shows the
selected source label). No `addEventListener` is wired to it in `main.js`.
Either add a click handler (e.g. re-open the source picker) or change it to
a non-interactive `<span>`.

---

## Inherits from the batch CLI (morse-decoder-engine)

The following limitations from the original CLI carry over into this app.
They are documented fully in the engine's README but summarised here for
reference.

**Prosigns not decoded** — `<SK>`, `<AR>`, `<BT>`, `<KN>` etc. are sent as
joined characters. The decoder emits `?` for the second half. A post-processing
pass over the decoded string checking for known two-character joined sequences
would fix this.

**Single carrier assumed** — simultaneous signals on different frequencies are
not separated. The bandpass filter passes whichever carrier is loudest within
the window.

**Filter stage timing distortion** — cascading biquad stages adds group delay
that slightly shortens measured pulse durations at higher WPM. At 2–3 stages
and 40+ WPM this can cause a few percent WPM over-estimation. The Narrow CW /
Noise Reduction settings trade timing accuracy for selectivity.

**Farnsworth detection edge cases** — see the engine README for details. The
gap calibration guard (c1 > 5 × dot) can misfire when filter stage count
changes the dot estimate.

**SNR floor** — below ~5 dB SNR, dot-swallowing becomes the dominant failure
mode (dots fall below the Schmitt trigger threshold and are merged with
adjacent silences, producing the telltale "NNN TTT" pattern). Pre-processing
with `sox` or Audacity helps; a per-frame SNR-based confidence score and
deferred decision would help more.

---

## Potential enhancements (future work)

- **Carrier tracking**: phase-locked loop or adaptive notch updated every 0.5 s
  would keep the bandpass centred on a drifting HF carrier.
- **Error-correcting decode**: build a Morse trie; when a pulse duration falls
  in the ambiguous zone (1.5–2.5 × dot for tones, 3–5 × dot for gaps), try
  both interpretations and emit the one that produces a valid trie path.
- **Automatic bandwidth selection**: after carrier detection, find the nearest
  adjacent peak above the noise floor and set bandwidth to half the distance,
  clamped to [50, 300] Hz.
- **OS dark-mode follow**: read
  `window.matchMedia('(prefers-color-scheme: dark)')` on startup to set the
  initial theme rather than always starting in light mode.
- **`wails dev` hot-reload breaks audio context** — the webview reloads on
  frontend changes, which disconnects the backend event listeners. Mic capture
  continues running in Go, but events are dropped until the new frontend
  reconnects. This is a Wails v2 limitation; workaround: click Stop before
  making frontend changes in dev mode.
- **Process-isolated audio capture** — see item 11 above. Moving the
  PortAudio `Read()` loop into a separate helper process is the only real
  fix for the mid-capture-disconnect crash; would also make the app
  resilient to any other PortAudio/CoreAudio-level fault, not just this one.
