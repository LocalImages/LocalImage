// Package session creates and tracks browser sessions as Docker containers.
// State lives in container labels, so the daemon itself is stateless.
package session

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
)

//go:embed assets
var assets embed.FS

const (
	labelManaged = "localimage.managed"
	labelID      = "localimage.id"
	labelToken   = "localimage.token"
	labelBrowser = "localimage.browser"
	labelExpires = "localimage.expires"
	labelView    = "localimage.view_token"
	labelAdmin   = "localimage.admin_pwd"
	labelViewer  = "localimage.viewer_pwd"
	labelProfile = "localimage.profile"

	// chromeProfileDir is where Chromium keeps its user data (see the Dockerfile).
	chromeProfileDir = "/home/neko/.config/chromium"

	webPort = "8080/tcp"
	cdpPort = "9222/tcp"
	pwPort  = "3000/tcp"

	// PlaywrightVersion is baked into the Firefox image. Clients must use a
	// matching playwright-core minor version to connect.
	PlaywrightVersion = "1.63.0"

	// APIVersion changes when the daemon's HTTP API changes incompatibly; SDKs check it.
	APIVersion = 1

	// Browser versions inside the pinned images. A test checks them against the real browsers.
	ChromiumVersion = "154.0.8037.57"
	FirefoxVersion  = "155.0" // the Firefox build that Playwright PlaywrightVersion (1.63.0) ships

	// Image revisions are bumped when the matching assets/ change, forcing a rebuild.
	chromiumRev = "r12"
	firefoxRev  = "r7"
)

// Protocol says how automation clients attach to a session.
const (
	ProtocolCDP        = "cdp"        // chromium.connectOverCDP(url)
	ProtocolPlaywright = "playwright" // firefox.connect(url)
)

type browserSpec struct {
	protocol string
	image    string
}

var browsers = map[string]browserSpec{
	"chromium": {protocol: ProtocolCDP, image: "localimage/chromium:" + chromiumRev},
	"firefox":  {protocol: ProtocolPlaywright, image: "localimage/firefox:pw" + PlaywrightVersion + "-" + firefoxRev},
}

// Record is everything the daemon knows about a session. Backend ports are
// published on host loopback only; the proxy is the public entry point.
type Record struct {
	ID          string
	Token       string
	Browser     string
	Protocol    string
	WebPort     string // host port of Neko's web UI
	BackendPort string // host port of CDP (chromium) or Playwright server (firefox)
	ExpiresAt   time.Time
	ViewToken   string // grants only the read-only live view
	AdminPwd    string // Neko login that can control the browser
	ViewerPwd   string // Neko login that can only watch
	Profile     string
	Extensions  []string
	Tags        map[string]string
	State       string // Docker state: created, running, exited, ...
	Health      string // Docker health: starting, healthy, unhealthy ("" if unknown)
	CreatedAt   time.Time
}

