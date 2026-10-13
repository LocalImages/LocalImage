package session

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	browserUser    = "neko"
	browserHome    = "/home/neko"
	maxOutput      = 1 << 20   // per stream, for exec
	maxUpload      = 100 << 20 // bytes
	defaultTimeout = 30
	maxTimeout     = 300
)

// capBuffer keeps at most max bytes and notes when it dropped some.
type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

// Exec runs argv inside the session container as the browser user. It is not
// run through a shell; pass ["sh","-c","..."] explicitly if you want one.
func (m *Manager) Exec(ctx context.Context, id string, argv []string, cwd string, timeoutSec int) (*ExecResult, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("command must not be empty")
	}
	if timeoutSec == 0 {
		timeoutSec = defaultTimeout
	}
	if timeoutSec < 1 || timeoutSec > maxTimeout {
		return nil, fmt.Errorf("timeout_seconds must be 1-%d", maxTimeout)
	}
	if cwd == "" {
		cwd = browserHome
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	// `timeout` inside the container enforces the limit, so a runaway
	// process is killed there and not left behind.
	wrapped := append([]string{"timeout", "-s", "KILL", strconv.Itoa(timeoutSec)}, argv...)
	ex, err := m.cli.ContainerExecCreate(ctx, cid, container.ExecOptions{
		User: browserUser, WorkingDir: cwd, Cmd: wrapped,
		Env:          []string{"DISPLAY=:99.0", "HOME=" + browserHome},
		AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec+10)*time.Second)
	defer cancel()
	att, err := m.cli.ContainerExecAttach(rctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer att.Close()
	so, se := &capBuffer{max: maxOutput}, &capBuffer{max: maxOutput}
	if _, err := stdcopy.StdCopy(so, se, att.Reader); err != nil {
		return nil, err
	}
	insp, err := m.cli.ContainerExecInspect(rctx, ex.ID)
	if err != nil {
		return nil, err
	}
	res := &ExecResult{
		ExitCode: insp.ExitCode, Stdout: so.buf.String(), Stderr: se.buf.String(),
		Truncated: so.truncated || se.truncated,
	}
	if insp.ExitCode == 137 { // timeout -s KILL
		res.TimedOut = true
	}
	return res, nil
}

func cleanPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) || !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path must be absolute")
	}
	return path.Clean(p), nil
}

// UploadFile writes r (at most maxUpload bytes) to p, creating parent
// directories. The file is owned by the browser user so the browser can read it.
func (m *Manager) UploadFile(ctx context.Context, id, p string, r io.Reader) (int64, error) {
	p, err := cleanPath(p)
	if err != nil {
		return 0, err
	}
	if p == "/" {
		return 0, fmt.Errorf("path must name a file")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxUpload+1))
	if err != nil {
		return 0, err
	}
	if len(data) > maxUpload {
		return 0, fmt.Errorf("file exceeds %d MiB", maxUpload>>20)
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return 0, err
	}
	dir := path.Dir(p)
	if _, se, code, err := m.exec(ctx, cid, browserUser, "mkdir", "-p", dir); err != nil {
		return 0, err
	} else if code != 0 {
		return 0, fmt.Errorf("cannot create %s: %s", dir, bytes.TrimSpace(se))
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: path.Base(p), Mode: 0o644, Size: int64(len(data)), Uid: 1000, Gid: 1000}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	if _, err := tw.Write(data); err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := m.cli.CopyToContainer(ctx, cid, dir, &buf, container.CopyToContainerOptions{}); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// DownloadFile streams the file at p to w.
func (m *Manager) DownloadFile(ctx context.Context, id, p string, w io.Writer) (int64, error) {
	p, err := cleanPath(p)
	if err != nil {
		return 0, err
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return 0, err
	}
	rc, _, err := m.cli.CopyFromContainer(ctx, cid, p)
	if err != nil {
		return 0, fmt.Errorf("no such file: %s", p)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		return 0, err
	}
	if hdr.Typeflag != tar.TypeReg {
		return 0, fmt.Errorf("%s is not a regular file", p)
	}
	return io.Copy(w, tr)
}

type FileInfo struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // file, dir, link, other
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix seconds
}

// ListFiles lists the entries of directory p.
func (m *Manager) ListFiles(ctx context.Context, id, p string) ([]FileInfo, error) {
	p, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	so, se, code, err := m.exec(ctx, cid, browserUser,
		"find", p, "-mindepth", "1", "-maxdepth", "1", "-printf", `%y\t%s\t%T@\t%f\n`)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("cannot list %s: %s", p, bytes.TrimSpace(se))
	}
	out := []FileInfo{}
	for _, line := range strings.Split(strings.TrimRight(string(so), "\n"), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		mt, _ := strconv.ParseFloat(f[2], 64)
		typ := map[string]string{"f": "file", "d": "dir", "l": "link"}[f[0]]
		if typ == "" {
			typ = "other"
		}
		out = append(out, FileInfo{Name: f[3], Type: typ, Size: size, MTime: int64(mt)})
	}
	return out, nil
}
