// Package api exposes the session manager over a small REST API and mounts
// the token proxy that clients use to reach each session.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LocalImages/LocalImage/internal/proxy"
	"github.com/LocalImages/LocalImage/internal/session"
)

type Server struct {
	mgr        *session.Manager
	apiKey     string // optional; guards /v1 when set
	version    string
	onShutdown func() // called by POST /v1/shutdown; may be nil
}

func New(mgr *session.Manager, apiKey, version string, onShutdown func()) *Server {
	return &Server{mgr: mgr, apiKey: apiKey, version: version, onShutdown: onShutdown}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep & in URLs readable
	_ = enc.Encode(v)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return logged(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiKey)) != 1 {
				fail(w, http.StatusUnauthorized, errUnauthorized)
				return
			}
		}
		next(w, r)
	})
}

type apiError string

func (e apiError) Error() string { return string(e) }

const errUnauthorized = apiError("missing or invalid API key")

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", s.auth(s.create))
	mux.HandleFunc("GET /v1/info", s.auth(s.info))
	mux.HandleFunc("POST /v1/shutdown", s.auth(s.shutdown))
	mux.HandleFunc("POST /v1/images/prepare", s.auth(s.prepare))
	mux.HandleFunc("GET /v1/sessions", s.auth(s.list))
	mux.HandleFunc("GET /v1/sessions/{id}", s.auth(s.get))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.auth(s.delete))
	mux.HandleFunc("POST /v1/sessions/{id}/recording/start", s.auth(s.recStart))
	mux.HandleFunc("POST /v1/sessions/{id}/recording/stop", s.auth(s.recStop))
	mux.HandleFunc("GET /v1/sessions/{id}/recording", s.auth(s.recDownload))
	mux.HandleFunc("GET /v1/sessions/{id}/recordings", s.auth(s.recList))
	mux.HandleFunc("GET /v1/sessions/{id}/recordings/{rid}", s.auth(s.recDownload))
	mux.HandleFunc("POST /v1/sessions/{id}/process/exec", s.auth(s.exec))
	mux.HandleFunc("PUT /v1/sessions/{id}/files", s.auth(s.fileUpload))
	mux.HandleFunc("GET /v1/sessions/{id}/files", s.auth(s.fileDownload))
	mux.HandleFunc("GET /v1/sessions/{id}/files/list", s.auth(s.fileList))
	mux.HandleFunc("GET /v1/sessions/{id}/screenshot", s.auth(s.screenshot))
	mux.HandleFunc("GET /v1/sessions/{id}/logs", s.auth(s.logs))
	mux.HandleFunc("GET /v1/sessions/{id}/health", s.auth(s.health))
	mux.HandleFunc("PATCH /v1/sessions/{id}/tags", s.auth(s.updateTags))
	mux.HandleFunc("POST /v1/images/prune", s.auth(s.prune))
	mux.HandleFunc("POST /v1/sessions/{id}/computer/{action}", s.auth(s.computer))
	mux.HandleFunc("POST /v1/extensions", s.auth(s.extUpload))
	mux.HandleFunc("GET /v1/extensions", s.auth(s.extList))
	mux.HandleFunc("DELETE /v1/extensions/{name}", s.auth(s.extDelete))
	mux.HandleFunc("GET /v1/profiles", s.auth(s.profiles))
	mux.HandleFunc("DELETE /v1/profiles/{name}", s.auth(s.deleteProfile))
	// pid lets a caller that just started a daemon tell whether it is the one that answered.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "pid": os.Getpid()})
	})
	// The session token in the path is the credential for everything under /t/.
	px := proxy.New(s.mgr)
	mux.Handle("/t/{token}/", px)
	mux.Handle("/v/{token}/", px)
	return newGuard().wrap(mux)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Browser    string   `json:"browser"`
		Screen     string   `json:"screen"`
		TTLSeconds int      `json:"ttl_seconds"`
		Profile    string   `json:"profile"`
		MemoryMB   int      `json:"memory_mb"`
		Kiosk      bool     `json:"kiosk"`
		Extensions []string `json:"extensions"`
		CPUs       float64  `json:"cpus"`
		// Tags are caller metadata stored on the session and usable as list filters.
		Tags map[string]string `json:"tags"`
		// WaitSeconds queues the request for a free slot instead of failing at once.
		WaitSeconds int `json:"wait_seconds"`
		// NoBuild fails with image_not_ready instead of building a missing browser image.
		NoBuild bool `json:"no_build"`
		// WebGL turns on WebGL (software rendering, Chromium only); Timezone and Locale set the
		// browser's timezone (IANA name) and language.
		WebGL    bool   `json:"webgl"`
		Timezone string `json:"timezone"`
		Locale   string `json:"locale"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	if req.WaitSeconds < 0 || req.WaitSeconds > 600 {
		fail(w, http.StatusBadRequest, apiError("wait_seconds must be between 0 and 600"))
		return
	}
	rec, err := s.mgr.Create(r.Context(), session.CreateOptions{
		Browser:    req.Browser,
		Screen:     req.Screen,
		TTL:        time.Duration(req.TTLSeconds) * time.Second,
		Profile:    req.Profile,
		Memory:     req.MemoryMB,
		Kiosk:      req.Kiosk,
		Extensions: req.Extensions,
		CPUs:       req.CPUs,
		Tags:       req.Tags,
		Wait:       time.Duration(req.WaitSeconds) * time.Second,
		NoBuild:    req.NoBuild,
		WebGL:      req.WebGL,
		Timezone:   req.Timezone,
		Locale:     req.Locale,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec.View(r.Host))
}

// prepare builds the browser images that are missing, so the first session does not have to.
// The response is a stream of newline-delimited JSON events, because a build can take minutes:
//
//	{"event":"progress","browser":"firefox","message":"Step 3/9 : ..."}
//	{"event":"ping"}                                   every 10 s, so no client times out
//	{"event":"done","images":{"chromium":"ready","firefox":"built"}}
//	{"event":"error","error":"...","code":"..."}       the last line when it failed
//
// The headers go out at once, so the status is always 200: read the last event for the outcome.
// If the client disconnects, the build is cancelled.
func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Browsers []string `json:"browsers"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, apiError("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		flusher.Flush()
	}
	send(map[string]string{"event": "ping"}) // flush the headers now

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				send(map[string]string{"event": "ping"})
			}
		}
	}()
	defer close(done)

	images, err := s.mgr.Prepare(r.Context(), req.Browsers, func(browser, msg string) {
		send(map[string]string{"event": "progress", "browser": browser, "message": msg})
	})
	if err != nil {
		_, code, _ := classify(err, http.StatusInternalServerError)
		send(map[string]any{"event": "error", "error": err.Error(), "code": code, "images": images})
		return
	}
	send(map[string]any{"event": "done", "images": images})
}

