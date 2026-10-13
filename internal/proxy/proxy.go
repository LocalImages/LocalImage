// Package proxy exposes sessions at /t/<token>/... so that one daemon port
// (and one secret per session) is all a client needs. It forwards CDP,
// Playwright-server and Neko web/signaling traffic. WebRTC media does not go
// through here: Neko requires it on directly mapped ports.
package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LocalImages/LocalImage/internal/session"
)

type cacheEntry struct {
	rec     *session.Record
	fetched time.Time
}

type Proxy struct {
	mgr   *session.Manager
	mu    sync.Mutex
	cache map[string]cacheEntry
}

func New(mgr *session.Manager) *Proxy {
	return &Proxy{mgr: mgr, cache: map[string]cacheEntry{}}
}

func (p *Proxy) lookup(r *http.Request, token string, view bool) (*session.Record, error) {
	key := token
	if view {
		key = "v:" + token
	}
	p.mu.Lock()
	e, ok := p.cache[key]
	p.mu.Unlock()
	if ok && time.Since(e.fetched) < 5*time.Second {
		return e.rec, nil
	}
	lookup := p.mgr.ByToken
	if view {
		lookup = p.mgr.ByViewToken
	}
	rec, err := lookup(r.Context(), token)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cache[key] = cacheEntry{rec: rec, fetched: time.Now()}
	p.mu.Unlock()
	return rec, nil
}

// ServeHTTP handles /t/{token}/{cdp|pw|live}/...
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v/") {
		p.serveView(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/t/")
	token, tail, _ := strings.Cut(rest, "/")
	rec, err := p.lookup(r, token, false)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind, sub, _ := strings.Cut(tail, "/")

	switch {
	case kind == "live":
		p.forward(w, r, "127.0.0.1:"+rec.WebPort, r.URL.Path, false, nil)
	case kind == "cdp" && rec.Protocol == session.ProtocolCDP:
		prefix := "/t/" + token + "/cdp/"
		p.forward(w, r, "127.0.0.1:"+rec.BackendPort, "/"+sub, true, rewriteWS(r.Host, rec.BackendPort, prefix))
	case kind == "pw" && rec.Protocol == session.ProtocolPlaywright:
		p.forward(w, r, "127.0.0.1:"+rec.BackendPort, "/pw", true, nil)
	default:
		http.NotFound(w, r)
	}
}

// serveView handles /v/<view-token>/...: the read-only live view. The view
// token cannot reach CDP or the Playwright server, and the page it serves
// only holds the viewer login, which Neko restricts to watching.
func (p *Proxy) serveView(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v/")
	token, tail, _ := strings.Cut(rest, "/")
	rec, err := p.lookup(r, token, true)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p.forward(w, r, "127.0.0.1:"+rec.WebPort, "/t/"+rec.Token+"/live/"+tail, false, nil)
}

// forward proxies to target. Chromium rejects Host headers that aren't an IP
// or localhost, so CDP requests get the backend's Host; Neko checks the
// WebSocket Origin against Host, so live requests keep the client's.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, target, path string, setHost bool, modify func(*http.Response) error) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: target, Path: path, RawQuery: r.URL.RawQuery}
			if setHost {
				pr.Out.Host = target
			} else {
				pr.Out.Host = r.Host
			}
		},
		ModifyResponse: modify,
		FlushInterval:  -1,
	}
	rp.ServeHTTP(w, r)
}

// rewriteWS rewrites the webSocketDebuggerUrl values in Chromium's /json
// responses so they point back at the proxy.
func rewriteWS(clientHost, backendPort, prefix string) func(*http.Response) error {
	return func(resp *http.Response) error {
		if !strings.HasPrefix(resp.Request.URL.Path, "/json") || resp.StatusCode != http.StatusOK {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		old := []byte("ws://127.0.0.1:" + backendPort + "/")
		body = bytes.ReplaceAll(body, old, []byte("ws://"+clientHost+prefix))
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
}
