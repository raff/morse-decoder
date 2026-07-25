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
// Returns the URL the browser should open.
//
// The target's path, query and fragment are carried over, so a receiver-specific
// entry point survives the round trip. Several WebSDRs run more than one
// receiver behind one host and reserve "/" for something else entirely —
// websdr2.sdrutah.org:8902 serves a "we have moved" page at "/" that bounces
// the browser to sdrutah.org, with the actual receiver at "/index1a.html".
// Opening the bare root there drops the user out of the proxy altogether.
func (p *Proxy) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	p.port = ln.Addr().(*net.TCPAddr).Port
	p.script = p.tapScript() // port is now fixed; precompute once
	go p.server.Serve(ln)   //nolint:errcheck

	local := url.URL{
		Scheme:   "http",
		Host:     fmt.Sprintf("localhost:%d", p.port),
		Path:     p.target.Path,
		RawQuery: p.target.RawQuery,
		Fragment: p.target.Fragment,
	}
	if local.Path == "" {
		local.Path = "/"
	}
	return local.String(), nil
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
	// Suppress client-IP forwarding headers. httputil.ReverseProxy would
	// otherwise append X-Forwarded-For with our client's address — always
	// 127.0.0.1, since the browser talks to our local listener. Servers that
	// trust that header as the real client IP then see a loopback address and
	// refuse: KiwiSDR (Mongoose) answers 403 Forbidden for the whole site.
	// Assigning nil (rather than deleting) is what tells ReverseProxy to leave
	// the header off instead of adding its own.
	r.Header["X-Forwarded-For"] = nil
	r.Header.Del("X-Real-IP")
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
	patched := injectScript(body, script)

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

// injectScript places script at the earliest safe point in body: immediately
// after the opening <head> tag, falling back through </head>, <body>, </body>,
// <html> and finally a doctype-safe prepend.
//
// It no longer prefers "</head>" as the anchor. Several WebSDR installations
// (na5b.com:8901 among them) carry a commented-out boilerplate block near the
// top of the page:
//
//	<head>
//	<title>…</title>
//	<!-- <style> … </style>
//	</head> -->
//	<body>
//
// The first literal "</head>" there is inside the comment, so injecting before
// it buries the tap script where the browser never parses it — the page loads
// and plays audio normally, but no samples ever reach the proxy.
//
// Injecting at the top of <head> also guarantees the AudioNode.prototype.connect
// override is installed before any of the page's own scripts build an audio graph.
func injectScript(body []byte, script string) []byte {
	// Anchors in preference order. Each returns an offset to splice at, or -1.
	// The "</head>" / "</body>" entries reproduce the original (pre-fix) anchors
	// so that pages which omit the optional <head> open tag still get the script
	// where they always did — just skipping any anchor buried in a comment.
	for _, anchor := range []func([]byte) int{
		func(b []byte) int { return afterOpenTag(b, "<head") },
		func(b []byte) int { return indexTagOutsideComment(b, "</head>") },
		func(b []byte) int { return afterOpenTag(b, "<body") },
		func(b []byte) int { return indexTagOutsideComment(b, "</body>") },
		func(b []byte) int { return afterOpenTag(b, "<html") },
		// Last resort: after any doctype, so we never push the doctype down and
		// flip the page into quirks mode.
		afterDoctype,
	} {
		if at := anchor(body); at >= 0 {
			out := make([]byte, 0, len(body)+len(script)+1)
			out = append(out, body[:at]...)
			out = append(out, '\n')
			out = append(out, script...)
			return append(out, body[at:]...)
		}
	}
	return append([]byte(script+"\n"), body...)
}

// afterOpenTag returns the offset just past the '>' of the first live (not
// commented-out) occurrence of the given open tag, or -1.
func afterOpenTag(body []byte, tag string) int {
	i := indexTagOutsideComment(body, tag)
	if i < 0 {
		return -1
	}
	gt := bytes.IndexByte(body[i:], '>')
	if gt < 0 {
		return -1
	}
	return i + gt + 1
}

// afterDoctype returns the offset just past a leading <!DOCTYPE …> declaration,
// or 0 if there is none.
func afterDoctype(body []byte) int {
	i := 0
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\r' || body[i] == '\n') {
		i++
	}
	if !bytes.HasPrefix(bytes.ToLower(body[i:]), []byte("<!doctype")) {
		return 0
	}
	gt := bytes.IndexByte(body[i:], '>')
	if gt < 0 {
		return 0
	}
	return i + gt + 1
}

// indexTagOutsideComment returns the offset of the first occurrence of tag
// that is not inside an HTML comment, or -1 if there is none. Matching is
// case-insensitive. For a partial tag such as "<head" (no trailing '>') the
// match must end on '>' or whitespace, so "<header" does not count as "<head".
func indexTagOutsideComment(body []byte, tag string) int {
	lower := bytes.ToLower(body)
	t := []byte(tag)
	needBoundary := t[len(t)-1] != '>'
	for i := 0; i < len(lower); {
		if bytes.HasPrefix(lower[i:], []byte("<!--")) {
			end := bytes.Index(lower[i+4:], []byte("-->"))
			if end < 0 {
				return -1 // unterminated comment: nothing past it is live markup
			}
			i += 4 + end + 3
			continue
		}
		if bytes.HasPrefix(lower[i:], t) {
			n := i + len(t)
			if !needBoundary {
				return i
			}
			if n < len(lower) {
				switch lower[n] {
				case '>', ' ', '\t', '\r', '\n':
					return i
				}
			}
		}
		i++
	}
	return -1
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