// Session is the API view of a Record, with URLs relative to the daemon's
// public address.
type Session struct {
	ID         string            `json:"id"`
	Browser    string            `json:"browser"`
	Protocol   string            `json:"protocol"`
	ConnectURL string            `json:"connect_url"`
	LiveURL    string            `json:"live_url"` // interactive: can control the browser
	ViewURL    string            `json:"view_url"` // read-only: watch only
	Profile    string            `json:"profile,omitempty"`
	Extensions []string          `json:"extensions,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	// DownloadsDir is where the browser saves downloads; read files with the files API.
	DownloadsDir string `json:"downloads_dir"`
	State        string `json:"state"`
	// Health is Docker's verdict from a check covering the live view and the browser.
	Health    string    `json:"health,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// View renders the record for a client that reached the daemon at host.
func (r *Record) View(host string, secure bool) *Session {
	scheme, wsScheme := "http", "ws"
	if secure {
		scheme, wsScheme = "https", "wss"
	}
	base := "/t/" + r.Token
	connect := scheme + "://" + host + base + "/cdp"
	if r.Protocol == ProtocolPlaywright {
		connect = wsScheme + "://" + host + base + "/pw"
	}
	return &Session{
		ID:           r.ID,
		Browser:      r.Browser,
		Protocol:     r.Protocol,
		ConnectURL:   connect,
		LiveURL:      scheme + "://" + host + base + "/live/?usr=controller&pwd=" + r.AdminPwd + "&embed=1",
		ViewURL:      scheme + "://" + host + "/v/" + r.ViewToken + "/?usr=viewer&pwd=" + r.ViewerPwd + "&cast=1",
		Profile:      r.Profile,
		Extensions:   r.Extensions,
		Tags:         r.Tags,
		DownloadsDir: "/home/neko/Downloads",
		State:        r.State,
		Health:       r.Health,
		CreatedAt:    r.CreatedAt,
		ExpiresAt:    r.ExpiresAt,
	}
}

type CreateOptions struct {
	Browser    string
	Screen     string // e.g. 1280x720@30
	TTL        time.Duration
	Profile    string            // named persistent browser profile (chromium only)
	Kiosk      bool              // chromium: fullscreen without tabs or address bar
	Extensions []string          // chromium: names of uploaded extensions to load
	Memory     int               // MiB, 0 = unlimited
	CPUs       float64           // 0 = unlimited
	Tags       map[string]string // caller metadata, stored as Docker labels
	Wait       time.Duration     // how long to queue for a free slot (0 = fail at once)
	NoBuild    bool              // fail with ErrNotReady instead of building a missing image
	WebGL      bool              // chromium: enable WebGL through software rendering
	Timezone   string            // IANA name, for example Europe/Berlin (default UTC)
	Locale     string            // browser language, for example de-DE (default en-US)
}

var (
	timezoneName = regexp.MustCompile(`^[A-Za-z0-9_+/-]{1,64}$`)
	localeName   = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
)

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

const (
	adminProfile  = `{"is_admin":true,"can_login":true,"can_connect":true,"can_watch":true,"can_host":true,"can_share_media":true,"can_access_clipboard":true,"sends_inactive_cursor":true,"can_see_inactive_cursors":true}`
	viewerProfile = `{"is_admin":false,"can_login":true,"can_connect":true,"can_watch":true,"can_host":false,"can_share_media":false,"can_access_clipboard":false,"sends_inactive_cursor":false,"can_see_inactive_cursors":true}`
)

type Manager struct {
	cli         *client.Client
	publicIP    string        // address clients use to reach WebRTC media
	maxSessions int           // LOCALIMAGE_MAX_SESSIONS, default 10
	maxTTL      time.Duration // LOCALIMAGE_MAX_TTL_SECONDS, default 24h

	mu       sync.Mutex // guards inflight
	inflight int        // creates that hold a slot but have no container yet
	// activeFn counts sessions that occupy a slot. It is a field so tests can
	// exercise the reservation logic without Docker.
	activeFn func(context.Context) (int, error)
	imagesFn func(context.Context) map[string]bool // which browser images are built
	builds   sync.Map                              // image tag -> *sync.Mutex, so one build runs at a time per image
}

func New() (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	ip := os.Getenv("LOCALIMAGE_PUBLIC_IP")
	if ip == "" {
		ip = "127.0.0.1"
	}
	m := &Manager{
		cli:         cli,
		publicIP:    ip,
		maxSessions: envInt("LOCALIMAGE_MAX_SESSIONS", 10),
		maxTTL:      time.Duration(envInt("LOCALIMAGE_MAX_TTL_SECONDS", 24*3600)) * time.Second,
	}
	m.activeFn = m.dockerActiveCount
	m.imagesFn = m.ImagesReady
	return m, nil
}

// dockerActiveCount counts managed containers that are running or about to run.
func (m *Manager) dockerActiveCount(ctx context.Context) (int, error) {
	f := filters.NewArgs(filters.Arg("label", labelManaged+"=true"))
	for _, st := range []string{"created", "running", "restarting"} {
		f.Add("status", st)
	}
	list, err := m.cli.ContainerList(ctx, container.ListOptions{Filters: f})
	return len(list), err
}

// Info describes the daemon's limits and current load.
type Info struct {
	MaxSessions   int `json:"max_sessions"`
	MaxTTLSeconds int `json:"max_ttl_seconds"`
	Running       int `json:"running"`
	Available     int `json:"available"`
	// Images says which browser images are already built; a missing one is built on first use.
	Images map[string]bool `json:"images"`
	// StaleImages counts browser images left by older versions; `localimage prune` removes them.
	StaleImages int `json:"stale_images"`
	// APIVersion is the daemon's API revision; Components says which browser versions the images hold.
	APIVersion int                  `json:"api_version"`
	Components map[string]Component `json:"components"`
}

// Component names a browser the daemon runs and the version inside its image.
type Component struct {
	Version    string `json:"version"`
	Playwright string `json:"playwright,omitempty"` // the Playwright version the server in the image needs
}

func (m *Manager) Info(ctx context.Context) (*Info, error) {
	active, err := m.activeFn(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	active += m.inflight
	m.mu.Unlock()
	avail := m.maxSessions - active
	if avail < 0 {
		avail = 0
	}
	info := &Info{
		MaxSessions:   m.maxSessions,
		MaxTTLSeconds: int(m.maxTTL.Seconds()),
		Running:       active,
		Available:     avail,
		APIVersion:    APIVersion,
		Components: map[string]Component{
			"chromium": {Version: ChromiumVersion},
			"firefox":  {Version: FirefoxVersion, Playwright: PlaywrightVersion},
		},
	}
	if m.imagesFn != nil {
		info.Images = m.imagesFn(ctx)
	}
	if m.cli != nil {
		info.StaleImages = m.StaleImageCount(ctx)
	}
	return info, nil
}

// reserve claims one session slot, waiting up to wait for one to free up (0
// means fail at once). The returned release must be called as soon as the
// container exists (Docker then counts it) or the create failed.
//
// Counting and claiming happen under one lock so a burst of simultaneous
// creates cannot all pass the check and exceed the limit.
func (m *Manager) reserve(ctx context.Context, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		active, err := m.activeFn(ctx)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if active+m.inflight < m.maxSessions {
			m.inflight++
			m.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					m.mu.Lock()
					m.inflight--
					m.mu.Unlock()
				})
			}, nil
		}
		busy := active + m.inflight
		m.mu.Unlock()

		if !time.Now().Before(deadline) {
			return nil, &LimitError{Max: m.maxSessions, Active: busy}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// Ping checks that the Docker daemon is reachable and returns its version.
func (m *Manager) Ping(ctx context.Context) (string, error) {
	v, err := m.cli.ServerVersion(ctx)
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

// ImageReady reports whether the image for browser is already built locally.
func (m *Manager) ImageReady(ctx context.Context, browser string) bool {
	spec, ok := browsers[browser]
	if !ok {
		return false
	}
	_, err := m.cli.ImageInspect(ctx, spec.image)
	return err == nil
}

// ImagesReady reports, for every supported browser, whether its image is built.
func (m *Manager) ImagesReady(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for name := range browsers {
		out[name] = m.ImageReady(ctx, name)
	}
	return out
}

// Prepare builds the images for the given browsers (default: chromium) if they are
// missing, so the first session does not have to. It reports "ready" for images that
// already existed and "built" for ones it built. progress, if set, receives step
// headlines of a build. Cancelling ctx cancels a build in progress.
func (m *Manager) Prepare(ctx context.Context, names []string, progress func(browser, msg string)) (map[string]string, error) {
	if len(names) == 0 {
		names = []string{"chromium"}
	}
	say := func(b, msg string) {
		if progress != nil {
			progress(b, msg)
		}
	}
	for _, n := range names {
		if _, ok := browsers[n]; !ok {
			return nil, invalid("unsupported browser %q", n)
		}
	}
	out := map[string]string{}
	for _, n := range names {
		if m.ImageReady(ctx, n) {
			out[n] = "ready"
			say(n, "already built")
			continue
		}
		say(n, "building the image (first use only; this can take several minutes)")
		b := n
		if err := m.ensureImage(ctx, n, browsers[n], func(line string) { say(b, line) }); err != nil {
			return out, fmt.Errorf("prepare %s image: %w", n, err)
		}
		out[n] = "built"
	}
	return out, nil
}

// Limits returns the configured session and TTL limits.
func (m *Manager) Limits() (maxSessions int, maxTTL time.Duration) { return m.maxSessions, m.maxTTL }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freePort returns a port that is free for both TCP and UDP on loopback.
func freePort() (int, error) {
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		u, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		u.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port found")
}

func (m *Manager) ensureImage(ctx context.Context, name string, spec browserSpec, onLog func(string)) error {
	if _, err := m.cli.ImageInspect(ctx, spec.image); err == nil {
		return nil
	}
	// Many creates can arrive at once on a fresh machine; build the image once
	// and let the others wait for it.
	mu, _ := m.builds.LoadOrStore(spec.image, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if _, err := m.cli.ImageInspect(ctx, spec.image); err == nil {
		return nil
	}
	return m.buildImage(ctx, name, spec.image, onLog)
}

func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Record, error) {
	if opts.Browser == "" {
		opts.Browser = "chromium"
	}
	spec, ok := browsers[opts.Browser]
	if !ok {
		return nil, invalid("unsupported browser %q", opts.Browser)
	}
	if opts.Screen == "" {
		opts.Screen = "1440x900@30"
	}
	if opts.TTL == 0 {
		opts.TTL = 30 * time.Minute
	}
	if opts.TTL > m.maxTTL {
		return nil, fmt.Errorf("%w: ttl_seconds may not exceed %d", ErrLimit, int(m.maxTTL.Seconds()))
	}
	if err := validateTags(opts.Tags); err != nil {
		return nil, err
	}
	if opts.Profile != "" {
		if !profileName.MatchString(opts.Profile) {
			return nil, invalid("invalid profile name %q (use a-z, 0-9, - and _)", opts.Profile)
		}
		if spec.protocol != ProtocolCDP {
			return nil, invalid("profiles are only supported for chromium")
		}
	}
	if opts.Timezone != "" && !timezoneName.MatchString(opts.Timezone) {
		return nil, invalid("invalid timezone %q (use an IANA name such as Europe/Berlin)", opts.Timezone)
	}
	if opts.Locale != "" && !localeName.MatchString(opts.Locale) {
		return nil, invalid("invalid locale %q (use a tag such as de-DE)", opts.Locale)
	}
	if (opts.WebGL || opts.Locale != "") && spec.protocol != ProtocolCDP {
		return nil, invalid("webgl and locale are only supported for chromium (for firefox, set them on the Playwright context)")
	}
	if (opts.Kiosk || len(opts.Extensions) > 0) && spec.protocol != ProtocolCDP {
		return nil, invalid("kiosk mode and extensions are only supported for chromium")
	}

	if opts.NoBuild && !m.ImageReady(ctx, opts.Browser) {
		return nil, notReady(opts.Browser)
	}

	// Claim a slot (queueing if asked) before any side effect, so a refused
	// create leaves nothing behind.
	release, err := m.reserve(ctx, opts.Wait)
	if err != nil {
		return nil, err
	}
	defer release()

	if opts.Profile != "" {
		if err := m.claimProfile(ctx, opts.Profile); err != nil {
			return nil, err
		}
	}
	if err := m.ensureImage(ctx, opts.Browser, spec, nil); err != nil {
		return nil, fmt.Errorf("prepare %s image: %w", opts.Browser, err)
	}

	backend := nat.Port(cdpPort)
	if spec.protocol == ProtocolPlaywright {
		backend = nat.Port(pwPort)
	}

	id := randHex(4)
	name := "localimage-" + id
	// If the client gives up (or anything else fails) at any point from here on, remove
	// the container by its name. Docker may have created it even though the call that
	// asked for it was cancelled, in which case we never learned its ID.
	succeeded := false
	startedAt := time.Now()
	defer func() {
		if !succeeded {
			slog.Warn("session create failed; removing its container", "id", id, "browser", opts.Browser)
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = m.cli.ContainerRemove(rctx, name, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	}()
	// The docker calls that create and start the container are not tied to the request:
	// cancelling them half way is what leaves orphans behind. Cancellation is honoured
	// while waiting for readiness, and the defer above removes the container.
	bg := context.WithoutCancel(ctx)
	token := randHex(16)
	viewToken := randHex(16)
	adminPwd, viewerPwd := randHex(12), randHex(12)
	mux, err := freePort()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(opts.TTL)

	// Neko requires WebRTC ports to be mapped with identical numbers on host
	// and container, and media cannot go through an HTTP proxy.
	muxTCP := nat.Port(fmt.Sprintf("%d/tcp", mux))
	muxUDP := nat.Port(fmt.Sprintf("%d/udp", mux))
	loop := func(p string) []nat.PortBinding { return []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: p}} }
	mediaBind := func(p string) []nat.PortBinding {
		ip := "127.0.0.1"
		if m.publicIP != "127.0.0.1" {
			ip = "0.0.0.0"
		}
		return []nat.PortBinding{{HostIP: ip, HostPort: p}}
	}

	cfg := &container.Config{
		Image: spec.image,
		Env: []string{
			"NEKO_DESKTOP_SCREEN=" + opts.Screen,
			// Two logins: the admin password can drive the browser, the viewer
			// password only watches. Passwords are random per session.
			"NEKO_MEMBER_PROVIDER=multiuser",
			"NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD=" + adminPwd,
			"NEKO_MEMBER_MULTIUSER_ADMIN_PROFILE=" + adminProfile,
			"NEKO_MEMBER_MULTIUSER_USER_PASSWORD=" + viewerPwd,
			"NEKO_MEMBER_MULTIUSER_USER_PROFILE=" + viewerProfile,
			"NEKO_SERVER_PATH_PREFIX=/t/" + token + "/live",
			"NEKO_SERVER_PROXY=true",
			"NEKO_SESSION_IMPLICIT_HOSTING=true",
			fmt.Sprintf("NEKO_WEBRTC_UDPMUX=%d", mux),
			fmt.Sprintf("NEKO_WEBRTC_TCPMUX=%d", mux),
			"NEKO_WEBRTC_NAT1TO1=" + m.publicIP,
		},
		ExposedPorts: nat.PortSet{webPort: {}, backend: {}, muxTCP: {}, muxUDP: {}},
		Labels: map[string]string{
			labelManaged: "true",
			labelID:      id,
			labelToken:   token,
			labelBrowser: opts.Browser,
			labelExpires: expires.Format(time.RFC3339),
			labelView:    viewToken,
			labelAdmin:   adminPwd,
			labelViewer:  viewerPwd,
		},
	}
	if opts.Profile != "" {
		cfg.Labels[labelProfile] = opts.Profile
	}
	for k, v := range opts.Tags {
		cfg.Labels[labelTagPrefix+k] = v
	}
	if opts.Timezone != "" {
		cfg.Env = append(cfg.Env, "TZ="+opts.Timezone)
	}
	if opts.WebGL {
		cfg.Env = append(cfg.Env, "LOCALIMAGE_WEBGL=1")
	}
	if opts.Locale != "" {
		cfg.Env = append(cfg.Env, "LOCALIMAGE_LANG="+opts.Locale)
	}
	var chromiumArgs []string
	if opts.Kiosk {
		chromiumArgs = append(chromiumArgs, "--kiosk")
	}
	extMounts, loadFlag, err := m.extensionMounts(ctx, opts.Extensions)
	if err != nil {
		return nil, err
	}
	if loadFlag != "" {
		chromiumArgs = append(chromiumArgs, loadFlag)
		cfg.Labels[labelExts] = strings.Join(opts.Extensions, ",")
	}
	if len(chromiumArgs) > 0 {
		cfg.Env = append(cfg.Env, "LOCALIMAGE_CHROMIUM_ARGS="+strings.Join(chromiumArgs, " "))
	}
	host := &container.HostConfig{
		// Lets the browser reach services on the host that listen on a non-loopback address
		// (0.0.0.0) as host.docker.internal; Docker Desktop provides the name by itself.
		ExtraHosts:    []string{"host.docker.internal:host-gateway"},
		ShmSize:       2 << 30,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		PortBindings: nat.PortMap{
			webPort: loop(""),
			backend: loop(""),
			muxTCP:  mediaBind(strconv.Itoa(mux)),
			muxUDP:  mediaBind(strconv.Itoa(mux)),
		},
		Resources: container.Resources{
			Memory:   int64(opts.Memory) << 20,
			NanoCPUs: int64(opts.CPUs * 1e9),
		},
	}
	host.Mounts = append(host.Mounts, extMounts...)
	if opts.Profile != "" {
		host.Mounts = append(host.Mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: profileVolume(opts.Profile),
			Target: chromeProfileDir,
		})
	}

	created, err := m.cli.ContainerCreate(bg, cfg, host, nil, nil, name)
	if err != nil {
		return nil, err
	}
	release() // the container now exists, so Docker counts it against the limit
	cleanup := func() {
		_ = m.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	}

	if err := m.cli.ContainerStart(bg, created.ID, container.StartOptions{}); err != nil {
		cleanup()
		return nil, err
	}

	rec, err := m.byID(ctx, id)
	if err != nil {
		cleanup()
		return nil, err
	}

	readyTimeout := 90 * time.Second
	if until := time.Until(expires); until < readyTimeout {
		readyTimeout = until
	}
	if readyTimeout <= 0 {
		cleanup()
		return nil, fmt.Errorf("session expired before it became ready")
	}
	if err := waitReady(ctx, rec, readyTimeout); err != nil {
		cleanup()
		return nil, err
	}
	if spec.protocol == ProtocolCDP {
		m.fitWindow(ctx, created.ID)
	}
	succeeded = true
	slog.Info("session created", "id", id, "browser", opts.Browser, "ms", time.Since(startedAt).Milliseconds())
	return rec, nil
}

// waitReady blocks until the session is usable: the browser's automation
// endpoint answers and Neko's web server (the live view) is serving.
func waitReady(ctx context.Context, rec *Record, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	// Docker's port publisher accepts TCP before the service inside is up, so
	// a dial proves nothing: require an actual HTTP response. The Playwright
	// server answers a plain GET with a non-200 status, which still means it
	// is listening.
	addr := "127.0.0.1:" + rec.BackendPort
	path, anyStatus := "/json/version", false
	if rec.Protocol == ProtocolPlaywright {
		path, anyStatus = "/pw", true
	}
	if !pollHTTP(ctx, "http://"+addr+path, deadline, anyStatus) {
		return fmt.Errorf("browser did not become ready within %s", timeout)
	}
	// Neko starts alongside the browser and can lag behind it, particularly on
	// a slow machine; a live view URL handed out too early would fail.
	web := "http://127.0.0.1:" + rec.WebPort + "/t/" + rec.Token + "/live/health"
	if !pollHTTP(ctx, web, deadline, false) {
		return fmt.Errorf("live view did not become ready within %s", timeout)
	}
	if len(rec.Extensions) > 0 {
		waitExtensionTarget(ctx, addr)
	}
	return nil
}

// pollHTTP GETs url every 250ms until it answers (any status if anyStatus,
// otherwise 200), the deadline passes, or ctx ends.
func pollHTTP(ctx context.Context, url string, deadline time.Time, anyStatus bool) bool {
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if anyStatus || resp.StatusCode == http.StatusOK {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false
}

func (m *Manager) findContainer(ctx context.Context, label string) (string, error) {
	f := filters.NewArgs(filters.Arg("label", label))
	list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", ErrNotFound
	}
	return list[0].ID, nil
}

func (m *Manager) byLabel(ctx context.Context, label string) (*Record, error) {
	cid, err := m.findContainer(ctx, label)
	if err != nil {
		return nil, err
	}
	info, err := m.cli.ContainerInspect(ctx, cid)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return toRecord(info), nil
}

func (m *Manager) byID(ctx context.Context, id string) (*Record, error) {
	return m.byLabel(ctx, labelID+"="+id)
}

// Get returns a session by id.
func (m *Manager) Get(ctx context.Context, id string) (*Record, error) {
	rec, err := m.byID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cid, err := m.findContainer(ctx, labelID+"="+id); err == nil {
		m.withCurrentTags(ctx, cid, rec)
	}
	return rec, nil
}

// ByViewToken resolves the secret in a read-only /v/ URL to its session.
func (m *Manager) ByViewToken(ctx context.Context, token string) (*Record, error) {
	return m.byLabel(ctx, labelView+"="+token)
}

// ByToken resolves the secret in a proxy URL to its session.
func (m *Manager) ByToken(ctx context.Context, token string) (*Record, error) {
	return m.byLabel(ctx, labelToken+"="+token)
}

func toRecord(info container.InspectResponse) *Record {
	hostPort := func(p nat.Port) string {
		if b := info.NetworkSettings.Ports[p]; len(b) > 0 {
			return b[0].HostPort
		}
		return ""
	}
	l := info.Config.Labels
	exp, _ := time.Parse(time.RFC3339, l[labelExpires])
	created, _ := time.Parse(time.RFC3339Nano, info.Created)
	spec := browsers[l[labelBrowser]]
	backend := nat.Port(cdpPort)
	if spec.protocol == ProtocolPlaywright {
		backend = nat.Port(pwPort)
	}
	return &Record{
		ID:          l[labelID],
		Token:       l[labelToken],
		Browser:     l[labelBrowser],
		Protocol:    spec.protocol,
		WebPort:     hostPort(webPort),
		BackendPort: hostPort(backend),
		ExpiresAt:   exp,
		ViewToken:   l[labelView],
		AdminPwd:    l[labelAdmin],
		ViewerPwd:   l[labelViewer],
		Profile:     l[labelProfile],
		Extensions:  splitNonEmpty(l[labelExts]),
		Tags:        tagsFromLabels(l),
		State:       info.State.Status,
		Health:      healthOf(info),
		CreatedAt:   created,
	}
}

// List returns managed sessions in every state (a crashed worker can leave
// stopped containers behind), optionally only those carrying all of tags.
func (m *Manager) List(ctx context.Context, tags map[string]string) ([]*Record, error) {
	f := filters.NewArgs(filters.Arg("label", labelManaged+"=true"))
	list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(list))
	for _, c := range list {
		info, err := m.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue // removed since the list was taken
		}
		rec := toRecord(info)
		if len(tags) > 0 {
			// Tags can have been changed since creation, so filter on the current ones.
			m.withCurrentTags(ctx, c.ID, rec)
			if !hasAllTags(rec.Tags, tags) {
				continue
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

func hasAllTags(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Delete removes a session. It returns ErrNotFound if there is no such
// session (already deleted, expired, or never existed), which callers that
// release a lease should treat as success.
func (m *Manager) Delete(ctx context.Context, id string) error {
	cid, err := m.findContainer(ctx, labelID+"="+id)
	if err != nil {
		return err
	}
	info, err := m.cli.ContainerInspect(ctx, cid)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	if info.Config.Labels[labelProfile] != "" {
		// Let Chromium shut down cleanly so cookies and storage reach disk.
		timeout := 20
		_ = m.cli.ContainerStop(ctx, cid, container.StopOptions{Timeout: &timeout})
	}
	err = m.cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true, RemoveVolumes: true})
	switch {
	case err == nil:
		slog.Info("session deleted", "id", id)
		return nil
	case errdefs.IsNotFound(err):
		return ErrNotFound // a concurrent delete won the race
	case strings.Contains(err.Error(), "already in progress"):
		return ErrNotFound // another delete is removing it right now: only that one reports success
	}
	return err
}

func profileVolume(name string) string { return "localimage-profile-" + name }

// claimProfile ensures the profile volume exists and no live session uses
// it: two Chromium processes cannot share one user-data directory.
func (m *Manager) claimProfile(ctx context.Context, name string) error {
	f := filters.NewArgs(filters.Arg("label", labelProfile+"="+name))
	if list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f}); err != nil {
		return err
	} else if len(list) > 0 {
		return conflict("profile %q is already in use by another session", name)
	}
	_, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   profileVolume(name),
		Labels: map[string]string{labelProfile: name},
	})
	return err
}

