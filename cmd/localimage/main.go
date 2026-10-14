package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/LocalImages/LocalImage/internal/api"
	"github.com/LocalImages/LocalImage/internal/auth"
	"github.com/LocalImages/LocalImage/internal/session"
)

const defaultAddr = "127.0.0.1:7777"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	addr := os.Getenv("LOCALIMAGE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	switch os.Args[1] {
	case "help", "-h", "--help":
		fmt.Print(helpText)
	case "doctor":
		doctor(addr)
	case "version", "--version":
		fmt.Println(version)
	case "start":
		fs := flag.NewFlagSet("start", flag.ExitOnError)
		fs.StringVar(&addr, "addr", addr, "listen address")
		fs.Parse(os.Args[2:])
		start(addr)
	case "create":
		fs := flag.NewFlagSet("create", flag.ExitOnError)
		browser := fs.String("browser", "chromium", "browser to run")
		ttl := fs.Int("ttl", 1800, "session lifetime in seconds")
		profile := fs.String("profile", "", "named persistent profile (chromium only)")
		memory := fs.Int("memory", 0, "memory limit in MiB")
		cpus := fs.Float64("cpus", 0, "CPU limit")
		kiosk := fs.Bool("kiosk", false, "chromium: fullscreen without tabs or address bar")
		wait := fs.Int("wait", 0, "seconds to queue for a free session slot (0 = fail at once)")
		webgl := fs.Bool("webgl", false, "chromium: enable WebGL (software rendering)")
		timezone := fs.String("timezone", "", "IANA timezone, for example Europe/Berlin")
		locale := fs.String("locale", "", "browser language, for example de-DE (chromium)")
		noBuild := fs.Bool("no-build", false, "fail with image_not_ready instead of building a missing browser image")
		var exts, tags stringList
		fs.Var(&exts, "extension", "chromium: uploaded extension to load (repeatable)")
		fs.Var(&tags, "tag", "key=value metadata, usable to filter `ls` (repeatable)")
		fs.Parse(os.Args[2:])
		tagMap := map[string]string{}
		for _, t := range tags {
			k, v, ok := strings.Cut(t, "=")
			if !ok || k == "" {
				fmt.Fprintf(os.Stderr, "localimage: -tag expects key=value, got %q\n", t)
				os.Exit(2)
			}
			tagMap[k] = v
		}
		body, _ := json.Marshal(map[string]any{
			"browser": *browser, "ttl_seconds": *ttl,
			"profile": *profile, "memory_mb": *memory, "cpus": *cpus,
			"kiosk": *kiosk, "extensions": []string(exts),
			"tags": tagMap, "wait_seconds": *wait, "no_build": *noBuild,
			"webgl": *webgl, "timezone": *timezone, "locale": *locale,
		})
		call(addr, http.MethodPost, "/v1/sessions", body)
	case "ls":
		fs := flag.NewFlagSet("ls", flag.ExitOnError)
		var tags stringList
		fs.Var(&tags, "tag", "only sessions with this key=value tag (repeatable)")
		fs.Parse(os.Args[2:])
		q := url.Values{}
		for _, t := range tags {
			q.Add("tag", t)
		}
		path := "/v1/sessions"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		call(addr, http.MethodGet, path, nil)
	case "info":
		call(addr, http.MethodGet, "/v1/info", nil)
	case "get", "attach":
		need(3)
		call(addr, http.MethodGet, "/v1/sessions/"+os.Args[2], nil)
	case "health":
		need(3)
		call(addr, http.MethodGet, "/v1/sessions/"+os.Args[2]+"/health", nil)
	case "recordings":
		need(3)
		call(addr, http.MethodGet, "/v1/sessions/"+os.Args[2]+"/recordings", nil)
	case "logs":
		need(3)
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		source := fs.String("source", "", "container (default), chromium, firefox, neko, xorg, openbox, pulseaudio, cdp-forward")
		tail := fs.Int("tail", 200, "number of lines")
		fs.Parse(os.Args[3:])
		q := url.Values{"tail": {fmt.Sprint(*tail)}}
		if *source != "" {
			q.Set("source", *source)
		}
		download(addr, "/v1/sessions/"+os.Args[2]+"/logs?"+q.Encode(), "-")
	case "download":
		need(4)
		fs := flag.NewFlagSet("download", flag.ExitOnError)
		out := fs.String("o", filepath.Base(os.Args[3]), "output file (- for stdout)")
		fs.Parse(os.Args[4:])
		download(addr, "/v1/sessions/"+os.Args[2]+"/files?"+url.Values{"path": {os.Args[3]}}.Encode(), *out)
	case "upload":
		need(5)
		upload(addr, os.Args[2], os.Args[3], os.Args[4])
	case "tag":
		need(3)
		fs := flag.NewFlagSet("tag", flag.ExitOnError)
		var set, remove stringList
		fs.Var(&set, "set", "key=value to set (repeatable)")
		fs.Var(&remove, "remove", "key to remove (repeatable)")
		fs.Parse(os.Args[3:])
		setMap := map[string]string{}
		for _, kv := range set {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				fmt.Fprintf(os.Stderr, "localimage: -set expects key=value, got %q\n", kv)
				os.Exit(2)
			}
			setMap[k] = v
		}
		body, _ := json.Marshal(map[string]any{"set": setMap, "remove": []string(remove)})
		call(addr, http.MethodPatch, "/v1/sessions/"+os.Args[2]+"/tags", body)
	case "prune":
		call(addr, http.MethodPost, "/v1/images/prune", nil)
	case "pull", "prepare":
		pull(addr, os.Args[2:])
	case "stop":
		call(addr, http.MethodPost, "/v1/shutdown", nil)
	case "exec":
		// localimage exec <id> -- <command> [args...]
		if len(os.Args) < 5 || os.Args[3] != "--" {
			usage()
		}
		body, _ := json.Marshal(map[string]any{"command": os.Args[4:]})
		call(addr, http.MethodPost, "/v1/sessions/"+os.Args[2]+"/process/exec", body)
	case "extension":
		extension(addr, os.Args[2:])
	case "profile":
		if len(os.Args) >= 3 && os.Args[2] == "ls" {
			call(addr, http.MethodGet, "/v1/profiles", nil)
		} else if len(os.Args) >= 4 && os.Args[2] == "rm" {
			call(addr, http.MethodDelete, "/v1/profiles/"+os.Args[3], nil)
		} else {
			usage()
		}
	case "screenshot":
		if len(os.Args) < 3 {
			usage()
		}
		fs := flag.NewFlagSet("screenshot", flag.ExitOnError)
		out := fs.String("o", os.Args[2]+".png", "output file")
		fs.Parse(os.Args[3:])
		download(addr, "/v1/sessions/"+os.Args[2]+"/screenshot", *out)
	case "record":
		record(addr, os.Args[2:])
	case "rm":
		if len(os.Args) < 3 {
			usage()
		}
		call(addr, http.MethodDelete, "/v1/sessions/"+os.Args[2], nil)
	default:
		usage()
	}
}

