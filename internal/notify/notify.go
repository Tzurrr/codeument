// Package notify writes the one-line notice the shell hook prints at the
// next prompt.
package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Set writes the notice (an empty message clears it).
func Set(path, message string) error {
	if message == "" {
		return Clear(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(message+"\n"), 0o600)
}

// Clear removes the notice.
func Clear(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DraftsReady formats the standard notice.
func DraftsReady(n int) string {
	if n <= 0 {
		return ""
	}
	noun := "drafts"
	if n == 1 {
		noun = "draft"
	}
	return fmt.Sprintf("codeument: %d %s ready for review — run `codeument review`", n, noun)
}

// PruneSeen removes per-session markers older than maxAge.
func PruneSeen(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
