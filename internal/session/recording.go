package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

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

// StartRecording begins capturing the session's display to an MP4 and returns
// the recording id. Files are fragmented so they stay playable even if the
// container is killed. Only one recording can run at a time per session.
func (m *Manager) StartRecording(ctx context.Context, id string) (string, error) {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return "", err
	}
	rid := randHex(4)
	script := fmt.Sprintf(`
mkdir -p %[1]s
for f in %[1]s/*.pid; do
  [ -f "$f" ] && kill -0 "$(cat "$f")" 2>/dev/null && { echo "a recording is already running"; exit 3; }
done
nohup ffmpeg -y -loglevel error -f x11grab -framerate 15 -i :99.0 \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
  -movflags +frag_keyframe+empty_moov+default_base_moof %[1]s/%[2]s.mp4 </dev/null >%[1]s/%[2]s.log 2>&1 &
echo $! > %[1]s/%[2]s.pid
sleep 1
kill -0 "$(cat %[1]s/%[2]s.pid)" 2>/dev/null || { cat %[1]s/%[2]s.log; exit 4; }
`, recDir, rid)
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c", script)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("start recording: %s", bytes.TrimSpace([]byte(out)))
	}
	return rid, nil
}

// StopRecording finalizes a recording (the running one if rid is empty) and
// returns its id once ffmpeg has exited.
func (m *Manager) StopRecording(ctx context.Context, id, rid string) (string, error) {
	if rid != "" && !recID.MatchString(rid) {
		return "", fmt.Errorf("invalid recording id")
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return "", err
	}
	script := fmt.Sprintf(`
rid="$1"
if [ -z "$rid" ]; then
  for f in %[1]s/*.pid; do
    [ -f "$f" ] && kill -0 "$(cat "$f")" 2>/dev/null && rid=$(basename "$f" .pid)
  done
fi
[ -n "$rid" ] && [ -f %[1]s/$rid.pid ] || { echo "no running recording"; exit 3; }
pid=$(cat %[1]s/$rid.pid)
kill -INT "$pid" 2>/dev/null
for i in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || { echo "$rid"; exit 0; }; sleep 0.1; done
kill -KILL "$pid"; echo "$rid"
`, recDir)
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c", script, "sh", rid)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("stop recording: %s", bytes.TrimSpace([]byte(out)))
	}
	return string(bytes.TrimSpace([]byte(out))), nil
}

type RecordingInfo struct {
	ID        string `json:"replay_id"`
	Running   bool   `json:"running"`
	Size      int64  `json:"size"`
	StartedAt int64  `json:"started_at"` // unix seconds
}

// Recordings lists a session's recordings, oldest first.
func (m *Manager) Recordings(ctx context.Context, id string) ([]RecordingInfo, error) {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`
for f in %[1]s/*.mp4; do
  [ -f "$f" ] || continue
  rid=$(basename "$f" .mp4); run=0
  [ -f %[1]s/$rid.pid ] && kill -0 "$(cat %[1]s/$rid.pid)" 2>/dev/null && run=1
  printf '%%s\t%%s\t%%s\t%%s\n' "$rid" "$run" "$(stat -c %%s "$f")" "$(stat -c %%W "$f")"
done`, recDir)
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c", script)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("list recordings: %s", bytes.TrimSpace([]byte(out)))
	}
	list := []RecordingInfo{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		born, _ := strconv.ParseInt(f[3], 10, 64)
		list = append(list, RecordingInfo{ID: f[0], Running: f[1] == "1", Size: size, StartedAt: born})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].StartedAt < list[j].StartedAt })
	return list, nil
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
