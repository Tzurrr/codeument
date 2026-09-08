// Package capture turns shell activity into journal events: it renders the
// per-shell hook snippets, parses the records they send, and enriches them
// with git and host context.
package capture

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed hooks/*
var hookFS embed.FS

// HookOptions parameterise the rendered snippet.
type HookOptions struct {
	Binary     string // absolute path to the codeument binary
	NotifyFile string // prompt-time notification file
	SeenDir    string // directory of per-session "seen" markers
}

// Shells lists the supported shells.
var Shells = []string{"bash", "zsh", "fish"}

// RenderHook returns the hook snippet for a shell.
func RenderHook(shell string, opts HookOptions) (string, error) {
	var name string
	switch shell {
	case "bash":
		name = "hooks/bash.sh"
	case "zsh":
		name = "hooks/zsh.sh"
	case "fish":
		name = "hooks/fish.fish"
	default:
		return "", fmt.Errorf("unsupported shell %q (supported: %s)", shell, strings.Join(Shells, ", "))
	}
	src, err := hookFS.ReadFile(name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Delims("[[", "]]").Parse(string(src))
	if err != nil {
		return "", err
	}
	for _, v := range []string{opts.Binary, opts.NotifyFile, opts.SeenDir} {
		if strings.ContainsAny(v, "'\n") {
			return "", fmt.Errorf("path %q contains characters the hook cannot quote", v)
		}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, opts); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RCLine is the line to add to a shell rc file.
func RCLine(shell, binary string) string {
	switch shell {
	case "fish":
		return fmt.Sprintf("%s hook fish | source", binary)
	default:
		return fmt.Sprintf(`eval "$(%s hook %s)"`, binary, shell)
	}
}

// RCFile is the rc file the hook line goes into.
func RCFile(shell, home string) string {
	switch shell {
	case "bash":
		return home + "/.bashrc"
	case "zsh":
		return home + "/.zshrc"
	case "fish":
		return home + "/.config/fish/conf.d/codeument.fish"
	}
	return ""
}
