//go:build !windows

package auth

import "os"

func secureKeyFile(path string) error {
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(path, 0o600)
	}
	return nil
}