// shutdown stops the daemon gracefully. Sessions keep running as containers; they are
// removed when their TTL passes and a daemon is running again, or with `localimage rm`.
func (s *Server) shutdown(w http.ResponseWriter, _ *http.Request) {
	if s.onShutdown == nil {
		fail(w, http.StatusNotFound, apiError("shutdown is not available"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting down"})
	go func() {
		time.Sleep(100 * time.Millisecond) // let the response go out first
		s.onShutdown()
	}()
}

// logs returns the end of one of a session's logs as text: ?source=container|chromium|firefox|
// neko|xorg|openbox|... (default container) and ?tail=N lines (default 200). It also works for a
// stopped session, to see why it died.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fail(w, http.StatusBadRequest, apiError("tail must be a number"))
			return
		}
		tail = n
	}
	text, err := s.mgr.Logs(r.Context(), r.PathValue("id"), r.URL.Query().Get("source"), tail)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// health reports on a session's browser: container state, Docker health, and how many times the
// browser has started (restarts > 0 means it crashed and came back as a new, empty browser).
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h, err := s.mgr.Health(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// updateTags changes a session's tags after creation: {"set": {...}, "remove": [...]}.
func (s *Server) updateTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Set    map[string]string `json:"set"`
		Remove []string          `json:"remove"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	tags, err := s.mgr.UpdateTags(r.Context(), r.PathValue("id"), req.Set, req.Remove)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// prune removes browser images left behind by older versions of localimage.
func (s *Server) prune(w http.ResponseWriter, r *http.Request) {
	removed, err := s.mgr.Prune(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// parseTagFilters turns repeated ?tag=key=value parameters into a filter map.
func parseTagFilters(vals []string) (map[string]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	tags := make(map[string]string, len(vals))
	for _, v := range vals {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, apiError("tag filters look like ?tag=key=value, got " + strconv.Quote(v))
		}
		tags[k] = val
	}
	return tags, nil
}

// info reports the daemon's version and limits, so a client can check capacity
// (max_sessions, running, available) before it starts many sessions at once.
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	info, err := s.mgr.Info(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Version string `json:"version"`
		*session.Info
	}{s.version, info})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	tags, err := parseTagFilters(r.URL.Query()["tag"])
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	recs, err := s.mgr.List(r.Context(), tags)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]*session.Session, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.View(r.Host))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	rec, err := s.mgr.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, rec.View(r.Host))
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.Context(), r.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxSeconds int `json:"max_seconds"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	info, err := s.mgr.StartRecording(r.Context(), r.PathValue("id"), req.MaxSeconds)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, info)
}