const helpText = `localimage - run headful browsers in Docker with live view, locally

Usage: localimage <command> [arguments]

Daemon
  start [-addr host:port]      run the daemon (default 127.0.0.1:7777)
  stop                         stop the daemon (sessions keep running until their TTL)
  pull [chromium|firefox ...]  build the browser images now (default: chromium), so no session has to
  doctor                       check Docker, images and the daemon

Sessions
  create [flags]               start a browser session and print its URLs
      -browser chromium|firefox   -ttl seconds   -profile name (chromium)
      -extension name (repeatable, chromium)   -kiosk (chromium)
      -memory MiB   -cpus n   -tag key=value (repeatable)
      -webgl (chromium)   -timezone Europe/Berlin   -locale de-DE (chromium)
      -wait seconds           queue for a free slot instead of failing at once
      -no-build               fail with image_not_ready instead of building a missing image
  ls [-tag key=value ...]      list sessions (all states), optionally filtered by tag
  info                         show the daemon's version, limits and current load
  rm <id>                      delete a session
  exec <id> -- <cmd...>        run a command inside the session
  screenshot <id> [-o file]    save a PNG of the display
  record start|stop <id>       start or stop an MP4 recording (start -max seconds, default 3600)
  recordings <id>              list a session's recordings
  get <id>                     show one session (URLs, state, health, tags)
  health <id>                  browser state and how many times it restarted
  logs <id> [-source s] [-tail n]  the end of a session's log (works for stopped sessions)
  download <id> <path> [-o file]   read a file from the session (downloads are in /home/neko/Downloads)
  upload <id> <local file> <path>  copy a file into the session
  tag <id> -set k=v -remove k  change a session's tags
  prune                        remove browser images left by older versions
  record get <id> [-o file]    download the latest recording

Saved state
  profile ls | profile rm <name>
  extension upload <name> <dir-or-zip> | extension ls | extension rm <name>

Environment
  LOCALIMAGE_ADDR              daemon address (default 127.0.0.1:7777)
  LOCALIMAGE_API_KEY           the control API key (default: generated into a private file the CLI and
                               SDKs read; LOCALIMAGE_NO_AUTH=1 turns authentication off)
  LOCALIMAGE_ALLOWED_HOSTS     extra Host names the daemon answers to (comma separated)
  LOCALIMAGE_ALLOWED_ORIGINS   extra browser origins allowed to call the API
  LOCALIMAGE_LOG_LEVEL         debug, info (default), warn or error
  LOCALIMAGE_MAX_RECORDING_SECONDS  default recording time limit (default 3600)
  LOCALIMAGE_PUBLIC_IP         address clients use for WebRTC media (default 127.0.0.1)
  LOCALIMAGE_MAX_SESSIONS      concurrent session limit (default 10)
  LOCALIMAGE_MAX_TTL_SECONDS   longest allowed session lifetime (default 86400)
`

