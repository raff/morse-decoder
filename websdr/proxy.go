package websdr

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// AudioChunk is a buffer of mono float32 PCM with its sample rate.
type AudioChunk struct {
	Rate    int
	Samples []float32
}

// Proxy is a reverse-proxy that sits between the app and a WebSDR server.
// It injects an AudioContext tap into HTML pages and exposes a /audio
// WebSocket endpoint that the tap connects to.
type Proxy struct {
	target   *url.URL
	AudioCh  chan AudioChunk // read by the engine
	done     chan struct{}   // closed by Stop() to unblock captureWebSDR
	stopOnce sync.Once      // ensures done is closed exactly once
	script   string         // tap script precomputed after port is known
	port     int
	server   *http.Server
	rp       *httputil.ReverseProxy
	up       websocket.Upgrader
}

// New creates a Proxy for targetURL (e.g. "http://websdr.fi:8080/").
// Call Start() to begin listening.
func New(targetURL string) (*Proxy, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		target:  u,
		AudioCh: make(chan AudioChunk, 64),
		done:    make(chan struct{}),
		up:      websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
	p.rp = &httputil.ReverseProxy{
		Director:       p.director,
		ModifyResponse: p.modifyResponse,
		// Flush every write so streaming responses aren't buffered.
		FlushInterval: -1,
		// DisableCompression so we receive plain text we can inject into.
		Transport: &http.Transport{
			DisableCompression:    true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/audio", p.handleAudio)
	mux.HandleFunc("/", p.serveHTTP)
	p.server = &http.Server{Handler: mux}
	return p, nil
}

// Start binds to a random local port and serves in the background.
// Returns the URL the browser should open (http://localhost:PORT/).
func (p *Proxy) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	p.port = ln.Addr().(*net.TCPAddr).Port
	p.script = p.tapScript() // port is now fixed; precompute once
	go p.server.Serve(ln)   //nolint:errcheck
	return fmt.Sprintf("http://localhost:%d/", p.port), nil
}

// Stop shuts down the proxy. It closes the done channel so that
// captureWebSDR (which blocks on AudioCh) can exit promptly.
// Safe to call more than once.
func (p *Proxy) Stop() {
	p.server.Close()
	p.stopOnce.Do(func() { close(p.done) })
}

// Done returns a channel that is closed when Stop() is called.
// The engine's captureWebSDR goroutine selects on this to unblock.
func (p *Proxy) Done() <-chan struct{} { return p.done }

// ── HTTP handling ────────────────────────────────────────────────────────────

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		p.proxyWebSocket(w, r)
		return
	}
	p.rp.ServeHTTP(w, r)
}

func (p *Proxy) director(r *http.Request) {
	r.URL.Scheme = p.target.Scheme
	r.URL.Host = p.target.Host
	r.Host = p.target.Host
	// Request uncompressed content so modifyResponse can inject into plain HTML.
	r.Header.Set("Accept-Encoding", "identity")
	// Rewrite Origin/Referer so the server doesn't reject cross-origin requests.
	if r.Header.Get("Origin") != "" {
		r.Header.Set("Origin", p.target.Scheme+"://"+p.target.Host)
	}
	if r.Header.Get("Referer") != "" {
		r.Header.Set("Referer", p.target.Scheme+"://"+p.target.Host+"/")
	}
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	// Rewrite redirect Location headers so the browser stays on our proxy.
	if resp.StatusCode/100 == 3 {
		if loc := resp.Header.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil && u.Host == p.target.Host {
				u.Scheme = "http"
				u.Host = fmt.Sprintf("localhost:%d", p.port)
				resp.Header.Set("Location", u.String())
			}
		}
		return nil
	}

	// Only inject into successful HTML responses.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		return nil
	}

	// Remove headers that would block our injected script or WS connection.
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("X-Frame-Options")

	// Read body, decompressing if the server ignored our Accept-Encoding request.
	var body []byte
	var err error
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		gr, gerr := gzip.NewReader(resp.Body)
		if gerr != nil {
			return gerr
		}
		body, err = io.ReadAll(gr)
		gr.Close()
	default:
		body, err = io.ReadAll(resp.Body)
	}
	resp.Body.Close()
	if err != nil {
		return err
	}

	script := fmt.Sprintf("<script>\n%s\n</script>", p.script)
	patched := bytes.Replace(body, []byte("</head>"), []byte(script+"\n</head>"), 1)
	if len(patched) == len(body) {
		// No </head> — try </body>, then prepend.
		patched = bytes.Replace(body, []byte("</body>"), []byte(script+"\n</body>"), 1)
		if len(patched) == len(body) {
			patched = append([]byte(script+"\n"), body...)
		}
	}

	resp.Body = io.NopCloser(bytes.NewReader(patched))
	resp.ContentLength = int64(len(patched))
	// Overwrite the Content-Length header — httputil.ReverseProxy copies
	// resp.Header verbatim, so the raw header must also reflect the new size.
	// Without this the browser sees the original (smaller) value and reports
	// ERR_CONTENT_LENGTH_MISMATCH when we send more bytes than promised.
	resp.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Transfer-Encoding")
	return nil
}

