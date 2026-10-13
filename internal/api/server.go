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
	"strconv"
	"strings"
	"time"

	"github.com/LocalImages/LocalImage/internal/proxy"
	"github.com/LocalImages/LocalImage/internal/session"
)

type Server struct {
	mgr    *session.Manager
	apiKey string // optional; guards /v1 when set
}

func New(mgr *session.Manager, apiKey string) *Server { return &Server{mgr: mgr, apiKey: apiKey} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep & in URLs readable
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiKey)) != 1 {
				writeErr(w, http.StatusUnauthorized, errUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

type apiError string

func (e apiError) Error() string { return string(e) }

const errUnauthorized = apiError("missing or invalid API key")

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", s.auth(s.create))
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
	mux.HandleFunc("POST /v1/sessions/{id}/computer/{action}", s.auth(s.computer))
	mux.HandleFunc("POST /v1/extensions", s.auth(s.extUpload))
	mux.HandleFunc("GET /v1/extensions", s.auth(s.extList))
	mux.HandleFunc("DELETE /v1/extensions/{name}", s.auth(s.extDelete))
	mux.HandleFunc("GET /v1/profiles", s.auth(s.profiles))
	mux.HandleFunc("DELETE /v1/profiles/{name}", s.auth(s.deleteProfile))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	// The session token in the path is the credential for everything under /t/.
	px := proxy.New(s.mgr)
	mux.Handle("/t/{token}/", px)
	mux.Handle("/v/{token}/", px)
	return mux
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
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
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
	})
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, session.ErrLimit) {
			code = http.StatusTooManyRequests
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec.View(r.Host))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	recs, err := s.mgr.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
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
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, rec.View(r.Host))
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recStart(w http.ResponseWriter, r *http.Request) {
	rid, err := s.mgr.StartRecording(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"replay_id": rid, "status": "recording"})
}

func (s *Server) recStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReplayID string `json:"replay_id"`
	}
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	rid, err := s.mgr.StopRecording(r.Context(), r.PathValue("id"), req.ReplayID)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"replay_id": rid, "status": "stopped"})
}

func (s *Server) recList(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Recordings(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// recDownload serves one recording; without {rid} it serves the latest.
func (s *Server) recDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var buf bytes.Buffer
	if _, err := s.mgr.DownloadRecording(r.Context(), id, r.PathValue("rid"), &buf); err != nil {
		writeErr(w, http.StatusNotFound, err)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.mgr.Exec(r.Context(), r.PathValue("id"), req.Command, req.Cwd, req.TimeoutSeconds)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request) {
	n, err := s.mgr.UploadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"size": n})
}

func (s *Server) fileDownload(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if _, err := s.mgr.DownloadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), &buf); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) fileList(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.ListFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// extUpload takes a zip of an unpacked extension as the request body.
func (s *Server) extUpload(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 51<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ext, err := s.mgr.AddExtension(r.Context(), r.URL.Query().Get("name"), data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, ext)
}

func (s *Server) extList(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Extensions(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) extDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteExtension(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusConflict, err)
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
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	names, err := s.mgr.Profiles(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteProfile(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusConflict, err)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
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
		writeErr(w, http.StatusNotFound, apiError("unknown action"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
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
