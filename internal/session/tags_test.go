package session

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTags(t *testing.T) {
	ok := []map[string]string{
		nil,
		{},
		{"e2e_run": "run-123", "e2e_target": "chromium"},
		{"a": "", "B.c-d_e": "value with spaces and = signs"},
	}
	for _, tags := range ok {
		if err := validateTags(tags); err != nil {
			t.Errorf("validateTags(%v) = %v, want nil", tags, err)
		}
	}

	tooMany := map[string]string{}
	for i := 0; i < maxTags+1; i++ {
		tooMany[strings.Repeat("k", i+1)] = "v"
	}
	bad := map[string]map[string]string{
		"empty key":        {"": "v"},
		"leading dash":     {"-a": "v"},
		"space in key":     {"a b": "v"},
		"equals in key":    {"a=b": "v"},
		"slash in key":     {"a/b": "v"},
		"key too long":     {strings.Repeat("k", 64): "v"},
		"value too long":   {"k": strings.Repeat("v", maxTagValueLen+1)},
		"newline in value": {"k": "a\nb"},
		"control in value": {"k": "a\x01b"},
		"too many tags":    tooMany,
	}
	for name, tags := range bad {
		err := validateTags(tags)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error %v is not ErrInvalid", name, err)
		}
	}
}

func TestTagsFromLabels(t *testing.T) {
	labels := map[string]string{
		"localimage.managed":      "true",
		"localimage.tag.e2e_run":  "r1",
		"localimage.tag.target":   "firefox",
		"unrelated.tag.something": "x",
	}
	got := tagsFromLabels(labels)
	if len(got) != 2 || got["e2e_run"] != "r1" || got["target"] != "firefox" {
		t.Errorf("tagsFromLabels = %v", got)
	}
	if tagsFromLabels(map[string]string{"localimage.managed": "true"}) != nil {
		t.Error("expected nil when there are no tags")
	}
}
