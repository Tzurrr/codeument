// Package collect gathers facts about the machine for a server snapshot.
// Each collector reads files under Env.Root (so tests can point it at a
// fixture tree) and runs external commands through Env.Run (so tests can
// replace them). Collectors are best-effort: they return what they could
// read and a reason when they could not read everything.
package collect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Env is the environment collectors read from.
type Env struct {
	// Root is prefixed to every absolute path read (default "/").
	Root string
	// Run executes a command and returns its stdout.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Now is the clock.
	Now func() time.Time
}

// Default is the real machine.
var Default = Env{Root: "/", Run: runCommand, Now: time.Now}

// ErrDenied is returned for files collectors must never read.
var ErrDenied = errors.New("collect: reading this file is not allowed")

// denied lists path fragments that are never read, whatever the collector.
var denied = []string{"/etc/shadow", "/etc/gshadow", "/etc/security/opasswd", "/.ssh/id_", "/etc/ssl/private", "/etc/letsencrypt/live", "/etc/letsencrypt/archive", ".pem", ".key", "/etc/krb5.keytab"}

// Denied reports whether a path is off-limits.
func Denied(path string) bool {
	lower := strings.ToLower(path)
	for _, d := range denied {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

// Path resolves an absolute path under Root.
func (e Env) Path(p string) string {
	if e.Root == "" || e.Root == "/" {
		return p
	}
	return filepath.Join(e.Root, p)
}

// ReadFile reads a file under Root, refusing denied paths.
func (e Env) ReadFile(p string) ([]byte, error) {
	if Denied(p) {
		return nil, ErrDenied
	}
	return os.ReadFile(e.Path(p))
}

// ReadLines reads a file as trimmed lines, dropping blanks and comments.
func (e Env) ReadLines(p string) ([]string, error) {
	data, err := e.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// Exists reports whether a path exists under Root.
func (e Env) Exists(p string) bool {
	_, err := os.Stat(e.Path(p))
	return err == nil
}

// Glob globs under Root and returns unprefixed paths.
func (e Env) Glob(pattern string) []string {
	matches, _ := filepath.Glob(e.Path(pattern))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, e.strip(m))
	}
	return out
}

func (e Env) strip(p string) string {
	if e.Root == "" || e.Root == "/" {
		return p
	}
	rel, err := filepath.Rel(e.Root, p)
	if err != nil {
		return p
	}
	return "/" + rel
}

// Command runs a command with a timeout, returning stdout.
func (e Env) Command(ctx context.Context, name string, args ...string) (string, error) {
	if e.Run == nil {
		return "", errors.New("no command runner")
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return e.Run(cctx, name, args...)
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("%s: not installed", name)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(out) > 0 {
			return string(out), nil // partial output is still useful
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// Partial records why a collector could not read everything.
type Partial map[string]string

// Add appends a reason for a collector.
func (p Partial) Add(collector, reason string) {
	if reason == "" {
		return
	}
	if prev, ok := p[collector]; ok {
		p[collector] = prev + "; " + reason
		return
	}
	p[collector] = reason
}

// Fields splits on whitespace.
func Fields(s string) []string { return strings.Fields(s) }