// need exits with the usage text unless at least n arguments (including the program name and
// the command) were given.
func need(n int) {
	if len(os.Args) < n {
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, helpText)
	os.Exit(2)
}

// doctor reports whether the environment can run sessions.
func doctor(addr string) {
	ok := true
	pass := func(f string, a ...any) { fmt.Printf("  ok    "+f+"\n", a...) }
	warn := func(f string, a ...any) { fmt.Printf("  note  "+f+"\n", a...) }
	fail := func(f string, a ...any) { ok = false; fmt.Printf("  FAIL  "+f+"\n", a...) }

	fmt.Println("localimage", version)
	mgr, err := session.New()
	var ver string
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ver, err = mgr.Ping(ctx)
	}
	if err != nil {
		fail("Docker is not reachable: %v", err)
		fmt.Println("        Install Docker and make sure it is running (try `docker ps`).")
		fmt.Println("        On Linux your user needs access to the Docker socket (docker group).")
		os.Exit(1)
	}
	pass("Docker %s", ver)

	ctx := context.Background()
	for _, b := range []string{"chromium", "firefox"} {
		if mgr.ImageReady(ctx, b) {
			pass("%s image is built", b)
		} else {
			warn("%s image is not built yet; it is built on first use (needs internet, takes 1-4 minutes)", b)
		}
	}
	if n := mgr.StaleImageCount(ctx); n > 0 {
		warn("%d browser images from older versions are on disk; `localimage prune` removes them", n)
	}
	if os.Getenv("LOCALIMAGE_NO_AUTH") == "1" {
		warn("authentication is off (LOCALIMAGE_NO_AUTH=1): any local process or web page that can reach the daemon can use it")
	} else if os.Getenv("LOCALIMAGE_API_KEY") == "" {
		if p, err := auth.Path(); err == nil {
			pass("API key file: %s", p)
		}
	}
	maxS, maxTTL := mgr.Limits()
	pass("limits: %d sessions, ttl up to %s", maxS, maxTTL)

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		warn("daemon is not running at %s (start it with `localimage start`)", addr)
	} else {
		resp.Body.Close()
		pass("daemon is running at %s", addr)
	}
	if !strings.HasPrefix(addr, "127.0.0.1") && !strings.HasPrefix(addr, "localhost") && os.Getenv("LOCALIMAGE_API_KEY") == "" {
		fail("%s is not a loopback address and LOCALIMAGE_API_KEY is not set; anyone who can reach it can run commands in your sessions", addr)
	}
	if ok {
		fmt.Println("ready")
		return
	}
	os.Exit(1)
}

