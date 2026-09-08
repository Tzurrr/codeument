package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/redact"
)

// DirOptions tune directory discovery.
type DirOptions struct {
	Include  []string // extra globs
	Exclude  []string // globs to skip
	Services []string // running service names, used to find /etc/<name>
	MaxDirs  int
}

// Limits for dossiers.
const (
	maxListing   = 60
	maxHeads     = 3
	maxHeadLines = 40
	maxHeadBytes = 8 * 1024
	maxWalkFiles = 20000
)

var projectMarkers = []string{".git", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", "Makefile", "package.json", "go.mod", "pyproject.toml", "requirements.txt", "Cargo.toml", "pom.xml", "build.gradle", "Gemfile", "Dockerfile", "manage.py", "artisan"}
var headFiles = []string{"README", "README.md", "README.txt", "readme.md", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", "Dockerfile", "Makefile", "*.service", "*.conf", "nginx.conf", "httpd.conf", "*.toml", "*.ini", "config.yml", "config.yaml", "app.yaml", "package.json", "go.mod", "pyproject.toml"}
var defaultExcludes = []string{"/proc", "/sys", "/dev", "/run", "/tmp", "/var/tmp", "/var/cache", "/var/log", "/usr", "/bin", "/sbin", "/lib*", "/boot", "/snap", "*/node_modules", "*/.cache", "*/.git", "/var/lib/apt", "/var/lib/dpkg", "/var/lib/docker/overlay2", "/var/lib/containerd", "/var/lib/snapd", "/home/*/.*", "/root/.*"}

// Dirs discovers important directories and builds a dossier for each.
func Dirs(ctx context.Context, e Env, opts DirOptions, partial Partial) []explain.Dossier {
	candidates := map[string]bool{}
	add := func(p string) {
		p = filepath.Clean(p)
		if p == "/" || p == "." || excluded(p, opts.Exclude) {
			return
		}
		st, err := os.Stat(e.Path(p))
		if err != nil || !st.IsDir() {
			return
		}
		candidates[p] = true
	}
	for _, s := range opts.Services {
		base := strings.TrimSuffix(s, ".service")
		for _, p := range []string{"/etc/" + base, "/opt/" + base, "/srv/" + base, "/var/lib/" + base} {
			add(p)
		}
	}
	for _, pattern := range []string{"/opt/*", "/srv/*", "/var/www/*", "/data", "/data/*", "/apps/*", "/app", "/docker/*", "/etc/nginx", "/etc/apache2", "/etc/httpd", "/etc/postgresql", "/etc/mysql", "/etc/docker", "/etc/systemd/system", "/etc/cron.d", "/etc/letsencrypt", "/var/lib/docker/volumes", "/home/*/*", "/home/*/*/*", "/root/*"} {
		for _, m := range e.Glob(pattern) {
			if strings.Contains(pattern, "/home/") || strings.HasPrefix(pattern, "/root/") {
				if !hasMarker(e, m) {
					continue
				}
			}
			add(m)
		}
	}
	for _, m := range e.Glob("/etc/*") {
		// Config dirs that also have a running service or a compose file.
		if hasMarker(e, m) {
			add(m)
		}
	}
	for _, g := range opts.Include {
		for _, m := range e.Glob(g) {
			add(m)
		}
	}
	// Drop children whose parent is already a candidate unless the child
	// has its own project marker.
	var paths []string
	for p := range candidates {
		keep := true
		for q := range candidates {
			if q != p && strings.HasPrefix(p, q+"/") && !hasMarker(e, p) {
				keep = false
				break
			}
		}
		if keep {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	max := opts.MaxDirs
	if max <= 0 {
		max = 40
	}
	if len(paths) > max {
		partial.Add("dirs", "more than "+itoa(max)+" candidate directories; keeping the first "+itoa(max))
		paths = paths[:max]
	}
	out := make([]explain.Dossier, 0, len(paths))
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		out = append(out, dossier(e, p))
	}
	return out
}

func excluded(p string, extra []string) bool {
	for _, pat := range append(append([]string{}, defaultExcludes...), extra...) {
		if ok, _ := filepath.Match(pat, p); ok {
			return true
		}
		if strings.HasSuffix(pat, "*") && strings.HasPrefix(p, strings.TrimSuffix(pat, "*")) {
			return true
		}
	}
	return false
}

func hasMarker(e Env, dir string) bool {
	for _, m := range projectMarkers {
		if e.Exists(filepath.Join(dir, m)) {
			return true
		}
	}
	return false
}

func dossier(e Env, dir string) explain.Dossier {
	d := explain.Dossier{Path: dir}
	full := e.Path(dir)
	h := sha256.New()
	var listing []string
	var size int64
	files := 0
	_ = filepath.WalkDir(full, func(p string, ent fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(full, p)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if ent.IsDir() && (depth > 2 || ent.Name() == "node_modules" || ent.Name() == ".git" || ent.Name() == ".cache" || ent.Name() == "__pycache__") {
			if depth <= 2 && len(listing) < maxListing {
				listing = append(listing, rel+"/")
				h.Write([]byte(rel + "/\n"))
			}
			return fs.SkipDir
		}
		files++
		if files > maxWalkFiles {
			return fs.SkipAll
		}
		if info, err := ent.Info(); err == nil && !ent.IsDir() {
			size += info.Size()
		}
		if depth <= 2 && len(listing) < maxListing {
			name := rel
			if ent.IsDir() {
				name += "/"
			}
			listing = append(listing, name)
			h.Write([]byte(name + "\n"))
		}
		return nil
	})
	sort.Strings(listing)
	d.Listing = listing
	d.SizeBytes = size
	// Informative file heads, redacted line by line.
	for _, pat := range headFiles {
		if len(d.Heads) >= maxHeads {
			break
		}
		for _, m := range globIn(full, pat) {
			if len(d.Heads) >= maxHeads {
				break
			}
			rel, _ := filepath.Rel(full, m)
			if Denied(m) || strings.HasPrefix(filepath.Base(m), ".env") {
				continue
			}
			content := head(m)
			if content == "" {
				continue
			}
			d.Heads = append(d.Heads, explain.Head{Name: rel, Content: content})
			h.Write([]byte(rel + "\x00" + content))
		}
	}
	d.ContentHash = hex.EncodeToString(h.Sum(nil))[:16]
	return d
}

func globIn(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Strings(m)
	return m
}

func head(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, maxHeadBytes)
	n, _ := f.Read(buf)
	if n == 0 {
		return ""
	}
	if strings.ContainsRune(string(buf[:n]), 0) {
		return "" // binary
	}
	lines := strings.Split(string(buf[:n]), "\n")
	if len(lines) > maxHeadLines {
		lines = lines[:maxHeadLines]
	}
	for i, l := range lines {
		lines[i] = redact.Default.Redact(l).Text
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
