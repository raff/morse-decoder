package main

import (
	"context"
	"log"
	"os"
	"sync/atomic"

	"morse-decoder/websdr"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// FilterConfig mirrors the "Filters" dialog in the UI.
type FilterConfig struct {
	Type      string `json:"type"`      // "None" | "Bandpass" | "Narrow CW"
	Center    int    `json:"center"`    // Hz
	Bandwidth int    `json:"bandwidth"` // Hz
	Squelch   int    `json:"squelch"`   // 0-9
	NoiseRed  bool   `json:"noiseRed"`
	Follow    bool   `json:"follow"` // follow the active station across the band
}

// SpeedConfig mirrors the WPM control (with auto-detect).
type SpeedConfig struct {
	WPM  int  `json:"wpm"`
	Auto bool `json:"auto"`
}

// App is the service bound to the frontend. Every exported method here becomes
// callable from JS through the generated bindings (frontend/bindings).
type App struct {
	engine       *Engine
	webSDRProxy  *websdr.Proxy
	shuttingDown atomic.Bool
}

func NewApp() *App {
	return &App{}
}

// ServiceStartup is called by Wails when the application starts.
func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	// The engine pushes data to the UI by emitting Wails events.
	a.engine = NewEngine(func(event string, data interface{}) {
		application.Get().Event.Emit(event, data)
	})
	a.engine.onPoisoned = func() {
		if !a.shuttingDown.Load() {
			go a.promptRestartAfterPoisoned()
		}
	}
	if err := a.engine.Init(); err != nil {
		log.Println("portaudio init failed:", err)
	}
	return nil
}

// promptRestartAfterPoisoned fires once, the first time the engine latches
// everLeaked (see NOTES.md #11): a mic capture goroutine never confirmed
// exit, most likely still blocked inside a PortAudio C call. From that
// point on ListInputDevices() refuses to touch PortAudio again for the
// rest of this run, so the affected device can't be reselected and the
// list can't be refreshed — there's no clean way back short of a restart.
// Other sources (a WAV file, WebSDR, or a different mic device already in
// the cached list) never call Terminate()/Initialize() and should be
// unaffected. The dialog is non-blocking in Wails v3; the buttons call back.
func (a *App) promptRestartAfterPoisoned() {
	d := application.Get().Dialog.Warning().
		SetTitle("Microphone capture didn't stop cleanly").
		SetMessage("A USB microphone capture didn't stop cleanly after a disconnect, most likely because the driver is still blocked. " +
			"That device can't be reselected and the device list can no longer be refreshed for the rest of this session.\n\n" +
			"Other sources should be unaffected — a WAV file, WebSDR, or a different microphone device already in the list. " +
			"Restart the app if you need the affected device back.")
	cont := d.AddButton("Continue")
	quit := d.AddButton("Quit")
	quit.OnClick(func() { application.Get().Quit() })
	d.SetDefaultButton(cont).SetCancelButton(cont)
	d.Show()
}

// ServiceShutdown is called by Wails when the application is closing.
func (a *App) ServiceShutdown() error {
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
	return nil
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
	return application.Get().Dialog.OpenFile().
		SetTitle("Open WAV file").
		AddFilter("WAV audio (*.wav)", "*.wav").
		PromptForSingleSelection()
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

	return application.Get().Browser.OpenURL(localURL)
}

// ExportText shows a save dialog and writes the decoded text to disk.
func (a *App) ExportText(text string) error {
	path, err := application.Get().Dialog.SaveFile().
		SetMessage("Export decoded text").
		SetFilename("decoded.txt").
		PromptForSingleSelection()
	if err != nil || path == "" {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
