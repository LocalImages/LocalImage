package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, g *guard, method, target, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(method, target, strings.NewReader(`{}`))
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.wrap(ok).ServeHTTP(rec, req)
	return rec
}

func TestGuardHostAndOrigin(t *testing.T) {
	g := newGuard()
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		want    int
	}{
		{"loopback ip", "127.0.0.1:7777", nil, http.StatusNoContent},
		{"localhost", "localhost:7777", nil, http.StatusNoContent},
		{"ipv6 loopback", "[::1]:7777", nil, http.StatusNoContent},
		{"lan ip", "192.168.1.20:7777", nil, http.StatusNoContent},
		{"dns rebinding: attacker's name in Host", "attacker.test:7777", nil, http.StatusForbidden},
		{"rebinding with a port-less name", "evil.example", nil, http.StatusForbidden},
		{"cross-site origin", "127.0.0.1:7777", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"opaque origin", "127.0.0.1:7777", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"same-origin live view page", "127.0.0.1:7777", map[string]string{"Origin": "http://127.0.0.1:7777"}, http.StatusNoContent},
		{"localhost origin", "localhost:7777", map[string]string{"Origin": "http://localhost:7777"}, http.StatusNoContent},
		{"a form post cannot forge JSON", "127.0.0.1:7777", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"urlencoded form", "127.0.0.1:7777", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"multipart form", "127.0.0.1:7777", map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, http.StatusUnsupportedMediaType},
		{"json is fine", "127.0.0.1:7777", map[string]string{"Content-Type": "application/json"}, http.StatusNoContent},
		{"binary upload is fine", "127.0.0.1:7777", map[string]string{"Content-Type": "application/octet-stream"}, http.StatusNoContent},
	}
	for _, c := range cases {
		rec := serve(t, g, http.MethodPost, "/v1/sessions", c.host, c.headers)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

func TestGuardLetsReadsAndTokenPathsThrough(t *testing.T) {
	g := newGuard()
	// A GET carries no body to forge; the live view (token paths) is ordinary browser traffic.
	if rec := serve(t, g, http.MethodGet, "/v1/info", "127.0.0.1:7777", map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusNoContent {
		t.Errorf("GET with a form content type: %d", rec.Code)
	}
	if rec := serve(t, g, http.MethodPost, "/t/abc/live/api/login", "127.0.0.1:7777", map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusNoContent {
		t.Errorf("token path with a form content type: %d", rec.Code)
	}
	// ...but the Host check still applies to token paths: a rebinding page cannot reach a live view.
	if rec := serve(t, g, http.MethodGet, "/t/abc/live/", "attacker.test", nil); rec.Code != http.StatusForbidden {
		t.Errorf("rebinding against a token path: %d", rec.Code)
	}
}

func TestGuardAllowLists(t *testing.T) {
	t.Setenv("LOCALIMAGE_ALLOWED_HOSTS", "ci-runner.internal, Build.Example")
	t.Setenv("LOCALIMAGE_ALLOWED_ORIGINS", "https://dashboard.example")
	g := newGuard()
	if rec := serve(t, g, http.MethodGet, "/v1/info", "ci-runner.internal:7777", nil); rec.Code != http.StatusNoContent {
		t.Errorf("an allowed host name: %d", rec.Code)
	}
	if rec := serve(t, g, http.MethodGet, "/v1/info", "build.example", nil); rec.Code != http.StatusNoContent {
		t.Errorf("host names are case-insensitive: %d", rec.Code)
	}
	if rec := serve(t, g, http.MethodPost, "/v1/sessions", "127.0.0.1:7777", map[string]string{"Origin": "https://dashboard.example"}); rec.Code != http.StatusNoContent {
		t.Errorf("an allowed origin: %d", rec.Code)
	}
}

func TestRedactKeepsTokensOutOfTheLog(t *testing.T) {
	cases := map[string]string{
		"/t/0123456789abcdef/cdp/json/version": "/t/<redacted>/cdp/json/version",
		"/v/secrettoken/":                      "/v/<redacted>/",
		"/t/onlytoken":                         "/t/<redacted>/",
		"/v1/sessions":                         "/v1/sessions",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}
