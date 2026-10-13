package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RecordingInfo describes one recording. Times come from the container clock, which is the
// host clock, so they line up with a test run's own timestamps.
type RecordingInfo struct {
	ID          string `json:"replay_id"`
	Status      string `json:"status"` // "recording" or "stopped"
	Running     bool   `json:"running"`
	StartedAt   string `json:"started_at"` // RFC 3339, UTC, millisecond precision
	StartedAtMS int64  `json:"started_at_ms"`
	StoppedAt   string `json:"stopped_at,omitempty"` // when capture was told to stop: the video's end
	StoppedAtMS int64  `json:"stopped_at_ms,omitempty"`
	// FinalizedAt is when the file was complete, a moment after StoppedAt.
	FinalizedAt   string  `json:"finalized_at,omitempty"`
	FinalizedAtMS int64   `json:"finalized_at_ms,omitempty"`
	DurationMS    int64   `json:"duration_ms,omitempty"` // media duration once stopped
	Size          int64   `json:"size"`
	ContentType   string  `json:"content_type"` // always video/mp4
	Format        string  `json:"format"`
	Codec         string  `json:"codec,omitempty"`
	PixFmt        string  `json:"pix_fmt,omitempty"`
	Width         int     `json:"width,omitempty"`
	Height        int     `json:"height,omitempty"`
	FPS           float64 `json:"fps,omitempty"`
}

func isoMS(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }

// StartRecording begins capturing the display to an MP4 and returns once the file is being
// written. started_at is when capture began, which is the video's time zero. The file is a
// fragmented MP4, so it stays playable even if the container is killed. Only one recording
// can run at a time per session.
func (m *Manager) StartRecording(ctx context.Context, id string) (*RecordingInfo, error) {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	rid := randHex(4)
	script := fmt.Sprintf(`
d=%[1]s; rid=%[2]s
mkdir -p $d
for f in $d/*.pid; do
  [ -f "$f" ] && kill -0 "$(cat "$f")" 2>/dev/null && { echo "a recording is already running"; exit 3; }
done
ms=$(date +%%s%%3N)
nohup ffmpeg -y -loglevel error -f x11grab -framerate 15 -i :99.0 \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
  -movflags +frag_keyframe+empty_moov+default_base_moof $d/$rid.mp4 </dev/null >$d/$rid.log 2>&1 &
echo $! > $d/$rid.pid
echo $ms > $d/$rid.start
i=0
while [ $i -lt 40 ]; do
  [ -s $d/$rid.mp4 ] && break
  kill -0 "$(cat $d/$rid.pid)" 2>/dev/null || { cat $d/$rid.log; exit 4; }
  sleep 0.1; i=$((i+1))
done
[ -s $d/$rid.mp4 ] || { echo "the recorder produced no output"; exit 4; }
echo $ms
`, recDir, rid)
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c", script)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("start recording: %s", bytes.TrimSpace([]byte(out)))
	}
	return m.recordingInfo(ctx, cid, rid, false)
}

// StopRecording finalizes a recording (the running one if rid is empty). The file is complete
// when this returns, and the result carries its measured duration and video details.
func (m *Manager) StopRecording(ctx context.Context, id, rid string) (*RecordingInfo, error) {
	if rid != "" && !recID.MatchString(rid) {
		return nil, invalid("invalid recording id")
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`
d=%[1]s
rid="$1"
if [ -z "$rid" ]; then
  for f in $d/*.pid; do
    [ -f "$f" ] && kill -0 "$(cat "$f")" 2>/dev/null && rid=$(basename "$f" .pid)
  done
fi
[ -n "$rid" ] && [ -f $d/$rid.pid ] || { echo "no running recording"; exit 3; }
pid=$(cat $d/$rid.pid)
date +%%s%%3N > $d/$rid.stop
kill -INT "$pid" 2>/dev/null
i=0
while [ $i -lt 100 ]; do
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.1; i=$((i+1))
done
kill -0 "$pid" 2>/dev/null && kill -KILL "$pid"
date +%%s%%3N > $d/$rid.done
echo "$rid"
`, recDir)
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c", script, "sh", rid)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("stop recording: %s", bytes.TrimSpace([]byte(out)))
	}
	return m.recordingInfo(ctx, cid, strings.TrimSpace(out), true)
}

