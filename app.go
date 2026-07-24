package main

import (
	"context"
	"os"
	"sync/atomic"

	"morse-decoder/websdr"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// FilterConfig mirrors the "Filters" dialog in the UI.
type FilterConfig struct {
	Type      string `json:"type"`      // "None" | "Bandpass" | "Narrow CW"
	Center    int    `json:"center"`    // Hz
	Bandwidth int    `json:"bandwidth"` // Hz
	Squelch   int    `json:"squelch"`   // 0-9
	NoiseRed  bool   `json:"noiseRed"`
}

// SpeedConfig mirrors the WPM control (with auto-detect).
type SpeedConfig struct {
	WPM  int  `json:"wpm"`
	Auto bool `json:"auto"`
}

// App is the object bound to the frontend. Every exported method here becomes
// callable from JS as window.go.main.App.<Method>(...).
type App struct {
	ctx          context.Context
	engine       *Engine
	webSDRProxy  *websdr.Proxy
	shuttingDown atomic.Bool
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// The engine pushes data to the UI by emitting Wails runtime events.
	a.engine = NewEngine(func(event string, data interface{}) {
		runtime.EventsEmit(a.ctx, event, data)
	})
	a.engine.onPoisoned = func() {
		if !a.shuttingDown.Load() {
			go a.promptRestartAfterPoisoned()
		}
	}
	if err := a.engine.Init(); err != nil {
		runtime.LogError(a.ctx, "portaudio init failed: "+err.Error())
	}
}

// promptRestartAfterPoisoned fires once, the first time the engine latches
// everLeaked (see NOTES.md #11): a mic capture goroutine never confirmed
// exit, most likely still blocked inside a PortAudio C call. From that
// point on ListInputDevices() refuses to touch PortAudio again for the
// rest of this run, so the affected device can't be reselected and the
// list can't be refreshed — there's no clean way back short of a restart.
// Other sources (a WAV file, WebSDR, or a different mic device already in
// the cached list) never call Terminate()/Initialize() and should be
// unaffected. Runs on its own goroutine since MessageDialog blocks until
// the user responds.
func (a *App) promptRestartAfterPoisoned() {
	result, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
		Type:  runtime.WarningDialog,
		Title: "Microphone capture didn't stop cleanly",
		Message: "A USB microphone capture didn't stop cleanly after a disconnect, most likely because the driver is still blocked. " +
			"That device can't be reselected and the device list can no longer be refreshed for the rest of this session.\n\n" +
			"Other sources should be unaffected — a WAV file, WebSDR, or a different microphone device already in the list. " +
			"Restart the app if you need the affected device back.",
		Buttons:       []string{"Continue", "Quit"},
		DefaultButton: "Continue",
		CancelButton:  "Continue",
	})
	if err != nil {
		runtime.LogError(a.ctx, "message dialog failed: "+err.Error())
		return
	}
	if result == "Quit" {
		runtime.Quit(a.ctx)
	}
}

func (a *App) shutdown(ctx context.Context) {
	// Once shutdown has started, a leak detected by engine.Close()'s own
	// Stop() call shouldn't pop a "quit?" dialog — the app is already on
	// its way out.
	a.shuttingDown.Store(true)
	// Stop the proxy first — this closes its Done() channel, which unblocks
	// captureWebSDR so engine.Close() / wg.Wait() can return promptly.
	if a.webSDRProxy != nil {
		a.webSDRProxy.Stop()
	}
	if a.engine != nil {
		a.engine.Close()
	}
}

// ---- Methods callable from the frontend ----

// Start begins decoding from the currently selected source.
func (a *App) Start() { a.engine.Start() }

// Stop halts decoding.
func (a *App) Stop() { a.engine.Stop() }

// SetSource selects the input. kind is "mic" or "file".
// For "mic", device is the device name returned by ListInputDevices.
// For "file", device is the path returned by OpenWavFile.
func (a *App) SetSource(kind string, device string) {
	a.engine.SetSource(kind, device)
}

// ListInputDevices returns the available audio input device names.
func (a *App) ListInputDevices() []string {
	return a.engine.ListInputDevices()
}

// OpenWavFile shows a native file picker and returns the chosen path ("" if cancelled).
func (a *App) OpenWavFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Open WAV file",
		Filters: []runtime.FileFilter{
			{DisplayName: "WAV audio (*.wav)", Pattern: "*.wav"},
		},
	})
}

// SetFilter applies new filter / squelch settings (from the Filters dialog).
func (a *App) SetFilter(cfg FilterConfig) { a.engine.SetFilter(cfg) }

// SetSpeed applies the WPM setting (and whether to auto-detect speed).
func (a *App) SetSpeed(cfg SpeedConfig) { a.engine.SetSpeed(cfg) }

// Clear resets the decoder's running state / buffers.
func (a *App) Clear() { a.engine.Clear() }

// OpenWebSDR starts a local reverse-proxy for targetURL, opens it in the
// system browser (with an injected AudioContext tap), and switches the engine
// to the WebSDR source.  Calling it again with a different URL restarts the proxy.
func (a *App) OpenWebSDR(targetURL string) error {
	// Stop any existing proxy first — closes Done() so captureWebSDR unblocks
	// before engine.Stop()/wg.Wait() is called.
	if a.webSDRProxy != nil {
		a.webSDRProxy.Stop()
		a.engine.Stop()
		a.webSDRProxy = nil
	}

	proxy, err := websdr.New(targetURL)
	if err != nil {
		return err
	}
	localURL, err := proxy.Start()
	if err != nil {
		return err
	}

	a.webSDRProxy = proxy
	a.engine.SetWebSDRProxy(proxy)
	a.engine.SetSource("websdr", "")

	runtime.BrowserOpenURL(a.ctx, localURL)
	return nil
}

// ExportText shows a save dialog and writes the decoded text to disk.
func (a *App) ExportText(text string) error {
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Export decoded text",
		DefaultFilename: "decoded.txt",
	})
	if err != nil || path == "" {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
