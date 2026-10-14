package auth

import (
	"os"
	"runtime"
	"testing"
)

// useTempConfig points the config directory at a fresh temp dir for the test.
func useTempConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux
	t.Setenv("HOME", dir)            // macOS and fallback
	t.Setenv("AppData", dir)         // Windows
	t.Setenv("LOCALIMAGE_API_KEY", "")
}

func TestLoadOrCreateMakesAPrivateKeyOnce(t *testing.T) {
	useTempConfig(t)
	if Read() != "" {
		t.Fatal("a key exists before one was created")
	}
	k1, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) < 32 {
		t.Errorf("key %q is too short", k1)
	}
	k2, err := LoadOrCreate()
	if err != nil || k2 != k1 {
		t.Errorf("second call = %q, %v; want the same key", k2, err)
	}
	if Read() != k1 {
		t.Error("Read does not return the stored key")
	}
	if runtime.GOOS != "windows" {
		p, _ := Path()
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("key file mode = %o, want 600: other users must not read it", mode)
		}
	}
}

func TestResolvePrefersTheEnvironment(t *testing.T) {
	useTempConfig(t)
	stored, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if Resolve() != stored {
		t.Error("with no env key, the stored key is used")
	}
	t.Setenv("LOCALIMAGE_API_KEY", "explicit")
	if Resolve() != "explicit" {
		t.Error("an explicit key must win over the file")
	}
}

func TestLooseKeyFileIsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not permissions on Windows")
	}
	useTempConfig(t)
	key, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := Path()
	if err := os.Chmod(p, 0o644); err != nil { // as a careless copy or backup might leave it
		t.Fatal(err)
	}
	if Read() != key {
		t.Fatal("the key is still readable by its owner")
	}
	info, _ := os.Stat(p)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("a world-readable key file was left at %o, want 600", mode)
	}
}

func TestNoConfigDirIsAnErrorNotASilentFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("AppData", "")
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("the config directory is found another way here")
	}
	if _, err := LoadOrCreate(); err == nil {
		t.Error("LoadOrCreate succeeded with no config directory; the daemon would run unauthenticated")
	}
}