// Recordings lists a session's recordings, oldest first.
func (m *Manager) Recordings(ctx context.Context, id string) ([]RecordingInfo, error) {
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c",
		fmt.Sprintf(`for f in %s/*.mp4; do [ -f "$f" ] && basename "$f" .mp4; done`, recDir))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("list recordings: %s", bytes.TrimSpace([]byte(out)))
	}
	list := []RecordingInfo{}
	for _, rid := range strings.Fields(out) {
		if !recID.MatchString(rid) {
			continue
		}
		info, err := m.recordingInfo(ctx, cid, rid, false)
		if err != nil {
			return nil, err
		}
		list = append(list, *info)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].StartedAtMS < list[j].StartedAtMS })
	return list, nil
}

// recordingInfo reads a recording's state from the container. With probe it also measures the
// finished file with ffprobe.
func (m *Manager) recordingInfo(ctx context.Context, cid, rid string, probe bool) (*RecordingInfo, error) {
	out, code, err := m.run(ctx, cid, "neko", "sh", "-c", fmt.Sprintf(`
d=%s; rid=%s
run=0; [ -f $d/$rid.pid ] && kill -0 "$(cat $d/$rid.pid)" 2>/dev/null && run=1
printf '%%s\t%%s\t%%s\t%%s\t%%s\n' "$run" "$(cat $d/$rid.start 2>/dev/null)" "$(cat $d/$rid.stop 2>/dev/null)" "$(stat -c %%s $d/$rid.mp4 2>/dev/null)" "$(cat $d/$rid.done 2>/dev/null)"
`, recDir, rid))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("recording %s: %s", rid, bytes.TrimSpace([]byte(out)))
	}
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	if len(f) != 5 {
		return nil, fmt.Errorf("recording %s: unexpected state %q", rid, out)
	}
	num := func(s string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64); return n }
	info := &RecordingInfo{
		ID:            rid,
		Running:       f[0] == "1",
		StartedAtMS:   num(f[1]),
		StoppedAtMS:   num(f[2]),
		Size:          num(f[3]),
		FinalizedAtMS: num(f[4]),
		ContentType:   "video/mp4",
		Format:        "mp4",
		Status:        "stopped",
	}
	if info.Running {
		info.Status = "recording"
	}
	info.StartedAt = isoMS(info.StartedAtMS)
	if info.StoppedAtMS > 0 {
		info.StoppedAt = isoMS(info.StoppedAtMS)
		info.DurationMS = info.StoppedAtMS - info.StartedAtMS
	}
	if info.FinalizedAtMS > 0 {
		info.FinalizedAt = isoMS(info.FinalizedAtMS)
	}
	if !probe || info.Running {
		return info, nil
	}

	pout, pcode, err := m.run(ctx, cid, "neko", "ffprobe", "-v", "error", "-show_entries",
		"format=duration:stream=codec_name,pix_fmt,width,height,r_frame_rate", "-of", "json", recDir+"/"+rid+".mp4")
	if err != nil || pcode != 0 {
		return info, nil // the recording is fine; only its details are missing
	}
	var p struct {
		Format  struct{ Duration string }
		Streams []struct {
			Codec  string `json:"codec_name"`
			PixFmt string `json:"pix_fmt"`
			Width  int
			Height int
			Rate   string `json:"r_frame_rate"`
		}
	}
	if json.Unmarshal([]byte(pout), &p) != nil {
		return info, nil
	}
	if d, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil {
		info.DurationMS = int64(math.Round(d * 1000))
	}
	if len(p.Streams) > 0 {
		st := p.Streams[0]
		info.Codec, info.PixFmt, info.Width, info.Height = st.Codec, st.PixFmt, st.Width, st.Height
		if n, d, ok := strings.Cut(st.Rate, "/"); ok {
			if a, b := num(n), num(d); b > 0 {
				info.FPS = float64(a) / float64(b)
			}
		}
	}
	return info, nil
}
