// Package auth manages the API key that protects the daemon's control API.
//
// By default the daemon generates a random key on first start and keeps it in a file only the
// current user can read. The CLI and SDKs read the same file, so nothing needs configuring for the
// same user, while other users on the machine, and web pages, cannot call the API.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Path is where the key lives: <config dir>/localimage/api-key. LOCALIMAGE_CONFIG_DIR
// overrides the platform default so the daemon and SDKs can share an explicit location.
func Path() (string, error) {
	dir := os.Getenv("LOCALIMAGE_CONFIG_DIR")
	if dir == "" {
		var err error
		dir, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "localimage", "api-key"), nil
}

// Read returns the key from the key file, or "" if there is none.
func Read() string {
	p, err := Path()
	if err != nil {
		return ""
	}
	if err := secureKeyFile(p); err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// LoadOrCreate returns the key, creating the file (mode 0600) with a new random key if needed.
func LoadOrCreate() (string, error) {
	if k := Read(); k != "" {
		return k, nil
	}

	p, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	// O_EXCL: if two daemons start together, one wins and the other reads the winner's key.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			for i := 0; i < 20; i++ {
				if k := Read(); k != "" {
					return k, nil
				}
			}
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(key + "\n"); err != nil {
		return "", err
	}
	if err := secureKeyFile(p); err != nil {
		return "", err
	}
	return key, nil
}

// Rotate replaces the persisted key and returns the new value.
func Rotate() (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	tmp, err := os.CreateTemp(filepath.Dir(p), ".api-key-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := secureKeyFile(tmpName); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, p); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := os.Remove(p); err != nil {
			return "", err
		}
		if err := os.Rename(tmpName, p); err != nil {
			return "", err
		}
	}
	return key, nil
}

// Resolve is the key a client should send: LOCALIMAGE_API_KEY, else the key file.
func Resolve() string {
	if k := os.Getenv("LOCALIMAGE_API_KEY"); k != "" {
		return k
	}
	return Read()
}