// ── WebSocket: our audio tap receiver ───────────────────────────────────────

func (p *Proxy) handleAudio(w http.ResponseWriter, r *http.Request) {
	conn, err := p.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(msg) < 8 {
			continue
		}
		rate := binary.LittleEndian.Uint32(msg[:4])
		raw := msg[4:]
		n := len(raw) / 4
		samples := make([]float32, n)
		for i := range samples {
			bits := binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
			samples[i] = math.Float32frombits(bits)
		}
		select {
		case p.AudioCh <- AudioChunk{Rate: int(rate), Samples: samples}:
		case <-p.done:
			return
		default: // drop if the engine is behind
		}
	}
}

// ── WebSocket: transparent proxy for the SDR's own WS connections ───────────

func (p *Proxy) proxyWebSocket(w http.ResponseWriter, r *http.Request) {
	targetURL := *p.target
	if targetURL.Scheme == "https" {
		targetURL.Scheme = "wss"
	} else {
		targetURL.Scheme = "ws"
	}
	targetURL.Path = r.URL.Path
	targetURL.RawQuery = r.URL.RawQuery

	// Forward with an Origin the server expects.
	dialHeaders := http.Header{
		"Origin": {p.target.Scheme + "://" + p.target.Host},
	}
	backend, _, err := websocket.DefaultDialer.Dial(targetURL.String(), dialHeaders)
	if err != nil {
		http.Error(w, "websocket backend: "+err.Error(), http.StatusBadGateway)
		return
	}
	client, err := p.up.Upgrade(w, r, nil)
	if err != nil {
		backend.Close()
		return
	}

	done := make(chan struct{}, 2)
	relay := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, msg, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}
	go relay(backend, client)
	go relay(client, backend)
	<-done
	backend.Close()
	client.Close()
	<-done
}

// ── Tap script ───────────────────────────────────────────────────────────────

func (p *Proxy) tapScript() string {
	return fmt.Sprintf(`(function () {
  var wsURL = 'ws://localhost:%d/audio';
  var ws = new WebSocket(wsURL);
  ws.binaryType = 'arraybuffer';

  var orig = AudioNode.prototype.connect;
  AudioNode.prototype.connect = function (dest, outCh, inCh) {
    var ctx = this.context;
    // Tap in parallel: every node that reaches ctx.destination is also fed
    // into a per-context capture ScriptProcessor. Doing this on *every*
    // connect (rather than splicing once in series) means a band switch that
    // tears down and rebuilds the audio graph is picked up automatically —
    // the freshly created source connects to destination, and we tap it too.
    if (dest === ctx.destination && this !== ctx.__morseTap) {
      if (!ctx.__morseTap) {
        var proc = ctx.createScriptProcessor(4096, 2, 2);
        proc.onaudioprocess = function (ev) {
          var ib = ev.inputBuffer;
          var L = ib.getChannelData(0);
          var nCh = ib.numberOfChannels;
          var R = nCh > 1 ? ib.getChannelData(1) : L;

          // Output silence: the source is already wired straight to
          // ctx.destination, so this parallel tap must not add a 2nd copy.
          ev.outputBuffer.getChannelData(0).fill(0);
          if (ev.outputBuffer.numberOfChannels > 1) ev.outputBuffer.getChannelData(1).fill(0);

          if (ws.readyState !== 1) return;

          // Mix to mono and prepend sample-rate header (uint32 LE).
          var n = L.length;
          var buf = new ArrayBuffer(4 + n * 4);
          new DataView(buf).setUint32(0, ctx.sampleRate, true);
          var out = new Float32Array(buf, 4);
          for (var i = 0; i < n; i++) out[i] = (L[i] + R[i]) * 0.5;
          ws.send(buf);
        };
        // Bypass our override for the tap's own wiring to avoid recursion.
        orig.call(proc, ctx.destination);
        ctx.__morseTap = proc;
      }
      // Feed this source into the tap in parallel, then connect normally.
      orig.call(this, ctx.__morseTap);
    }
    return orig.call(this, dest, outCh, inCh);
  };
})();`, p.port)
}
