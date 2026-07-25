package websdr

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The na5b.com:8901 layout: a commented-out boilerplate block whose closing
// "</head>" is the first one in the document. Injecting before that "</head>"
// puts the tap script inside the comment, so it never runs and no audio ever
// reaches the proxy.
const commentedHeadPage = `<!DOCTYPE HTML>
<html lang="en">
<head>
<title>NA5B</title>
<!-- <style>
html { background-color: white; }
</style>
</head> -->
<body>
<script src="websdr-base.js"></script>
</body>
</html>`

// inComment reports whether offset off falls inside an HTML comment in s.
func inComment(s string, off int) bool {
	for i := 0; i < len(s); {
		start := strings.Index(s[i:], "<!--")
		if start < 0 {
			return false
		}
		start += i
		end := strings.Index(s[start+4:], "-->")
		if end < 0 {
			return off > start
		}
		end = start + 4 + end + 3
		if off > start && off < end {
			return true
		}
		i = end
	}
	return false
}

func TestInjectScript(t *testing.T) {
	const script = "<script>TAP</script>"

	tests := []struct {
		name string
		page string
	}{
		{"commented-out head", commentedHeadPage},
		{"plain page", "<html><head><title>x</title></head><body></body></html>"},
		{"head with attributes", `<html><head lang="en"></head><body></body></html>`},
		{"uppercase tags", "<HTML><HEAD></HEAD><BODY></BODY></HTML>"},
		{"body only", "<html><body><p>hi</p></body></html>"},
		{"no tags at all", "just some text"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(injectScript([]byte(tc.page), script))
			off := strings.Index(got, script)
			if off < 0 {
				t.Fatalf("script not injected:\n%s", got)
			}
			if inComment(got, off) {
				t.Errorf("script injected inside an HTML comment — it would never execute:\n%s", got)
			}
		})
	}
}

// The tap monkey-patches AudioNode.prototype.connect, so it has to be in place
// before the page's own scripts build their audio graph.
func TestInjectScriptRunsBeforePageScripts(t *testing.T) {
	const script = "<script>TAP</script>"
	got := string(injectScript([]byte(commentedHeadPage), script))

	tap := strings.Index(got, script)
	page := strings.Index(got, "websdr-base.js")
	if tap < 0 || page < 0 {
		t.Fatalf("missing markers in output:\n%s", got)
	}
	if tap > page {
		t.Errorf("tap injected after the page's own script; it would miss the audio graph:\n%s", got)
	}
}

// httputil.ReverseProxy adds X-Forwarded-For with the client address unless the
// header is explicitly set to nil. Our client is always 127.0.0.1, and servers
// that trust the header as the real client IP reject that: KiwiSDR answers 403
// Forbidden for every request, so the SDR page never even loads.
func TestDirectorSuppressesForwardedFor(t *testing.T) {
	p, err := New("http://sdr.example:8073/")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost:1234/index.html", nil)
	req.Header.Set("X-Real-IP", "127.0.0.1")
	p.director(req)

	if v, ok := req.Header["X-Forwarded-For"]; !ok || v != nil {
		t.Errorf(`req.Header["X-Forwarded-For"] = %v (present=%v), want present with nil value`, v, ok)
	}
	if got := req.Header.Get("X-Real-IP"); got != "" {
		t.Errorf("X-Real-IP = %q, want removed", got)
	}
	if got := req.Host; got != "sdr.example:8073" {
		t.Errorf("req.Host = %q, want the target host", got)
	}
}

// Start must carry the target's path/query/fragment into the URL handed to the
// browser. Dropping the path lands the user on the site root, which on some
// multi-receiver WebSDRs is not the receiver at all: websdr2.sdrutah.org:8902
// serves a "we have moved" page at "/" that redirects the browser off to
// sdrutah.org, escaping the proxy entirely.
func TestStartPreservesTargetPath(t *testing.T) {
	tests := []struct {
		target string
		want   string // expected suffix after the host:port
	}{
		{"http://websdr2.sdrutah.org:8902/index1a.html", "/index1a.html"},
		{"http://sdr.example:8901/index1a.html?tune=7030cw", "/index1a.html?tune=7030cw"},
		{"http://sdr.example:8901/", "/"},
		{"http://sdr.example:8901", "/"},
		{"http://sdr.example:8073/#freq=7030", "/#freq=7030"},
	}

	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			p, err := New(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Start()
			if err != nil {
				t.Fatal(err)
			}
			defer p.Stop()

			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("Start returned unparseable URL %q: %v", got, err)
			}
			if u.Hostname() != "localhost" {
				t.Errorf("Start host = %q, want localhost", u.Host)
			}
			rest := strings.TrimPrefix(got, "http://"+u.Host)
			if rest != tc.want {
				t.Errorf("Start = %q, path part %q, want %q", got, rest, tc.want)
			}
		})
	}
}

func TestIndexTagOutsideComment(t *testing.T) {
	// "<head>" appears only inside a comment, so there is no live one to find.
	if got := indexTagOutsideComment([]byte("<html><!-- <head> --><body>"), "<head"); got != -1 {
		t.Errorf("indexTagOutsideComment = %d, want -1 (only match is commented out)", got)
	}
	// "<header>" must not match the "<head" tag name.
	if got := indexTagOutsideComment([]byte("<header>x</header>"), "<head"); got != -1 {
		t.Errorf("indexTagOutsideComment = %d, want -1 (<header> is not <head>)", got)
	}
	// An unterminated comment swallows the rest of the document.
	if got := indexTagOutsideComment([]byte("<!-- oops <head>"), "<head"); got != -1 {
		t.Errorf("indexTagOutsideComment = %d, want -1 (unterminated comment)", got)
	}
}
