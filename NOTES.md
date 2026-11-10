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
written as a standalone CLI (`morse-decoder-engine`), and both now decode
through `Engine.decodeBuffer` and `dsp.Detector`; see item 13.

---

## Known issues

### 1. Char-gap peek feeds the decoder every buffer — **[fixed]**

Fixed: `Engine.charGapPeeked` (`engine.go`) is set on the first peek for a
given silence run and reset when a new tone starts, so the peek now fires
exactly once per silence transition instead of on every buffer.

### 2. Word space only appears when the next word starts — **[fixed]**

Fixed. Turned out a single peek flag (as originally proposed here) wasn't
enough: the word-gap threshold is crossed on a *later* buffer than the
char-gap threshold within the same silence run, so by the time silence is
long enough to be a word gap, the one flag from item 1 was already spent.

The actual fix, in `engine.go`/`morse/decoder.go`:
- A second flag, `wordGapPeeked`, gates the word-space peek independently of
  `charGapPeeked`, both reset when a new tone starts.
- `Decoder.Feed(SymWordGap)` (`morse/decoder.go`) is now idempotent — it only
  writes a space if the output doesn't already end with one — so an early
  speculative peek and the real completion event later in `decodePulse` can
  both feed the same gap without producing a double space.
- Added `Decoder.Peek()`, which flushes any in-progress character like
  `Flush()` but doesn't trim trailing whitespace; `emitDecoded` now diffs
  against `Peek()` instead of `Flush()` so a trailing inter-word space shows
  up immediately instead of being trimmed away until more text follows it.
  `Flush()` itself is now just `TrimSpace(Peek())`.

One accepted cosmetic side effect: if a live session ends exactly on a word
gap, the final decoded text can carry one trailing space. Invisible in the
UI (`#textOut` is `white-space:pre-wrap`), so not worth guarding against.

Regression test: `TestLiveWordSpaceEmittedDuringPause` in
`engine_wordspace_test.go`.

### 3. AGC toggle — **[obsolete]**

The toggle and the running-peak AGC are both gone; see item 13.

### 4. Auto-notch filter — **[obsolete]**

No longer offered in the filter type dropdown, and there's no filter stage for
it to apply to; see item 13.

### 5. No live carrier tracking — **[fixed]**

Went with the "lock button + visual warning" option rather than full
auto-tracking (an EMA continuously chasing the Goertzel peak was judged too
invasive: needs a new config toggle, backend loop, and risks chasing noise or
fighting a manual drag). Frontend-only change, `frontend/src/main.js`:

- `status.freq` (the Goertzel peak, already computed backend-side) is now
  tracked client-side as `state.freqNow`.
- `updateFreqWarn()` colors `#freqRo` (`.ro.warn`, `style.css`) when the peak
  has drifted more than half the bandwidth from `filter.Center` — i.e. off
  the edge of the passband — and enables/disables the new lock button
  accordingly. Wired into the `status` handler, `pushFilter()`, and
  `renderRecBtn()` so it stays in sync with filter changes and run state.
- The new `#lockFreqBtn` (target icon, next to the Freq readout) sets
  `filter.Center` to the current peak in one click. Reuses the same
  clamp/step/push logic the spectrum-drag handler already used — extracted
  into `setCenter()` so both share it instead of duplicating the logic.

Not visually verified in a running window — no headless browser or Wails
driver was available in this environment to screenshot it; verified via a
clean `vite build` and by tracing the logic. Worth an eyeball in `wails dev`
before considering this fully done.

### 6. Bootstrap delay at start of live session — **[fixed]**

Pulses are now held until the first speed lock and then decoded, so the start
of a transmission is delayed rather than lost. The lock waits for tones that
look like real dots and dashes, up to `maxBootstrapTones`.

### 7. Speed estimator EMA α-boost can be triggered by noise — **[fixed]**

(Note: by the time this was fixed, the boosted α had already been tuned down
to 0.15 in an earlier commit — the α = 0.25 figure above was stale.)

Fixed in `morse/timing.go`: `update()` now tracks `devDir`/`devStreak`, the
direction and length of the current run of same-direction >30% deviations.
The boosted α only kicks in once `speedChangeStreak` (3) consecutive dots
deviate the same way; a single outlier, or a normal dot in between, resets
the streak. Regression tests in `morse/timing_test.go`:
`TestUpdateIgnoresSingleOutlier`, `TestUpdateBoostsOnSustainedSpeedChange`.

