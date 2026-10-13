package session

import (
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/filters"
)

// Tags are caller-chosen key/value pairs stored as Docker labels on the
// session container, so they survive a daemon restart and can be filtered by
// Docker itself.

const (
	labelTagPrefix = "localimage.tag."
	maxTags        = 20
	maxTagValueLen = 256
)

var tagKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func validateTags(tags map[string]string) error {
	if len(tags) > maxTags {
		return invalid("at most %d tags are allowed", maxTags)
	}
	for k, v := range tags {
		if !tagKey.MatchString(k) {
			return invalid("invalid tag key %q (letters, digits, '.', '_' and '-', up to 63 characters, starting with a letter or digit)", k)
		}
		if len(v) > maxTagValueLen {
			return invalid("tag %q value exceeds %d characters", k, maxTagValueLen)
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return invalid("tag %q value contains a control character", k)
			}
		}
	}
	return nil
}

// tagsFromLabels extracts the caller's tags from a container's labels.
func tagsFromLabels(labels map[string]string) map[string]string {
	var out map[string]string
	for k, v := range labels {
		if strings.HasPrefix(k, labelTagPrefix) {
			if out == nil {
				out = map[string]string{}
			}
			out[strings.TrimPrefix(k, labelTagPrefix)] = v
		}
	}
	return out
}

// addTagFilters narrows a Docker container list to sessions carrying every
// given tag (Docker ANDs repeated label filters).
func addTagFilters(f filters.Args, tags map[string]string) {
	for k, v := range tags {
		f.Add("label", labelTagPrefix+k+"="+v)
	}
}
