package api

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// guard protects the daemon from web pages in the user's own browser.
//
// A page on any site can send requests to http://127.0.0.1:7777. Without checks it could create
// sessions or stop the daemon (a "simple" cross-site POST needs no preflight), and with DNS
// rebinding it can read responses too, because the browser then believes the daemon is the
// page's own origin. The checks:
//
//   - Host must be an IP address, localhost, or listed in LOCALIMAGE_ALLOWED_HOSTS. A rebinding
//     attack arrives with the attacker's domain name in Host, so it is refused.
//   - A request carrying an Origin header (browsers add it to cross-site requests) is refused
//     unless that origin's host is allowed. The SDK, CLI and Playwright send no Origin.
//   - Writes to /v1 must not use a content type a browser can send without a preflight
//     (text/plain, form types), so JSON bodies cannot be forged cross-site.
type guard struct {
	hosts   map[string]bool
	origins map[string]bool
}

func newGuard() *guard {
	g := &guard{hosts: map[string]bool{"localhost": true}, origins: map[string]bool{}}
	for _, h := range strings.Split(os.Getenv("LOCALIMAGE_ALLOWED_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			g.hosts[h] = true
		}
	}
	for _, o := range strings.Split(os.Getenv("LOCALIMAGE_ALLOWED_ORIGINS"), ",") {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			g.origins[o] = true
		}
	}
	return g
}

// hostOnly strips the port and IPv6 brackets from a Host or origin authority.
func hostOnly(authority string) string {
	h, _, err := net.SplitHostPort(authority)
	if err != nil {
		h = authority
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func (g *guard) hostAllowed(authority string) bool {
	h := hostOnly(authority)
	return h != "" && (net.ParseIP(h) != nil || g.hosts[h])
}

func (g *guard) originAllowed(origin string) bool {
	if g.origins[strings.ToLower(origin)] {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && g.hostAllowed(u.Host)
}

// simpleTypes are what a browser may send cross-site without asking the server first.
var simpleTypes = map[string]bool{
	"text/plain":                        true,
	"application/x-www-form-urlencoded": true,
	"multipart/form-data":               true,
}

func (g *guard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r.Host) {
			refuse(w, r, http.StatusForbidden, apiError("this Host is not allowed; set LOCALIMAGE_ALLOWED_HOSTS to permit it"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && !g.originAllowed(origin) {
			refuse(w, r, http.StatusForbidden, apiError("cross-origin requests are not allowed; set LOCALIMAGE_ALLOWED_ORIGINS to permit one"))
			return
		}
		if origin := r.Header.Get("Origin"); origin == "null" && strings.HasPrefix(r.URL.Path, "/v1/") {
			refuse(w, r, http.StatusForbidden, apiError("requests from opaque origins are not allowed"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
			if simpleTypes[ct] {
				refuse(w, r, http.StatusUnsupportedMediaType, apiError("send JSON (application/json) or a binary type, not "+ct))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// refuse rejects a request and records why, so a blocked page leaves a trace in the daemon log.
func refuse(w http.ResponseWriter, r *http.Request, status int, err apiError) {
	slog.Warn("request refused", "reason", string(err), "host", r.Host, "origin", r.Header.Get("Origin"), "method", r.Method, "path", redact(r.URL.Path))
	fail(w, status, err)
}

// redact keeps session tokens out of the log: /t/<token>/... becomes /t/<redacted>/...
func redact(path string) string {
	for _, prefix := range []string{"/t/", "/v/"} {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			_, tail, _ := strings.Cut(rest, "/")
			return prefix + "<redacted>/" + tail
		}
	}
	return path
}