### 8. `toneDurMs` / `silDurMs` not a true sliding window — **[fixed]**

Fixed in `engine.go`: `appendDur()` replaces the fixed 500-entry cap with a
`durationWindowMs` (30s) cumulative-duration window, trimming from the front
of the slice and tracking a running sum (`toneDurSumMs`/`silDurSumMs`) so
trimming stays O(1) amortised rather than re-summing on every call. A
mid-session speed change now rotates out in ~30s instead of the old cap's
worst case of ~100s. Regression tests in `engine_durwindow_test.go`:
`TestAppendDurTrimsByDuration`, `TestAppendDurKeepsAtLeastOneEntry`.

### 9. `wails dev` may still lack the microphone permission key

`build/darwin/Info.plist` (used by `wails build`) has been updated with
`NSMicrophoneUsageDescription`. The equivalent file for `wails dev` is
`build/darwin/Info.dev.plist` (generated by Wails if it exists). If mic capture
fails only in dev mode but works in a built `.app`, add the same key there.

### 10. Silence noise pulses not filtered — **[fixed]**

Fixed: `decodePulse` (`engine.go`) now drops silences shorter than
`0.2 × DotMs`, mirroring the existing tone noise gate, before they reach
`silDurMs`/the decoder.

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

**Update — `ListInputDevices()` now stops capture before reinit**:
`ListInputDevices()` used to skip the `Terminate()`/`Initialize()` rescan
entirely whenever `e.stream != nil`, so a disconnect that left `capture()`
wedged (not erroring, not exiting — the "stale reads then blocks forever"
sequence above) meant the device stayed in the list forever, and the
"select device" control stayed showing it as active, until the whole app
was restarted. `ListInputDevices()` now calls `e.Stop()` first whenever a
mic stream is open, exactly like a user-initiated stop: it clears
`e.running`, then waits for the capture goroutine to finish tearing down
its stream, bounded by `shutdownTimeout`. This closes the fast-path gap —
if `capture()`'s `Read()` loop is still returning (even with repeated
`InputOverflowed`), opening the picker now reliably stops it and the
rescan proceeds — and an `"error"` event is emitted afterward so the
frontend's existing disconnect-handling path (see `main.js`'s `error`
listener) resets the mic/web button state, which previously stayed stuck
"pressed" since nothing told the frontend capture had stopped.

This does **not** change the outcome for a genuinely wedged `Read()`:
`e.Stop()`'s bounded wait times out the same way it always would, and
`ListInputDevices()` correctly leaves the stream alone (skips
`Terminate()`) rather than forcing it — the crash risk described above is
about forcing a wedged call, and this change deliberately doesn't do that.

One deliberate UX trade-off: opening the device picker now stops mic
capture even if the current device is perfectly healthy, not just a dead
one — there's no cheap way to tell those apart without touching the
stream. Reselecting a device already went through a `Stop()`+`Start()`
cycle via the frontend's `startDecoding()`, so this mostly just moves that
interruption earlier (to picker-open instead of device-click) for the
common case, but it does mean briefly opening the picker to look at what's
connected now interrupts an active decode.

**Update — a leaked capture goroutine poisons *future*, unrelated streams
too**: confirmed on real hardware that the above wasn't the whole story.
After several open/close/reselect cycles post-disconnect, the app crashed
again — same `SIGSEGV` signature, same PC — but this time inside `Read()`
on a **freshly opened, healthy stream**, in a goroutine `Start()` had just
created (not the earlier, presumably-still-wedged one). The likely
mechanism: once one capture goroutine leaks (blocked forever inside
`Pa_ReadStream`, per the sequence above), `e.stream` gets overwritten by
the *next* successful `Start()` and the engine loses all track of that
orphaned goroutine — but its blocked C call is still live. Any later
`ListInputDevices()` call that observes `e.stream == nil` (e.g. because
the *current* capture stopped cleanly) will happily call
`portaudio.Terminate()`, unaware that an old, invisible leaked read is
still in flight underneath. `palist.go`'s standalone repro tested exactly
one `Terminate()`+`Initialize()` against exactly one leaked goroutine and
found it safe — but the real app can accumulate *multiple* leaks across
repeated cycles, and each subsequent `Terminate()` is another roll of the
dice against all of them at once plus whatever concurrent cgo/webview
activity Wails adds. That combination is apparently what it takes to
trigger it, which is consistent with why the isolated single-leak repro
never reproduced it.

