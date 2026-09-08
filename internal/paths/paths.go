// Package paths resolves where codeument keeps its config, data and state,
// honouring XDG conventions and CODEUMENT_* overrides.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// EnvConfig overrides the config file path.
	EnvConfig = "CODEUMENT_CONFIG"
	// EnvDataDir overrides the data directory.
	EnvDataDir = "CODEUMENT_DATA_DIR"
)

// Home returns the user's home directory, falling back to the current
// directory when it cannot be determined.
func Home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// Expand replaces a leading ~ with the home directory and expands $VARS.
func Expand(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(Home(), strings.TrimPrefix(p, "~"))
	}
	return os.ExpandEnv(p)
}

// UserConfigPath is the per-user config file location.
func UserConfigPath() string {
	if v := os.Getenv(EnvConfig); v != "" {
		return Expand(v)
	}
	return filepath.Join(userConfigDir(), "codeument", "config.yaml")
}

// SystemConfigPath is the machine-wide config file, merged under the user file.
func SystemConfigPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "codeument", "config.yaml")
	}
	return "/etc/codeument/config.yaml"
}

// RelayConfigPath is the default relay config location.
func RelayConfigPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "codeument", "relay.yaml")
	}
	return "/etc/codeument/relay.yaml"
}

// DefaultDataDir is where the journal and state live unless configured.
func DefaultDataDir() string {
	if v := os.Getenv(EnvDataDir); v != "" {
		return Expand(v)
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "codeument")
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(Home(), "Library", "Application Support", "codeument")
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "codeument")
	}
	return filepath.Join(Home(), ".local", "share", "codeument")
}

func userConfigDir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows":
		return os.Getenv("APPDATA")
	}
	return filepath.Join(Home(), ".config")
}

// EnsureDir creates dir with mode 0700 if it does not exist.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// WriteFilePrivate writes data to path with 0600 permissions, creating the
// parent directory (0700) when needed. The write is atomic: a temp file is
// renamed into place.
func WriteFilePrivate(path string, data []byte) error {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
