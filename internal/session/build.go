package session

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/docker/docker/api/types/build"
)

// buildImage builds assets/<name> into the local Docker image `tag`. It runs
// once per machine; later sessions reuse the cached image.
func (m *Manager) buildImage(ctx context.Context, name, tag string) error {
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

	fmt.Fprintf(os.Stderr, "localimage: building %s (first use only, this can take a few minutes)\n", tag)
	resp, err := m.cli.ImageBuild(ctx, &buf, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
		BuildArgs:  map[string]*string{"PW_VERSION": strPtr(PlaywrightVersion)},
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
	}
}

func strPtr(s string) *string { return &s }