Given a leaked capture goroutine can never be confirmed to have gone away
(there is no way to cancel a blocked C call), the fix is to treat *any*
observed leak as permanently disqualifying further PortAudio reinit: once
`Stop()` (called from `ListInputDevices`, the user's Stop button, or
`Close()`) observes its bounded wait on the capture goroutine time out
*while a mic stream was open*, it latches `Engine.everLeaked = true` for
the rest of the process's life. From then on, `ListInputDevices()` skips
`Stop()`'s bounded wait and `portaudio.Terminate()`/`Initialize()`
entirely — no more attempts to rescan, and no more repeated
`shutdownTimeout` (5s) stalls on every picker open, which is also what
was making the picker "take a very long time" to open: that delay itself
was the symptom of a leak having already happened. The device list is
just returned as last known and an error is emitted explaining a restart
is needed to recover enumeration. This is a strictly more conservative
version of the existing accepted trade-off (hang over crash) — it now
also gives up the ability to refresh the list at all, for the whole
remaining session, rather than just for the one affected device.
(`everLeaked` only latches when the timed-out capture actually owned a
PortAudio stream — a stuck WebSDR proxy read, which never touches
PortAudio, is excluded; see `TestStopLatchesEverLeakedOnlyForWedgedMicStream`
in `engine_capture_test.go`.)

