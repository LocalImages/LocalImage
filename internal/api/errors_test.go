package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LocalImages/LocalImage/internal/session"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		fallback int
		status   int
		code     string
	}{
		{"limit error", &session.LimitError{Max: 4, Active: 4}, 500, 429, "limit_exceeded"},
		{"wrapped limit", fmt.Errorf("ttl: %w", session.ErrLimit), 500, 429, "limit_exceeded"},
		{"session gone", session.ErrNotFound, 500, 404, "session_gone"},
		{"wrapped gone", fmt.Errorf("lookup: %w", session.ErrNotFound), 400, 404, "session_gone"},
		{"invalid", fmt.Errorf("bad: %w", session.ErrInvalid), 500, 400, "invalid_request"},
		{"conflict", fmt.Errorf("in use: %w", session.ErrConflict), 500, 409, "conflict"},
		{"image not ready", fmt.Errorf("x: %w", session.ErrNotReady), 500, 409, "image_not_ready"},
		{"docker down", errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), 500, 503, "docker_unavailable"},
		{"fallback 400", errors.New("x"), 400, 400, "invalid_request"},
		{"fallback 401", errors.New("x"), 401, 401, "unauthorized"},
		{"fallback 404", errors.New("x"), 404, 404, "not_found"},
		{"fallback 500", errors.New("x"), 500, 500, "internal"},
	}
	for _, c := range cases {
		status, code, _ := classify(c.err, c.fallback)
		if status != c.status || code != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, status, code, c.status, c.code)
		}
	}

	_, _, details := classify(&session.LimitError{Max: 4, Active: 3}, 500)
	d, ok := details.(map[string]int)
	if !ok || d["max"] != 4 || d["running"] != 3 {
		t.Errorf("limit details = %v", details)
	}
}

func TestFailWritesStableBody(t *testing.T) {
	rec := httptest.NewRecorder()
	fail(rec, http.StatusInternalServerError, &session.LimitError{Max: 2, Active: 2})
	if rec.Code != 429 {
		t.Errorf("status = %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 should carry Retry-After")
	}
	body := rec.Body.String()
	for _, want := range []string{`"code":"limit_exceeded"`, `"max":2`, `"running":2`, `"error":"session limit reached`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s does not contain %s", body, want)
		}
	}
}

func TestParseTagFilters(t *testing.T) {
	got, err := parseTagFilters([]string{"e2e_run=r1", "target=a=b"})
	if err != nil || got["e2e_run"] != "r1" || got["target"] != "a=b" {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := parseTagFilters(nil); got != nil || err != nil {
		t.Errorf("no filters: got %v, %v", got, err)
	}
	for _, bad := range []string{"novalue", "=v"} {
		if _, err := parseTagFilters([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