// Profiles lists saved profile names.
func (m *Manager) Profiles(ctx context.Context) ([]string, error) {
	res, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(filters.Arg("label", labelProfile))})
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, v := range res.Volumes {
		out = append(out, v.Labels[labelProfile])
	}
	return out, nil
}

// DeleteProfile removes a saved profile. It fails while a session uses it.
func (m *Manager) DeleteProfile(ctx context.Context, name string) error {
	if !profileName.MatchString(name) {
		return invalid("invalid profile name")
	}
	return m.cli.VolumeRemove(ctx, profileVolume(name), false)
}

// reapParallel is how many expired sessions are removed at once. Stopping a session with a
// saved profile can take 20 seconds, which must not hold up the others.
const reapParallel = 8

// Reap removes sessions whose TTL has passed.
func (m *Manager) Reap(ctx context.Context) {
	list, err := m.List(ctx, nil)
	if err != nil {
		return
	}
	now := time.Now()
	var expired []string
	for _, r := range list {
		if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
			expired = append(expired, r.ID)
		}
	}
	runLimited(reapParallel, expired, func(id string) {
		slog.Info("session expired", "id", id)
		_ = m.Delete(ctx, id)
	})
}

// runLimited calls fn for every item, at most limit at a time, and returns when all are done.
func runLimited[T any](limit int, items []T, fn func(T)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for _, it := range items {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			fn(it)
		}()
	}
	wg.Wait()
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// waitExtensionTarget gives Chromium a moment to register its --load-extension
// extensions, so a client that navigates right after create still gets their
// content scripts. It is best effort: an extension with no background page has
// no target to wait for, so it gives up quietly after a few seconds.
func waitExtensionTarget(ctx context.Context, addr string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json/list", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if strings.Contains(string(body), "chrome-extension://") {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func healthOf(info container.InspectResponse) string {
	if info.State != nil && info.State.Health != nil {
		return info.State.Health.Status
	}
	return ""
}
