package session

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	mode       uint32 // 0 = regular file
}

func makeZip(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(fs.ModeSymlink | 0o777)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, e.body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tarNames(t *testing.T, b *bytes.Buffer) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(b)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 1000 || h.Gid != 1000 {
			t.Errorf("%s owned by %d:%d, want 1000:1000", h.Name, h.Uid, h.Gid)
		}
		names = append(names, h.Name)
	}
}

const mv3 = `{"manifest_version":3,"name":"Demo","version":"1.2"}`

func TestZipToTar(t *testing.T) {
	cases := []struct {
		name    string
		entries []entry
		wantErr string
		want    []string
	}{
		{"root manifest", []entry{{name: "manifest.json", body: mv3}, {name: "sw.js", body: "x"}}, "", []string{"manifest.json", "sw.js"}},
		{"strips single top folder", []entry{{name: "my-ext/manifest.json", body: mv3}, {name: "my-ext/js/a.js", body: "x"}}, "", []string{"manifest.json", "js/a.js"}},
		{"ignores files outside the folder", []entry{{name: "my-ext/manifest.json", body: mv3}, {name: "__MACOSX/junk", body: "x"}}, "", []string{"manifest.json"}},
		{"no manifest", []entry{{name: "a.js", body: "x"}}, "manifest.json not found", nil},
		{"manifest too deep", []entry{{name: "a/b/manifest.json", body: mv3}}, "manifest.json not found", nil},
		{"bad json", []entry{{name: "manifest.json", body: "{nope"}}, "not valid JSON", nil},
		{"unsupported manifest version", []entry{{name: "manifest.json", body: `{"manifest_version":1}`}}, "manifest_version 2 or 3", nil},
		{"path traversal", []entry{{name: "manifest.json", body: mv3}, {name: "../evil.js", body: "x"}}, "unsafe path", nil},
		{"nested traversal", []entry{{name: "manifest.json", body: mv3}, {name: "a/../../evil.js", body: "x"}}, "unsafe path", nil},
		{"absolute path", []entry{{name: "manifest.json", body: mv3}, {name: "/etc/passwd", body: "x"}}, "unsafe path", nil},
		{"symlink", []entry{{name: "manifest.json", body: mv3}, {name: "link", body: "/etc/passwd", mode: 1}}, "symlinks are not allowed", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, man, err := zipToTar(makeZip(t, c.entries...))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if man.Name != "Demo" || man.Version != "1.2" {
				t.Errorf("manifest = %+v", man)
			}
			got := tarNames(t, out)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("entries = %v, want %v", got, c.want)
			}
		})
	}
}

func TestZipToTarRejectsGarbageAndBombs(t *testing.T) {
	if _, _, err := zipToTar([]byte("not a zip")); err == nil {
		t.Error("garbage accepted")
	}
	big := strings.Repeat("a", 1<<20)
	entries := []entry{{name: "manifest.json", body: mv3}}
	for i := 0; i < 210; i++ { // 210 MiB unpacked, compresses to almost nothing
		entries = append(entries, entry{name: "f" + string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ".bin", body: big})
	}
	if _, _, err := zipToTar(makeZip(t, entries...)); err == nil || !strings.Contains(err.Error(), "unpacked") {
		t.Errorf("zip bomb not rejected: %v", err)
	}
}