// setupLogging sets the daemon's log level from LOCALIMAGE_LOG_LEVEL (debug, info, warn, error;
// default info). Lines go to stderr, which `ensureDaemon` captures in the daemon log file.
func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOCALIMAGE_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func start(addr string) {
	setupLogging()
	mgr, err := session.New()
	if err == nil {
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = mgr.Ping(pctx)
		cancel()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "localimage: cannot reach Docker: %v\nIs Docker installed and running? Run `localimage doctor` for details.\n", err)
		os.Exit(1)
	}
	if !strings.HasPrefix(addr, "127.0.0.1") && !strings.HasPrefix(addr, "localhost") && os.Getenv("LOCALIMAGE_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "localimage: refusing to listen on a non-loopback address without LOCALIMAGE_API_KEY set")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go api.RunReaper(ctx, mgr)

	key := os.Getenv("LOCALIMAGE_API_KEY")
	if key == "" && os.Getenv("LOCALIMAGE_NO_AUTH") != "1" {
		// No key configured: use a generated one in a file only this user can read. The CLI and
		// SDKs read it too, so nothing needs setting up, but other users and web pages cannot call us.
		k, err := auth.LoadOrCreate()
		if err != nil {
			// Never fall back to an open API silently: that would turn the protection off exactly
			// when something unusual (no home directory, a read-only disk) is going on.
			fmt.Fprintf(os.Stderr, "localimage: cannot set up the API key file (%v).\nSet LOCALIMAGE_API_KEY to a key of your choice, or LOCALIMAGE_NO_AUTH=1 to run without authentication.\n", err)
			os.Exit(1)
		}
		key = k
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(mgr, key, version, stop).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 16,
	}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	fmt.Println("localimage daemon listening on http://" + addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func call(addr, method, path string, body []byte) {
	req, _ := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if k := auth.Resolve(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable (run `localimage start`):", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "%s\n", out)
		os.Exit(1)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		fmt.Println(pretty.String())
	}
}

func record(addr string, args []string) {
	if len(args) < 2 {
		usage()
	}
	action, id := args[0], args[1]
	switch action {
	case "start":
		fs := flag.NewFlagSet("record start", flag.ExitOnError)
		max := fs.Int("max", 0, "stop by itself after this many seconds (default 3600)")
		fs.Parse(args[2:])
		body, _ := json.Marshal(map[string]any{"max_seconds": *max})
		call(addr, http.MethodPost, "/v1/sessions/"+id+"/recording/start", body)
	case "stop":
		call(addr, http.MethodPost, "/v1/sessions/"+id+"/recording/stop", nil)
	case "get":
		fs := flag.NewFlagSet("record get", flag.ExitOnError)
		out := fs.String("o", id+".mp4", "output file")
		fs.Parse(args[2:])
		download(addr, "/v1/sessions/"+id+"/recording", *out)
	default:
		usage()
	}
}

func download(addr, path, out string) {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if k := auth.Resolve(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable (run `localimage start`):", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "%s\n", b)
		os.Exit(1)
	}
	if out == "-" {
		_, _ = io.Copy(os.Stdout, resp.Body)
		return
	}
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	n, _ := io.Copy(f, resp.Body)
	fmt.Printf("saved %s (%d bytes)\n", out, n)
}

type stringList []string

func (l *stringList) String() string     { return fmt.Sprint([]string(*l)) }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// extension manages uploaded Chromium extensions.
func extension(addr string, args []string) {
	switch {
	case len(args) == 1 && args[0] == "ls":
		call(addr, http.MethodGet, "/v1/extensions", nil)
	case len(args) == 2 && args[0] == "rm":
		call(addr, http.MethodDelete, "/v1/extensions/"+args[1], nil)
	case len(args) == 3 && args[0] == "upload":
		data, err := zipSource(args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		call(addr, http.MethodPost, "/v1/extensions?name="+url.QueryEscape(args[1]), data)
	default:
		usage()
	}
}

// zipSource returns the bytes of a zip: the file itself, or a directory zipped up.
func zipSource(p string) ([]byte, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return os.ReadFile(p)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(p, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Close first: it writes the central directory that buf.Bytes() must include.
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pull builds the browser images ahead of time, printing progress as it goes.
func pull(addr string, browsers []string) {
	body, _ := json.Marshal(map[string]any{"browsers": browsers})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/images/prepare", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if k := auth.Resolve(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable (run `localimage start`):", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "%s\n", b)
		os.Exit(1)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Event, Browser, Message, Error, Code string
			Images                               map[string]string
		}
		if err := dec.Decode(&ev); err != nil {
			fmt.Fprintln(os.Stderr, "the daemon closed the connection before the build finished")
			os.Exit(1)
		}
		switch ev.Event {
		case "progress":
			fmt.Printf("[%s] %s\n", ev.Browser, ev.Message)
		case "error":
			fmt.Fprintf(os.Stderr, "failed (%s): %s\n", ev.Code, ev.Error)
			os.Exit(1)
		case "done":
			for b, st := range ev.Images {
				fmt.Printf("%s: %s\n", b, st)
			}
			return
		}
	}
}

// upload copies a local file into a session.
func upload(addr, id, local, remote string) {
	data, err := os.ReadFile(local)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	req, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/v1/sessions/"+id+"/files?"+url.Values{"path": {remote}}.Encode(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	if k := auth.Resolve(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable (run `localimage start`):", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "%s\n", out)
		os.Exit(1)
	}
	fmt.Printf("uploaded %s to %s (%d bytes)\n", local, remote, len(data))
}
