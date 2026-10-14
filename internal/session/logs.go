package session

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	defaultLogTail = 200
	maxLogTail     = 5000
	maxLogBytes    = 1 << 20 // only the last MiB of a log file is read
)

// logFiles are the logs the images write under /var/log/neko, by source name. "container" is
// Docker's own capture of the container's output (supervisord's, mostly).
var logFiles = map[string]string{
	"chromium":    "/var/log/neko/chromium.log",
	"firefox":     "/var/log/neko/pw.log", // the Playwright server and Firefox
	"neko":        "/var/log/neko/neko.log",
	"xorg":        "/var/log/neko/xorg.log",
	"openbox":     "/var/log/neko/openbox.log",
	"pulseaudio":  "/var/log/neko/pulseaudio.log",
	"cdp-forward": "/var/log/neko/cdp-forward.log",
}

// LogSources lists the valid source names.
func LogSources() []string {
	out := []string{"container"}
	for k := range logFiles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Logs returns the last tail lines of a session's log. It works for stopped containers too,
// which is what makes it useful for finding out why a session died.
func (m *Manager) Logs(ctx context.Context, id, source string, tail int) (string, error) {
	if source == "" {
		source = "container"
	}
	if tail == 0 {
		tail = defaultLogTail
	}
	if tail < 1 || tail > maxLogTail {
		return "", invalid("tail must be between 1 and %d", maxLogTail)
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return "", err
	}

	if source == "container" {
		rc, err := m.cli.ContainerLogs(ctx, cid, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: fmt.Sprint(tail)})
		if err != nil {
			return "", err
		}
		defer rc.Close()
		var out bytes.Buffer
		if _, err := stdcopy.StdCopy(&out, &out, rc); err != nil {
			return "", err
		}
		return out.String(), nil
	}

	path, ok := logFiles[source]
	if !ok {
		return "", invalid("unknown log source %q (use one of: %s)", source, strings.Join(LogSources(), ", "))
	}
	rc, _, err := m.cli.CopyFromContainer(ctx, cid, path)
	if err != nil {
		return "", nil // that browser's log does not exist in this image, or nothing was written yet
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		return "", nil
	}
	// Keep only the end of a possibly huge file.
	var ring []byte
	buf := make([]byte, 64<<10)
	for {
		n, rerr := tr.Read(buf)
		ring = append(ring, buf[:n]...)
		if len(ring) > 2*maxLogBytes {
			ring = ring[len(ring)-maxLogBytes:]
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return lastLines(string(ring), tail), nil
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
