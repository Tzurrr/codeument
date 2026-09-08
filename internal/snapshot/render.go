package snapshot

import (
	"fmt"
	"strings"
	"time"

	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/snapshot/collect"
)

// RenderOptions control the page.
type RenderOptions struct {
	// Credential resolves the credential column for an account.
	Credential func(a collect.Account) secrets.Reference
	// History is the list of recent diffs, newest first.
	History []HistoryEntry
	// ManagerName labels manager links.
	ManagerName string
}

// HistoryEntry is one past change set.
type HistoryEntry struct {
	At      time.Time
	Changes []string
}

// Render produces the Markdown body of the server page.
func Render(s *Snapshot, opts RenderOptions) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("Living page for **%s**, maintained by codeument. Last snapshot %s.\n\n", s.Hostname, s.TakenAt.Format("2006-01-02 15:04 MST"))
	if len(s.Partial) > 0 {
		w("> Some sections are incomplete: ")
		var parts []string
		for k, v := range s.Partial {
			parts = append(parts, k+" ("+v+")")
		}
		w("%s\n\n", strings.Join(parts, "; "))
	}

	w("## Overview\n\n")
	w("| | |\n|---|---|\n")
	w("| Hostname | `%s` |\n", s.OS.Hostname)
	w("| OS | %s |\n", strings.TrimSpace(s.OS.Distro+" "+s.OS.DistroVersion))
	w("| Kernel | %s |\n", s.OS.Kernel)
	w("| Architecture | %s |\n", s.OS.Arch)
	w("| Uptime | %s |\n", s.OS.Uptime)
	if s.OS.BootTime != "" {
		w("| Booted | %s |\n", s.OS.BootTime)
	}
	w("| Timezone | %s |\n", s.OS.Timezone)
	if s.OS.Virtualization != "" {
		w("| Virtualization | %s |\n", s.OS.Virtualization)
	}
	if s.OS.Cloud != "" {
		w("| Cloud | %s |\n", s.OS.Cloud)
	}
	if s.OS.Product != "" {
		w("| Hardware | %s |\n", s.OS.Product)
	}
	w("| Machine id | `%s` |\n\n", s.MachineID)

	r := s.Resources
	w("## Resources\n\n")
	w("| | |\n|---|---|\n")
	cpu := fmt.Sprintf("%d", r.CPUs)
	if r.CPUModel != "" {
		cpu += " × " + r.CPUModel
	}
	w("| CPU | %s (%.0f%% busy, load %.2f / %.2f / %.2f) |\n", cpu, r.CPUPercent, r.Load1, r.Load5, r.Load15)
	w("| Memory | %s used of %s (%.0f%%) |\n", mb(r.MemUsedMB), mb(r.MemTotalMB), r.MemPercent)
	if r.SwapTotalMB > 0 {
		w("| Swap | %s used of %s |\n", mb(r.SwapUsedMB), mb(r.SwapTotalMB))
	}
	w("\n")
	if len(r.Disks) > 0 {
		w("| Mount | Device | Type | Used | Size |\n|---|---|---|---|---|\n")
		for _, d := range r.Disks {
			flag := ""
			if d.Percent >= 90 {
				flag = " ⚠"
			}
			w("| `%s` | %s | %s | %.0f%%%s | %.0f GB |\n", d.Mount, d.Device, d.FSType, d.Percent, flag, d.TotalGB)
		}
		w("\n")
	}

	if len(s.Dirs) > 0 {
		w("## Directories\n\n")
		for _, d := range s.Dirs {
			w("### `%s`\n\n", d.Path)
			if d.Explanation != nil {
				e := d.Explanation
				w("%s", strings.TrimSpace(e.Purpose))
				if e.Confidence == "low" {
					w(" _(low confidence)_")
				}
				w("\n\n")
				if strings.TrimSpace(e.WhatRunsHere) != "" && e.WhatRunsHere != "unknown" {
					w("**Runs here:** %s\n\n", strings.TrimSpace(e.WhatRunsHere))
				}
				if strings.TrimSpace(e.HowToOperate) != "" && e.HowToOperate != "unknown" {
					w("**Operating it:** %s\n\n", strings.TrimSpace(e.HowToOperate))
				}
				if len(e.ConfigFiles) > 0 {
					w("**Config:** %s\n\n", codeList(e.ConfigFiles))
				}
				if strings.TrimSpace(e.Risks) != "" && e.Risks != "unknown" {
					w("**Careful:** %s\n\n", strings.TrimSpace(e.Risks))
				}
			} else {
				w("_Not yet explained._\n\n")
			}
			w("Size %s · %d entries", bytes(d.SizeBytes), len(d.Listing))
			if len(d.Listing) > 0 {
				show := d.Listing
				if len(show) > 12 {
					show = show[:12]
				}
				w(" · %s", codeList(show))
				if len(d.Listing) > 12 {
					w(" …")
				}
			}
			w("\n\n")
		}
	}

	if len(s.Services) > 0 {
		w("## Services\n\n| Service | Enabled | State | Description |\n|---|---|---|---|\n")
		for _, x := range s.Services {
			state := x.Active
			if state == "failed" {
				state = "**failed**"
			}
			w("| %s | %s | %s | %s |\n", x.Name, x.Enabled, state, x.Description)
		}
		w("\n")
	}
	if len(s.Ports) > 0 {
		w("## Listening ports\n\n| Port | Proto | Address | Process |\n|---|---|---|---|\n")
		for _, p := range s.Ports {
			proc := p.Process
			if p.PID > 0 {
				proc += fmt.Sprintf(" (pid %d)", p.PID)
			}
			w("| %d | %s | %s | %s |\n", p.Port, p.Proto, p.Address, proc)
		}
		w("\n")
	}
	if len(s.Cron) > 0 {
		w("## Scheduled jobs\n\n| Schedule | User | Command | Source |\n|---|---|---|---|\n")
		for _, c := range s.Cron {
			w("| `%s` | %s | `%s` | %s |\n", c.Schedule, c.User, strings.ReplaceAll(c.Command, "|", "\\|"), c.Source)
		}
		w("\n")
	}
	if len(s.Containers) > 0 {
		w("## Containers\n\n| Name | Image | Status | Ports | Project |\n|---|---|---|---|---|\n")
		for _, c := range s.Containers {
			w("| %s | %s | %s | %s | %s |\n", c.Name, c.Image, c.Status, c.Ports, c.Project)
		}
		w("\n")
	}
	if len(s.Accounts) > 0 {
		w("## Accounts and access\n\n| User | Groups | Sudo | Last login | SSH keys | Credential |\n|---|---|---|---|---|---|\n")
		for _, a := range s.Accounts {
			sudo := "no"
			if a.Sudo {
				sudo = "yes"
				if a.SudoSource != "" && a.SudoSource != "root" {
					sudo += " (" + a.SudoSource + ")"
				}
			}
			keys := fmt.Sprintf("%d", a.AuthorizedKeys)
			if len(a.KeyComments) > 0 {
				keys += " (" + strings.Join(a.KeyComments, ", ") + ")"
			}
			cred := ""
			if opts.Credential != nil {
				cred = credentialCell(opts.Credential(a), opts.ManagerName)
			}
			w("| `%s` | %s | %s | %s | %s | %s |\n", a.Username, strings.Join(a.Groups, ", "), sudo, a.LastLogin, keys, cred)
		}
		w("\n")
	}
	if len(opts.History) > 0 {
		w("## Recent changes\n\n")
		for _, h := range opts.History {
			w("- **%s**: %s\n", h.At.Format("2006-01-02"), strings.Join(h.Changes, "; "))
		}
		w("\n")
	}
	return b.String()
}

func credentialCell(ref secrets.Reference, manager string) string {
	switch ref.Mode {
	case secrets.ModeInline:
		if ref.Password != "" {
			return "`" + strings.ReplaceAll(ref.Password, "`", "'") + "`"
		}
		return "_not recorded_"
	case secrets.ModeManager:
		label := ref.Ref
		if manager != "" {
			label = manager + ": " + ref.Ref
		}
		if ref.URL != "" {
			return "[" + label + "](" + ref.URL + ")"
		}
		return label
	default:
		if ref.Ref == "" {
			return ""
		}
		return "`" + ref.Ref + "`"
	}
}

func codeList(items []string) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, "`"+it+"`")
	}
	return strings.Join(out, ", ")
}

func mb(n int64) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(n)/1024)
	}
	return fmt.Sprintf("%d MB", n)
}

func bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
