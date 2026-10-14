package session

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/docker/docker/api/types/container"
)

// Tags given at creation are Docker labels, which cannot be changed afterwards. Changes made
// later (for example marking a session "failed") are kept in a small file inside the container
// and overlaid on the labels whenever sessions are read, so they live exactly as long as the
// session does and survive a daemon restart.
const tagsFile = "/tmp/localimage-tags.json"

type tagOverlay struct {
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

// readOverlay returns the changes made since creation, or nil if there are none.
func (m *Manager) readOverlay(ctx context.Context, cid string) *tagOverlay {
	rc, _, err := m.cli.CopyFromContainer(ctx, cid, tagsFile)
	if err != nil {
		return nil
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
	var o tagOverlay
	if json.Unmarshal(data, &o) != nil {
		return nil
	}
	return &o
}

// applyOverlay merges the changes over the tags the session was created with.
func applyOverlay(base map[string]string, o *tagOverlay) map[string]string {
	if o == nil {
		return base
	}
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for _, k := range o.Remove {
		delete(out, k)
	}
	for k, v := range o.Set {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// withCurrentTags returns rec with its tags brought up to date.
func (m *Manager) withCurrentTags(ctx context.Context, cid string, rec *Record) *Record {
	rec.Tags = applyOverlay(rec.Tags, m.readOverlay(ctx, cid))
	return rec
}

// UpdateTags sets and removes tags on an existing session and returns the result. The
// same limits as at creation apply (20 tags, valid keys, 256-character values).
func (m *Manager) UpdateTags(ctx context.Context, id string, set map[string]string, remove []string) (map[string]string, error) {
	if err := validateTags(set); err != nil {
		return nil, err
	}
	cid, err := m.containerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := m.cli.ContainerInspect(ctx, cid)
	if err != nil {
		return nil, ErrNotFound
	}
	base := tagsFromLabels(info.Config.Labels)

	// Fold this change into whatever was changed before.
	prev := m.readOverlay(ctx, cid)
	next := &tagOverlay{Set: map[string]string{}}
	if prev != nil {
		for k, v := range prev.Set {
			next.Set[k] = v
		}
		next.Remove = append(next.Remove, prev.Remove...)
	}
	for k, v := range set {
		next.Set[k] = v
		next.Remove = without(next.Remove, k)
	}
	for _, k := range remove {
		delete(next.Set, k)
		if !contains(next.Remove, k) {
			next.Remove = append(next.Remove, k)
		}
	}
	merged := applyOverlay(base, next)
	if len(merged) > maxTags {
		return nil, invalid("at most %d tags are allowed", maxTags)
	}

	payload, _ := json.Marshal(next)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "localimage-tags.json", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(payload); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := m.cli.CopyToContainer(ctx, cid, "/tmp", &buf, container.CopyToContainerOptions{}); err != nil {
		return nil, err
	}
	if merged == nil {
		merged = map[string]string{}
	}
	return merged, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func without(list []string, s string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
