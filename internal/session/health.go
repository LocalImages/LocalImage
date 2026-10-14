package session

import (
	"archive/tar"
	"context"
	"io"
	"strconv"
	"strings"
	"time"
)

// BrowserHealth says whether a session's browser is up and how often it has had to start.
// After a crash the browser comes back by itself within a second on the same URL, but as a new
// browser with no pages, so a client needs a way to find out that it happened.
type BrowserHealth struct {
	// State is the container state: running, exited, ...
	State string `json:"state"`
	// Health is Docker's verdict, from a check that covers both the live view and the browser:
	// starting, healthy or unhealthy.
	Health string `json:"health,omitempty"`
	// Starts counts how many times the browser has been started (1 after a normal launch).
	Starts int `json:"starts"`
	// Restarts is Starts - 1: how many times it came back after a crash.
	Restarts int `json:"restarts"`
	// LastStartedAt is when the browser last started, RFC 3339, UTC.
	LastStartedAt   string `json:"last_started_at,omitempty"`
	LastStartedAtMS int64  `json:"last_started_at_ms,omitempty"`
}

// Health reports on a session's browser. It works for stopped sessions too.
func (m *Manager) Health(ctx context.Context, id string) (*BrowserHealth, error) {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := m.cli.ContainerInspect(ctx, cid)
	if err != nil {
		return nil, ErrNotFound
	}
	h := &BrowserHealth{State: info.State.Status}
	if info.State.Health != nil {
		h.Health = info.State.Health.Status
	}
	rc, _, err := m.cli.CopyFromContainer(ctx, cid, "/tmp/browser-starts")
	if err != nil {
		return h, nil // the browser has not started yet
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		return h, nil
	}
	data, _ := io.ReadAll(io.LimitReader(tr, 1<<16))
	for _, line := range strings.Fields(string(data)) {
		if ms, err := strconv.ParseInt(line, 10, 64); err == nil {
			h.Starts++
			h.LastStartedAtMS = ms
		}
	}
	if h.Starts > 0 {
		h.Restarts = h.Starts - 1
		h.LastStartedAt = time.UnixMilli(h.LastStartedAtMS).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return h, nil
}
