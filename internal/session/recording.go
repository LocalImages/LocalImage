package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

const recDir = "/tmp/recs"

var recID = regexp.MustCompile(`^[0-9a-f]{8}$`)

// exec runs cmd in the container as user, keeping stdout and stderr apart.
func (m *Manager) exec(ctx context.Context, cid, user string, cmd ...string) (stdout, stderr []byte, code int, err error) {
	ex, err := m.cli.ContainerExecCreate(ctx, cid, container.ExecOptions{
		User: user, Cmd: cmd, AttachStdout: true, AttachStderr: true,
		Env: []string{"DISPLAY=:99.0"},
	})
	if err != nil {
		return nil, nil, 0, err
	}
	att, err := m.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, 0, err
	}
	defer att.Close()
	var so, se bytes.Buffer
	if _, err := stdcopy.StdCopy(&so, &se, att.Reader); err != nil {
		return nil, nil, 0, err
	}
	insp, err := m.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return so.Bytes(), se.Bytes(), 0, err
	}
	return so.Bytes(), se.Bytes(), insp.ExitCode, nil
}

// run executes cmd in the container as user and returns its combined output.
func (m *Manager) run(ctx context.Context, cid, user string, cmd ...string) (string, int, error) {
	so, se, code, err := m.exec(ctx, cid, user, cmd...)
	return string(so) + string(se), code, err
}

// Region selects part of the display for a screenshot.
type Region struct{ X, Y, Width, Height int }

// Screenshot returns a PNG of the session's display, or of region if given.
func (m *Manager) Screenshot(ctx context.Context, id string, region *Region) ([]byte, error) {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	args := []string{"ffmpeg", "-loglevel", "error", "-f", "x11grab", "-i", ":99.0", "-frames:v", "1"}
	if region != nil {
		if region.Width <= 0 || region.Height <= 0 || region.X < 0 || region.Y < 0 {
			return nil, fmt.Errorf("invalid region")
		}
		args = append(args, "-vf", fmt.Sprintf("crop=%d:%d:%d:%d", region.Width, region.Height, region.X, region.Y))
	}
	args = append(args, "-f", "image2pipe", "-c:v", "png", "-")
	png, se, code, err := m.exec(ctx, cid, "neko", args...)
	if err != nil {
		return nil, err
	}
	if code != 0 || len(png) == 0 {
		return nil, fmt.Errorf("screenshot failed: %s", bytes.TrimSpace(se))
	}
	return png, nil
}

func (m *Manager) containerOf(ctx context.Context, id string) (string, error) {
	return m.findContainer(ctx, labelID+"="+id)
}

// DownloadRecording streams a recording (the latest if rid is empty) to w.
func (m *Manager) DownloadRecording(ctx context.Context, id, rid string, w io.Writer) (int64, error) {
	if rid == "" {
		list, err := m.Recordings(ctx, id)
		if err != nil || len(list) == 0 {
			return 0, fmt.Errorf("no recording available")
		}
		rid = list[len(list)-1].ID
	}
	if !recID.MatchString(rid) {
		return 0, fmt.Errorf("invalid recording id")
	}
	return m.DownloadFile(ctx, id, recDir+"/"+rid+".mp4", w)
}
