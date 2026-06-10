# Morse Decoder

A desktop Morse audio decoder built with Go + Wails. The Go side captures audio
and runs the DSP pipeline; a Vite/Vanilla-JS frontend renders the spectrum,
decoded text, and controls. They communicate over Wails: the frontend calls
bound Go methods, and Go pushes results back as runtime events.

## Prerequisites

- Go 1.22+ with cgo enabled (on macOS: Xcode Command Line Tools)
- Node 18+
- PortAudio:
  - macOS: `brew install portaudio pkg-config`
  - Debian/Ubuntu: `sudo apt install portaudio19-dev`
- Wails v2 CLI:
  ```
  go install github.com/wailsapp/wails/v2/cmd/wails@latest
  ```

## Run / Build

```bash
go mod tidy
wails dev          # hot-reloading dev build
wails build        # distributable app → build/bin/
```

### macOS microphone permission

After first build, run once to clear any stale denial before the permission
dialog can appear:

```bash
tccutil reset Microphone com.wails.morse-decoder
```

The required `NSMicrophoneUsageDescription` key is already in
`build/darwin/Info.plist` and the built bundle's `Info.plist`.

## Sources

| Source | How to activate |
|--------|-----------------|
| **Microphone / radio interface** | Click the mic icon → pick a device. Decoding starts automatically. |
| **WAV file** | Click the folder icon → pick a file. Decoding starts automatically and the button resets when done. |

Previously selected mic devices are remembered across launches (stored in
`localStorage`). If the saved device is no longer present when decoding starts
(e.g. a USB radio interface that is powered off), an error is shown and no
audio is opened — the system default microphone is not used as a silent
fallback. File paths are not restored — they go stale.

## Controls

| Control | Purpose |
|---------|---------|
| ▶ / ⏸ | Start / pause decoding |
| Filters popover | Filter type, centre frequency, bandwidth, squelch (0–9), noise reduction, AGC |
| WPM stepper | Manual speed hint |
| Auto pill | Enable adaptive speed detection; detected WPM shown in accent colour when running |
| Clear | Reset decoded text and internal decoder state |
| Export | Save decoded text to a file |
| Both / Text / Morse | Toggle output panes |

## Filter types

| Type | Behaviour |
|------|-----------|
| None | No bandpass; full signal passes through |
| Bandpass | Single-stage IIR bandpass (default) |
| Narrow CW | Two-stage bandpass (steeper roll-off, ~64% effective BW) |
| Auto-notch | Not yet implemented — falls back to Bandpass |

Noise Reduction adds one extra filter stage to whichever type is selected.
Squelch sets the Schmitt trigger threshold (0 = most sensitive, 9 = strongest
signal required).

## Layout

```
main.go                  Wails setup (window, embed, bindings)
app.go                   API methods bound to the frontend
engine.go                DSP engine — capture, filtering, decoding
audio/wav.go             WAV parser (PCM 8/16/24/32-bit, mono/stereo)
dsp/filter.go            Biquad IIR filters (bandpass, lowpass, chains)
dsp/envelope.go          Envelope detection + Schmitt trigger
dsp/fft.go               FFT + carrier detection
morse/timing.go          Speed estimator (k-means bootstrap, EMA adaptation)
morse/decoder.go         Symbol → ASCII decoder
morse/table.go           ITU-R Morse code table
testdata/gen_test.go     Synthetic WAV tests (clean, noisy, WPM auto-detect)
frontend/index.html      Markup
frontend/src/main.js     UI logic + Wails event wiring
frontend/src/style.css   Theme (light/dark) and component styles
```

## Event protocol

**Frontend → Go** (Wails-bound methods on `App`):

| Call | Purpose |
|------|---------|
| `Start()` / `Stop()` | Begin / end decoding |
| `SetSource(kind, device)` | `"mic"` + device name, or `"file"` + path |
| `ListInputDevices()` | Device names for the source picker |
| `OpenWavFile()` | Native file picker, returns chosen path |
| `SetFilter(FilterConfig)` | Type, centre Hz, bandwidth Hz, squelch, NR, AGC |
| `SetSpeed(SpeedConfig)` | WPM hint + auto-detect flag |
| `Clear()` / `ExportText(text)` | Reset decoder state / save text |

**Go → Frontend** (Wails runtime events):

| Event | Payload | Purpose |
|-------|---------|---------|
| `spectrum` | `{ bins: float64[] }` (0–1) | Spectrum strip (Goertzel bank) |
| `decoded` | `{ text, morse }` | Appended to output panes |
| `status` | `{ freq, wpm, levelDb }` | Toolbar readouts |
| `done` | — | File decode finished; resets play button |
| `error` | string | Capture/decode failure; resets to idle |

`bins` map linearly across `SPEC_FMIN..SPEC_FMAX` (300–1100 Hz by default),
matching the same constants in `engine.go` and `main.js`.

## DSP pipeline

### File mode (batch)
```
LoadWAV → carrier FFT → bandpass chain → envelope (rectify + lowpass 100 Hz)
→ 95th-percentile AGC → Schmitt trigger → glitch filter (≥10 ms)
→ k-means WPM bootstrap → gap calibration → per-pulse classify → ASCII
```

### Live mic mode (streaming, per 2048-sample buffer at 48 kHz ≈ 42 ms)
```
portaudio read → bandpass chain → envelope (rectify + lowpass 100 Hz)
→ running-peak AGC (τ ≈ 2 s) → Schmitt trigger → pulse merger
→ noise gate (< max(10 ms, 0.3 × dotMs) dropped) → classify → ASCII
```

Speed is seeded from the manual WPM setting. In Auto mode the EMA adapts
after bootstrap (≥ 8 tones); the dot estimate is clamped to ≤ 50 WPM to
prevent noise from sending it haywire.

## Running the tests

```bash
go test ./testdata/ -v
```

Tests synthesise WAV files at various speeds and SNR levels and verify decode
accuracy and WPM auto-detection (clean, 20 dB SNR, 10 dB SNR, 10/20/30 WPM).

---

See [NOTES.md](NOTES.md) for implementation notes, known issues, and areas
that need further review.
