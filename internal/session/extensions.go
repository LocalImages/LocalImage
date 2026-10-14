package session

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
)

// Extensions are unpacked Chromium extensions, each kept in its own Docker
// volume and mounted read-only into the sessions that ask for them.

const (
	labelExtension = "localimage.extension"
	labelExtTitle  = "localimage.extension.title"
	labelExtVer    = "localimage.extension.version"
	labelExts      = "localimage.extensions" // on containers: comma-separated names

	extMountDir = "/opt/localimage/extensions"

	maxZipBytes    = 50 << 20  // compressed upload
	maxUnzipBytes  = 200 << 20 // total uncompressed, guards against zip bombs
	maxExtFiles    = 5000
	extVolumeImage = "chromium" // any local image works; it is never started
)

type Extension struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

func extVolume(name string) string { return "localimage-ext-" + name }

type manifest struct {
	ManifestVersion int    `json:"manifest_version"`
	Name            string `json:"name"`
	Version         string `json:"version"`
}

// zipToTar validates a zip of an unpacked extension and converts it to a tar
// stream owned by the browser user. manifest.json may sit at the zip root or
// inside a single top-level folder, which is stripped.
func zipToTar(data []byte) (*bytes.Buffer, *manifest, error) {
	if len(data) > maxZipBytes {
		return nil, nil, fmt.Errorf("zip exceeds %d MiB", maxZipBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("not a valid zip file: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxExtFiles {
		return nil, nil, fmt.Errorf("zip must contain 1-%d files", maxExtFiles)
	}

	// Locate manifest.json to decide how much leading path to strip.
	prefix, found := "", false
	for _, f := range zr.File {
		n := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if n == "manifest.json" {
			prefix, found = "", true
			break
		}
		if dir, base := path.Split(n); base == "manifest.json" && strings.Count(dir, "/") == 1 && !found {
			prefix, found = dir, true
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("manifest.json not found at the zip root or in a single top-level folder")
	}

	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	var man manifest
	var total int64
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if !strings.HasPrefix(name+"/", prefix) && prefix != "" {
			continue // outside the extension folder (e.g. __MACOSX)
		}
		name = strings.TrimPrefix(name, prefix)
		if name == "" || name == "." {
			continue
		}
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return nil, nil, fmt.Errorf("unsafe path in zip: %q", f.Name)
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("symlinks are not allowed: %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000}); err != nil {
				return nil, nil, err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, maxUnzipBytes-total+1))
		rc.Close()
		if err != nil {
			return nil, nil, err
		}
		if total += int64(len(body)); total > maxUnzipBytes {
			return nil, nil, fmt.Errorf("extension exceeds %d MiB unpacked", maxUnzipBytes>>20)
		}
		if name == "manifest.json" {
			if err := json.Unmarshal(body, &man); err != nil {
				return nil, nil, fmt.Errorf("manifest.json is not valid JSON: %w", err)
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Uid: 1000, Gid: 1000}); err != nil {
			return nil, nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	if man.ManifestVersion != 2 && man.ManifestVersion != 3 {
		return nil, nil, fmt.Errorf("manifest.json needs manifest_version 2 or 3")
	}
	return &out, &man, nil
}

// extSlots bounds concurrent extension uploads.
var extSlots = make(chan struct{}, 2)

// AddExtension stores (or replaces) the extension called name from a zip.
func (m *Manager) AddExtension(ctx context.Context, name string, zipData []byte) (*Extension, error) {
	if !profileName.MatchString(name) {
		return nil, invalid("invalid extension name %q (use a-z, 0-9, - and _)", name)
	}
	// Unpacking holds the whole extension in memory (up to maxUnzipBytes), so only a couple at a time.
	select {
	case extSlots <- struct{}{}:
		defer func() { <-extSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	tarball, man, err := zipToTar(zipData)
	if err != nil {
		return nil, err
	}
	spec := browsers[extVolumeImage]
	if err := m.ensureImage(ctx, extVolumeImage, spec, nil); err != nil {
		return nil, err
	}
	// Replace any previous version; this fails while a session uses it.
	if _, err := m.cli.VolumeInspect(ctx, extVolume(name)); err == nil {
		if err := m.cli.VolumeRemove(ctx, extVolume(name), false); err != nil {
			return nil, fmt.Errorf("extension %q is in use by a session", name)
		}
	}
	if _, err := m.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: extVolume(name),
		Labels: map[string]string{
			labelExtension: name, labelExtTitle: man.Name, labelExtVer: man.Version,
		},
	}); err != nil {
		return nil, err
	}
	// A created-but-never-started container is the way to write into a volume.
	tmp, err := m.cli.ContainerCreate(ctx,
		&container.Config{Image: spec.image, Cmd: []string{"true"}},
		&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: extVolume(name), Target: "/ext"}}},
		nil, nil, "")
	if err != nil {
		return nil, err
	}
	defer m.cli.ContainerRemove(context.Background(), tmp.ID, container.RemoveOptions{Force: true})
	if err := m.cli.CopyToContainer(ctx, tmp.ID, "/ext", tarball, container.CopyToContainerOptions{}); err != nil {
		_ = m.cli.VolumeRemove(context.Background(), extVolume(name), true)
		return nil, err
	}
	return &Extension{Name: name, Title: man.Name, Version: man.Version}, nil
}

func (m *Manager) Extensions(ctx context.Context) ([]Extension, error) {
	res, err := m.cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(filters.Arg("label", labelExtension))})
	if err != nil {
		return nil, err
	}
	out := []Extension{}
	for _, v := range res.Volumes {
		out = append(out, Extension{Name: v.Labels[labelExtension], Title: v.Labels[labelExtTitle], Version: v.Labels[labelExtVer]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) DeleteExtension(ctx context.Context, name string) error {
	if !profileName.MatchString(name) {
		return fmt.Errorf("invalid extension name")
	}
	return m.cli.VolumeRemove(ctx, extVolume(name), false)
}

// extensionMounts validates the requested extensions and returns the mounts
// and the Chromium flag that loads them.
func (m *Manager) extensionMounts(ctx context.Context, names []string) ([]mount.Mount, string, error) {
	var mounts []mount.Mount
	var dirs []string
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if !profileName.MatchString(n) {
			return nil, "", invalid("invalid extension name %q", n)
		}
		if _, err := m.cli.VolumeInspect(ctx, extVolume(n)); err != nil {
			return nil, "", invalid("extension %q not found (upload it first)", n)
		}
		target := extMountDir + "/" + n
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: extVolume(n), Target: target, ReadOnly: true})
		dirs = append(dirs, target)
	}
	if len(dirs) == 0 {
		return nil, "", nil
	}
	return mounts, "--load-extension=" + strings.Join(dirs, ","), nil
}
