package capture

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// GitInfo describes the repository a directory belongs to.
type GitInfo struct {
	Root   string
	Branch string
	Head   string
}

// LookupGit finds the enclosing git repository of dir without spawning git.
func LookupGit(dir string) GitInfo {
	dir = filepath.Clean(dir)
	for {
		gitPath := filepath.Join(dir, ".git")
		st, err := os.Stat(gitPath)
		if err == nil {
			gitDir := gitPath
			if !st.IsDir() {
				// Worktree or submodule: ".git" is a file with "gitdir: <path>".
				data, err := os.ReadFile(gitPath)
				if err != nil {
					return GitInfo{}
				}
				line := strings.TrimSpace(string(data))
				if !strings.HasPrefix(line, "gitdir:") {
					return GitInfo{}
				}
				gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(dir, gitDir)
				}
			}
			info := GitInfo{Root: dir}
			info.Branch, info.Head = readHead(gitDir)
			return info
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return GitInfo{}
		}
		dir = parent
	}
}

func readHead(gitDir string) (branch, head string) {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "ref:") {
		return "", line // detached
	}
	ref := strings.TrimSpace(strings.TrimPrefix(line, "ref:"))
	branch = strings.TrimPrefix(ref, "refs/heads/")
	// Worktrees keep refs in the common dir.
	dirs := []string{gitDir}
	if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(common))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		dirs = append(dirs, c)
	}
	for _, d := range dirs {
		if sha, err := os.ReadFile(filepath.Join(d, ref)); err == nil {
			return branch, strings.TrimSpace(string(sha))
		}
	}
	for _, d := range dirs {
		if sha := packedRef(filepath.Join(d, "packed-refs"), ref); sha != "" {
			return branch, sha
		}
	}
	return branch, ""
}

func packedRef(path, ref string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		if sha, r, ok := strings.Cut(line, " "); ok && r == ref {
			return sha
		}
	}
	return ""
}
