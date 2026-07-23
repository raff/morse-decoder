// engine_capture_test.go covers capture()'s teardown/state bookkeeping —
// the parts that are safe to fix at the Go level. It deliberately does NOT
// try to reproduce a mid-stream device disconnect (power-off, USB unplug):
// that turned out to require detecting failure via a wedged or repeatedly
// erroring stream.Read(), and retrying/waiting on that call was observed on
// real hardware to segfault inside PortAudio's own C code — a crash that
// cannot be caught or prevented from the Go side. See capture()'s doc
// comment in engine.go for the full story.
package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morse-decoder/websdr"
)

var errAborted = errors.New("stream aborted")

// fakeStream simulates a well-behaved PortAudio stream: every Read()
// succeeds immediately until Abort()/Stop() is called, after which every
// subsequent Read() errors — matching real PortAudio's behavior.
type fakeStream struct {
	mu      sync.Mutex
	aborted bool

	closeCalls int32
}

func (f *fakeStream) Read() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborted {
		return errAborted
	}
	return nil
}

func (f *fakeStream) Abort() error {
	f.mu.Lock()
	f.aborted = true
	f.mu.Unlock()
	return nil
}

func (f *fakeStream) Stop() error { return f.Abort() }
func (f *fakeStream) Close() error {
	atomic.AddInt32(&f.closeCalls, 1)
	return nil
}

// TestCaptureStopIsPromptAndClean verifies the normal user-initiated Stop
// path exits quickly and cleanly.
func TestCaptureStopIsPromptAndClean(t *testing.T) {
	emit := func(event string, data interface{}) {}
	e := NewEngine(emit)
	e.initLiveDecoder(FilterConfig{Type: "None"}, SpeedConfig{WPM: 20}, sampleRate)
	e.running = true

	stream := &fakeStream{}
	in := make([]float32, framesPerBuffer)

	done := make(chan struct{})
	e.wg.Add(1)
	go func() {
		e.capture(stream, in)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	e.mu.Lock()
	e.running = false
	e.mu.Unlock()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("capture() did not return promptly after running was cleared")
	}

	if atomic.LoadInt32(&stream.closeCalls) != 1 {
		t.Errorf("expected stream.Close() called once, got %d", stream.closeCalls)
	}

	e.mu.Lock()
	running := e.running
	stillSet := e.stream != nil
	e.mu.Unlock()
	// capture() must clear e.running/e.stream itself on exit: nothing else
	// does, and if it stayed true, every subsequent Start() would silently
	// no-op forever (its "already running" guard would stay tripped).
	if running {
		t.Error("e.running is still true after capture() exited")
	}
	if stillSet {
		t.Error("e.stream is still set after capture() exited")
	}
}

// TestCaptureReportsHardReadError verifies a genuine (non-overflow) read
// error ends capture() and reports it, without needing a watchdog.
func TestCaptureReportsHardReadError(t *testing.T) {
	var errMsgs []string
	var mu sync.Mutex
	emit := func(event string, data interface{}) {
		if event == "error" {
			mu.Lock()
			errMsgs = append(errMsgs, data.(string))
			mu.Unlock()
		}
	}

	e := NewEngine(emit)
	e.initLiveDecoder(FilterConfig{Type: "None"}, SpeedConfig{WPM: 20}, sampleRate)
	e.running = true

	stream := &fakeStream{}
	stream.Abort() // pre-aborted: every Read() errors immediately
	in := make([]float32, framesPerBuffer)

	done := make(chan struct{})
	e.wg.Add(1)
	go func() {
		e.capture(stream, in)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("capture() did not return after a hard read error")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(errMsgs) != 1 {
		t.Fatalf("expected exactly one error event, got %v", errMsgs)
	}
}

// TestCaptureWebSDRClearsRunningOnProxyDone covers the WebSDR-source analogue:
// if the proxy connection goes away on its own (proxy.Done() fires) rather
// than via Stop()-initiated shutdown, captureWebSDR must still clear
// e.running itself — otherwise Start() would silently no-op on any later
// attempt to pick a new source.
func TestCaptureWebSDRClearsRunningOnProxyDone(t *testing.T) {
	proxy, err := websdr.New("http://example.invalid/")
	if err != nil {
		t.Fatalf("websdr.New: %v", err)
	}
	proxy.Stop() // closes Done() without ever calling Start()

	e := NewEngine(func(string, interface{}) {})
	e.initLiveDecoder(FilterConfig{Type: "None"}, SpeedConfig{WPM: 20}, sampleRate)
	e.running = true

	done := make(chan struct{})
	e.wg.Add(1)
	go func() {
		e.captureWebSDR(proxy, make(chan struct{}))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("captureWebSDR did not return after proxy.Done() fired")
	}

	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if running {
		t.Error("e.running is still true after captureWebSDR exited via proxy.Done() — Start() will now silently no-op")
	}
}

// TestStopDoesNotHangOnWedgedGoroutine covers the "exit hangs, have to kill
// it" report: if the capture goroutine's own teardown gets wedged at the OS
// level, Stop() must still return instead of blocking forever on wg.Wait().
// Simulated by holding the WaitGroup open with a goroutine that never calls
// Done(), standing in for a stuck low-level call.
func TestStopDoesNotHangOnWedgedGoroutine(t *testing.T) {
	origTimeout := shutdownTimeout
	shutdownTimeout = 100 * time.Millisecond
	defer func() { shutdownTimeout = origTimeout }()

	e := NewEngine(func(string, interface{}) {})
	e.running = true
	e.wg.Add(1) // never Done() — stands in for a wedged teardown call

	done := make(chan struct{})
	go func() {
		e.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung waiting on a wedged goroutine instead of giving up after shutdownTimeout")
	}
}

// TestCloseDoesNotHangOnWedgedTerminate covers the same scenario one layer
// up: Close() (called from app shutdown) must return even if the wrapped
// call it bounds — standing in for a wedged portaudio.Terminate() — never
// completes.
func TestCloseDoesNotHangOnWedgedTerminate(t *testing.T) {
	origTimeout := shutdownTimeout
	shutdownTimeout = 100 * time.Millisecond
	defer func() { shutdownTimeout = origTimeout }()

	done := make(chan struct{})
	go func() {
		runBounded(func() { select {} }) // never returns
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBounded hung instead of giving up after shutdownTimeout")
	}
}
