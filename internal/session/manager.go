// Package session creates and tracks browser sessions as Docker containers.
// State lives in container labels, so the daemon itself is stateless.
package session

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
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

	// Image revisions are bumped when the matching assets/ change, forcing a rebuild.
	chromiumRev = "r6"
	firefoxRev  = "r3"
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
}

// Session is the API view of a Record, with URLs relative to the daemon's
// public address.
type Session struct {
	ID         string    `json:"id"`
	Browser    string    `json:"browser"`
	Protocol   string    `json:"protocol"`
	ConnectURL string    `json:"connect_url"`
	LiveURL    string    `json:"live_url"` // interactive: can control the browser
	ViewURL    string    `json:"view_url"` // read-only: watch only
	Profile    string    `json:"profile,omitempty"`
	Extensions []string  `json:"extensions,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// View renders the record for a client that reached the daemon at host.
func (r *Record) View(host string) *Session {
	base := "/t/" + r.Token
	connect := "http://" + host + base + "/cdp"
	if r.Protocol == ProtocolPlaywright {
		connect = "ws://" + host + base + "/pw"
	}
	return &Session{
		ID:         r.ID,
		Browser:    r.Browser,
		Protocol:   r.Protocol,
		ConnectURL: connect,
		LiveURL:    "http://" + host + base + "/live/?usr=controller&pwd=" + r.AdminPwd + "&embed=1",
		ViewURL:    "http://" + host + "/v/" + r.ViewToken + "/?usr=viewer&pwd=" + r.ViewerPwd + "&cast=1",
		Profile:    r.Profile,
		Extensions: r.Extensions,
		ExpiresAt:  r.ExpiresAt,
	}
}

type CreateOptions struct {
	Browser    string
	Screen     string // e.g. 1280x720@30
	TTL        time.Duration
	Profile    string   // named persistent browser profile (chromium only)
	Kiosk      bool     // chromium: fullscreen without tabs or address bar
	Extensions []string // chromium: names of uploaded extensions to load
	Memory     int      // MiB, 0 = unlimited
	CPUs       float64  // 0 = unlimited
}

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

const (
	adminProfile  = `{"is_admin":true,"can_login":true,"can_connect":true,"can_watch":true,"can_host":true,"can_share_media":true,"can_access_clipboard":true,"sends_inactive_cursor":true,"can_see_inactive_cursors":true}`
	viewerProfile = `{"is_admin":false,"can_login":true,"can_connect":true,"can_watch":true,"can_host":false,"can_share_media":false,"can_access_clipboard":false,"sends_inactive_cursor":false,"can_see_inactive_cursors":true}`
)

// ErrLimit is returned when a request exceeds a configured limit.
var ErrLimit = errors.New("limit exceeded")

type Manager struct {
	cli         *client.Client
	publicIP    string        // address clients use to reach WebRTC media
	maxSessions int           // LOCALIMAGE_MAX_SESSIONS, default 10
	maxTTL      time.Duration // LOCALIMAGE_MAX_TTL_SECONDS, default 24h
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
	return &Manager{
		cli:         cli,
		publicIP:    ip,
		maxSessions: envInt("LOCALIMAGE_MAX_SESSIONS", 10),
		maxTTL:      time.Duration(envInt("LOCALIMAGE_MAX_TTL_SECONDS", 24*3600)) * time.Second,
	}, nil
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

func (m *Manager) ensureImage(ctx context.Context, name string, spec browserSpec) error {
	if _, err := m.cli.ImageInspect(ctx, spec.image); err == nil {
		return nil
	}
	return m.buildImage(ctx, name, spec.image)
}

func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Record, error) {
	if opts.Browser == "" {
		opts.Browser = "chromium"
	}
	spec, ok := browsers[opts.Browser]
	if !ok {
		return nil, fmt.Errorf("unsupported browser %q", opts.Browser)
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
	if running, err := m.List(ctx); err != nil {
		return nil, err
	} else if len(running) >= m.maxSessions {
		return nil, fmt.Errorf("%w: %d sessions are already running (LOCALIMAGE_MAX_SESSIONS=%d)", ErrLimit, len(running), m.maxSessions)
	}
	if opts.Profile != "" {
		if !profileName.MatchString(opts.Profile) {
			return nil, fmt.Errorf("invalid profile name %q (use a-z, 0-9, - and _)", opts.Profile)
		}
		if spec.protocol != ProtocolCDP {
			return nil, fmt.Errorf("profiles are only supported for chromium")
		}
		if err := m.claimProfile(ctx, opts.Profile); err != nil {
			return nil, err
		}
	}
	if err := m.ensureImage(ctx, opts.Browser, spec); err != nil {
		return nil, fmt.Errorf("prepare %s image: %w", opts.Browser, err)
	}

	backend := nat.Port(cdpPort)
	if spec.protocol == ProtocolPlaywright {
		backend = nat.Port(pwPort)
	}

	id := randHex(4)
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
	if (opts.Kiosk || len(opts.Extensions) > 0) && spec.protocol != ProtocolCDP {
		return nil, fmt.Errorf("kiosk mode and extensions are only supported for chromium")
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

	created, err := m.cli.ContainerCreate(ctx, cfg, host, nil, nil, "localimage-"+id)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = m.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	}

	if err := m.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cleanup()
		return nil, err
	}

	rec, err := m.byID(ctx, id)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := waitReady(ctx, rec, 90*time.Second); err != nil {
		cleanup()
		return nil, err
	}
	return rec, nil
}

func waitReady(ctx context.Context, rec *Record, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + rec.BackendPort
	path := "/json/version"
	if rec.Protocol == ProtocolPlaywright {
		path = "/pw"
	}
	for time.Now().Before(deadline) {
		// Docker's port publisher accepts TCP before the service inside is up,
		// so a dial proves nothing: require an actual HTTP response. The
		// Playwright server answers a plain GET with a non-200 status, which
		// still means it is listening.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if rec.Protocol == ProtocolPlaywright || resp.StatusCode == http.StatusOK {
				if len(rec.Extensions) > 0 {
					waitExtensionTarget(ctx, addr)
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("browser did not become ready within %s", timeout)
}

func (m *Manager) findContainer(ctx context.Context, label string) (string, error) {
	f := filters.NewArgs(filters.Arg("label", label))
	list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("session not found")
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
		return nil, err
	}
	return toRecord(info), nil
}

func (m *Manager) byID(ctx context.Context, id string) (*Record, error) {
	return m.byLabel(ctx, labelID+"="+id)
}

// Get returns a session by id.
func (m *Manager) Get(ctx context.Context, id string) (*Record, error) { return m.byID(ctx, id) }

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
	}
}

func (m *Manager) List(ctx context.Context) ([]*Record, error) {
	f := filters.NewArgs(filters.Arg("label", labelManaged+"=true"))
	list, err := m.cli.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(list))
	for _, c := range list {
		info, err := m.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		out = append(out, toRecord(info))
	}
	return out, nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	cid, err := m.findContainer(ctx, labelID+"="+id)
	if err != nil {
		return err
	}
	if rec, err := m.byID(ctx, id); err == nil && rec.Profile != "" {
		// Let Chromium shut down cleanly so cookies and storage reach disk.
		timeout := 20
		_ = m.cli.ContainerStop(ctx, cid, container.StopOptions{Timeout: &timeout})
	}
	return m.cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true, RemoveVolumes: true})
}

func profileVolume(name string) string { return "localimage-profile-" + name }

// claimProfile ensures the profile volume exists and no live session uses
// it: two Chromium processes cannot share one user-data directory.
func (m *Manager) claimProfile(ctx context.Context, name string) error {
	f := filters.NewArgs(filters.Arg("label", labelProfile+"="+name))
	if list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f}); err != nil {
		return err
	} else if len(list) > 0 {
		return fmt.Errorf("profile %q is already in use by another session", name)
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
		return fmt.Errorf("invalid profile name")
	}
	return m.cli.VolumeRemove(ctx, profileVolume(name), false)
}

// Reap removes sessions whose TTL has passed.
func (m *Manager) Reap(ctx context.Context) {
	list, err := m.List(ctx)
	if err != nil {
		return
	}
	for _, r := range list {
		if !r.ExpiresAt.IsZero() && time.Now().After(r.ExpiresAt) {
			_ = m.Delete(ctx, r.ID)
		}
	}
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
