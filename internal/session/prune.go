package session

import (
	"context"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

// currentImages is the set of image tags this build of the daemon uses.
func currentImages() map[string]bool {
	out := map[string]bool{}
	for _, spec := range browsers {
		out[spec.image] = true
	}
	return out
}

// staleTags lists localimage browser image tags that this build does not use. It looks at tags,
// not images: an old tag can point at the same image as a current one (nothing to reclaim, but
// still clutter), and a stale tag is stale whatever else shares its image.
func (m *Manager) staleTags(ctx context.Context) ([]string, error) {
	f := filters.NewArgs(filters.Arg("reference", "localimage/chromium"), filters.Arg("reference", "localimage/firefox"))
	list, err := m.cli.ImageList(ctx, image.ListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	current := currentImages()
	var stale []string
	for _, img := range list {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, "localimage/") && !current[tag] {
				stale = append(stale, tag)
			}
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// StaleImageCount says how many old browser images are on disk (see Prune).
func (m *Manager) StaleImageCount(ctx context.Context) int {
	stale, _ := m.staleTags(ctx)
	return len(stale)
}

// Prune removes browser image tags left behind by older versions. Images a container is using,
// and the current ones, are kept. It returns the tags it removed.
func (m *Manager) Prune(ctx context.Context) ([]string, error) {
	stale, err := m.staleTags(ctx)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, tag := range stale {
		if _, err := m.cli.ImageRemove(ctx, tag, image.RemoveOptions{}); err == nil {
			removed = append(removed, tag)
		} // in use by a container: leave it
	}
	return removed, nil
}
