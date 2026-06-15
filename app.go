package main

import (
	"context"
	"os"

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
	ctx    context.Context
	engine *Engine
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
	if err := a.engine.Init(); err != nil {
		runtime.LogError(a.ctx, "portaudio init failed: "+err.Error())
	}
}

func (a *App) shutdown(ctx context.Context) {
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