Confirmed on real hardware: no crash after this, but the poisoned state is
a dead end as designed — the picker no longer removes the dead device or
lets it be reselected, and the only way out is restarting the app. Since
that's now the deliberate behavior rather than a bug, `App` (`app.go`)
offers to quit the first time `everLeaked` latches: `Engine.onPoisoned`
fires once (set up in `startup()`), which shows a native Continue/Quit
dialog via `runtime.MessageDialog` on its own goroutine (it blocks on the
user's response) and calls `runtime.Quit()` if they choose Quit. It's
guarded by `App.shuttingDown` so a leak detected during `Close()` itself
(app already exiting) doesn't pop a dialog on the way out. Other sources
— a WAV file, WebSDR, or a different mic device still in the cached list
— never call `Terminate()`/`Initialize()`, so they should keep working
normally even after poisoning; only the specific device that leaked, and
re-enumeration in general, are affected.

**Real fix (future work)**: isolate the actual PortAudio `Read()` loop in a
separate OS process (or helper binary), communicating audio buffers back
over a pipe/socket. If that child process segfaults, only it dies — the
main app detects the closed connection and reports "device disconnected"
cleanly instead of taking the whole GUI down. This is the only
architecturally sound fix given a C-level crash that Go cannot recover
from; anything short of process isolation is guesswork against a bug we
can't reproduce on demand.

### 12. `srcBtn` has no click handler — **[fixed]**

Fixed: `#srcBtn` (`frontend/index.html`) is now a non-interactive `<span>`
(reusing the existing `.lab` style) instead of a `<button>` with no
handler.

### 13. Decode path replaced by `dsp.Detector`

The bandpass, rectifier, peak AGC, Schmitt trigger and SNR gate were replaced
by a streaming detector shared by live and file decoding. Measured with
`engine_quality_test.go`, the old path failed in ways that couldn't be tuned out:

- Noise fills a peak-normalised envelope, so thresholds were crossed constantly.
- The SNR gate chopped that noise into letter-sized pieces and made garbage
  worse (0 to 54-116 characters per minute on noise alone).
- In auto mode the first speed lock trained on noise and never recovered, since
  only tones classified as dots feed the EMA.
- A recording's second station on another pitch decoded as pure noise.

Design points worth keeping if this is changed:

- Each bin's noise floor is its 10th percentile. An earlier 25th percentile
  followed the tone itself at high speed, where smearing leaves few key-up frames.
- The median of all bins measures wideband static each frame, except when one
  bin is far above it: strong tones' key clicks lift the median and aren't static.
- The integration window is 0.85 dot. A full dot was better at low SNR but merged
  elements; 0.7 cost a lot of sensitivity.
- Tone-to-noise ratio scales with window length, so the tracked level is
  adjusted when the window changes.
- Acquiring a station replays up to 3 s of stored frames, so nothing is lost
  while it's being recognised. Live output of a new station's first characters
  is delayed by that recognition time.

- A candidate station must show at least 4 separate key-down runs, and be
  either clearly above noise or key-down much of the time. Activity alone also
  rises for one isolated burst or for scattered noise spikes on one bin. These
  thresholds were set from a small set of recordings and may need adjusting.

Follow mode locks onto whichever station was keying first. With two stations
sending at once it stays on that one even if the other is the one wanted;
tuning by hand is the way to choose.

### 14. Crowded bands: one lock can't separate close stations **[open]**

Follow mode has a single lock that moves between signals. It works when
stations take turns and are well apart, but two recordings show the limit:

- `websdr_recording_2026-11-09T22_52_31Z_14044.0kHz.wav`: from about 108 s,
  signals near 580, 660, 730, 850 and 1010 Hz are active at once. The lock
  slides between them a few bins at a time, so no `NewStation` fires and the
  next signal is decoded with the previous station's speed. The first 38 s of
  this recording are also poor, for a reason not yet investigated.
- `websdr_recording_2026-11-09T21_46_56Z_14055.4kHz.wav`: stations near 570
  and 630 Hz, close enough that the lock can settle at 600 Hz between them.

Tried and reverted: bounding drift to `sameBins` from where the station was
acquired. It stopped the sliding but didn't help the first recording, and broke
the second, where the sliding happened to reach the other station.

Proposed next step: track each active pitch separately, with its own timing
and speed, and choose which one to display. Further tuning of the single lock
is likely to trade one recording against another.

### 15. Characters run together on tight spacing **[open]**

Some senders leave gaps between letters of only 80-100 ms against 60-70 ms
within letters (dot about 55 ms), which produces `?` from a run of two letters,
e.g. `--..--..` in the 14055.4 kHz recording. Timing alone doesn't resolve it.
Tried and reverted:

- Learning the intra/char gap boundary per sender: split real letters
  (`5NN` became `5NTE`) and didn't fix the `?` cases.
- Splitting unknown codes at their longest gaps: left valid letters alone, but
  replaced `?` with wrong letters (`?2` became `ETTTI2`) and raised the error
  rate in static.
- A longer detector debounce: changed only the noisy start of the recording.

This needs context about likely letters and words, from a sequence decoder or
a trained model.

### 16. Possible next steps

1. Per-station tracking (item 14).
2. A real-world benchmark: a hand-copied transcript of part of the 14044.0 kHz
   conversation would give the project its first real ground truth.
3. Trained tone detector replacing `Detector.decide`, then a sequence decoder.
   Copy is still mostly lost at about -10 dB, and fading remains weak.

An unconfirmed report: reception once appeared to stop while the spectrum kept
running, with the readout near 300 Hz. The likely cause, a station above the
old 1100 Hz limit, was reproduced and the band widened, but the original event
was never captured. A recording would confirm it if it recurs.

### Building

The project was migrated from Wails v2 to v3 (v2.12.0 bundled an old
`golang.org/x/tools` that fails under Go 1.27 with `package "math" without
types`). Use `wails3 build` / `wails3 package` / `wails3 dev`; frontend bindings
are generated into `frontend/bindings` (gitignored) by the build tasks.

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

**Farnsworth detection edge cases** — see the engine README for details. The
gap calibration guard (c1 > 5 × dot) can misfire when filter stage count
changes the dot estimate.

**SNR floor** — copy degrades quickly below about -5 dB (in 2.5 kHz) and is
mostly lost by -10 dB. Key decisions are still made frame by frame, so a soft
per-frame tone probability with a sequence decoder (or a trained tone detector
in place of `Detector.decide`) is the likely next gain.

---

## Potential enhancements (future work)

- **Error-correcting decode**: build a Morse trie; when a pulse duration falls
  in the ambiguous zone (1.5–2.5 × dot for tones, 3–5 × dot for gaps), try
  both interpretations and emit the one that produces a valid trie path.
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