func (s *Server) recStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReplayID string `json:"replay_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	info, err := s.mgr.StopRecording(r.Context(), r.PathValue("id"), req.ReplayID)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) recList(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Recordings(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// recDownload serves one recording; without {rid} it serves the latest.
func (s *Server) recDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var buf bytes.Buffer
	if _, err := s.mgr.DownloadRecording(r.Context(), id, r.PathValue("rid"), &buf); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.mp4"`)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command        []string `json:"command"`
		Cwd            string   `json:"cwd"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	res, err := s.mgr.Exec(r.Context(), r.PathValue("id"), req.Command, req.Cwd, req.TimeoutSeconds)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request) {
	n, err := s.mgr.UploadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), http.MaxBytesReader(w, r.Body, 101<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"size": n})
}

func (s *Server) fileDownload(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if _, err := s.mgr.DownloadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), &buf); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) fileList(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.ListFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// extUpload takes a zip of an unpacked extension as the request body.
func (s *Server) extUpload(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 51<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ext, err := s.mgr.AddExtension(r.Context(), r.URL.Query().Get("name"), data)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, ext)
}

func (s *Server) extList(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Extensions(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) extDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteExtension(r.Context(), r.PathValue("name")); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	var region *session.Region
	if q := r.URL.Query(); q.Get("width") != "" {
		n := func(k string) int { v, _ := strconv.Atoi(q.Get(k)); return v }
		region = &session.Region{X: n("x"), Y: n("y"), Width: n("width"), Height: n("height")}
	}
	png, err := s.mgr.Screenshot(r.Context(), r.PathValue("id"), region)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	names, err := s.mgr.Profiles(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteProfile(r.Context(), r.PathValue("name")); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// computer handles POST /v1/sessions/{id}/computer/{click,move,type,key,scroll,drag}.
func (s *Server) computer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		X, Y    int
		Button  string   `json:"button"`
		Count   int      `json:"num_clicks"`
		Text    string   `json:"text"`
		DelayMS int      `json:"delay_ms"`
		Keys    []string `json:"keys"`
		DeltaX  int      `json:"delta_x"`
		DeltaY  int      `json:"delta_y"`
		Path    [][2]int `json:"path"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, bodyStatus(err), err)
		return
	}
	id, pt := r.PathValue("id"), session.Point{X: req.X, Y: req.Y}
	var err error
	switch r.PathValue("action") {
	case "click":
		err = s.mgr.Click(r.Context(), id, pt, req.Button, req.Count)
	case "move":
		err = s.mgr.Move(r.Context(), id, pt)
	case "type":
		err = s.mgr.TypeText(r.Context(), id, req.Text, req.DelayMS)
	case "key":
		err = s.mgr.PressKeys(r.Context(), id, req.Keys)
	case "scroll":
		err = s.mgr.Scroll(r.Context(), id, pt, req.DeltaX, req.DeltaY)
	case "drag":
		path := make([]session.Point, len(req.Path))
		for i, p := range req.Path {
			path[i] = session.Point{X: p[0], Y: p[1]}
		}
		err = s.mgr.Drag(r.Context(), id, path, req.Button)
	default:
		fail(w, http.StatusNotFound, apiError("unknown action"))
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RunReaper deletes expired sessions until ctx is cancelled.
func RunReaper(ctx context.Context, mgr *session.Manager) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mgr.Reap(ctx)
		}
	}
}

// bodyStatus is 413 for an oversized body and 400 for a malformed one.
func bodyStatus(err error) int {
	if errors.Is(err, errTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
