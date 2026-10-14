package session

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/docker/docker/api/types/build"
)

// buildImage builds assets/<name> into the local Docker image `tag`. It runs
// once per machine; later sessions reuse the cached image.
func (m *Manager) buildImage(ctx context.Context, name, tag string, onLog func(string)) error {
	root := "assets/" + name
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := fs.WalkDir(assets, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		rel := p[len(root)+1:]
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}

	slog.Info("building image (first use only; this can take a few minutes)", "image", tag)
	resp, err := m.cli.ImageBuild(ctx, &buf, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
		BuildArgs:  buildArgs(),
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("image build failed: %s", msg.Error)
		}
		if onLog != nil {
			// Only the step headlines: the full apt output is thousands of lines.
			for _, line := range strings.Split(msg.Stream, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Step ") || strings.HasPrefix(line, "Successfully") {
					if len(line) > 160 {
						line = line[:160] + "..."
					}
					onLog(line)
				}
			}
		}
	}
}

func strPtr(s string) *string { return &s }

// buildArgs are the Dockerfile arguments: the pinned Playwright version, and, if
// LOCALIMAGE_APT_SNAPSHOT is set (for example 20260930T000000Z), the Debian snapshot to install
// packages from.
func buildArgs() map[string]*string {
	args := map[string]*string{"PW_VERSION": strPtr(PlaywrightVersion)}
	if snap := os.Getenv("LOCALIMAGE_APT_SNAPSHOT"); snap != "" {
		args["APT_SNAPSHOT"] = strPtr(snap)
	}
	return args
}
