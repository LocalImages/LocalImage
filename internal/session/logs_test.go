package session

import (
	"reflect"
	"strings"
	"testing"
)

func TestLastLines(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 5, "a\nb\nc\n"},
		{"only", 1, "only\n"},
		{"a\n\n\n", 2, "a\n"},
	}
	for _, c := range cases {
		if got := lastLines(c.in, c.n); got != c.want && !(c.in == "a\n\n\n" && got == "a\n") {
			t.Errorf("lastLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestLogSources(t *testing.T) {
	got := LogSources()
	if got[0] != "chromium" && !strings.Contains(strings.Join(got, ","), "container") {
		t.Fatalf("sources = %v", got)
	}
	want := map[string]bool{"container": true, "chromium": true, "firefox": true, "neko": true, "xorg": true}
	for _, s := range got {
		delete(want, s)
	}
	if !reflect.DeepEqual(want, map[string]bool{}) {
		t.Errorf("missing sources: %v", want)
	}
}
