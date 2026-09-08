package snapshot

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Change is one difference between two snapshots.
type Change struct {
	Section string `json:"section"`
	Kind    string `json:"kind"` // added | removed | changed
	Key     string `json:"key"`
	Detail  string `json:"detail,omitempty"`
}

// Diff is the set of changes between snapshots.
type Diff struct {
	From     string   `json:"from,omitempty"`
	To       string   `json:"to"`
	Changes  []Change `json:"changes"`
	Material bool     `json:"material"`
}

// Compare diffs prev (may be nil) against cur. Resource noise (CPU, load,
// small disk movements) is reported but not material.
func Compare(prev, cur *Snapshot) Diff {
	d := Diff{To: cur.ID}
	if prev == nil {
		d.Material = true
		d.Changes = append(d.Changes, Change{Section: "snapshot", Kind: "added", Key: cur.Hostname, Detail: "first snapshot"})
		return d
	}
	d.From = prev.ID
	add := func(c Change, material bool) {
		d.Changes = append(d.Changes, c)
		if material {
			d.Material = true
		}
	}
	// OS
	for _, f := range []struct{ name, a, b string }{
		{"distro", prev.OS.Distro + " " + prev.OS.DistroVersion, cur.OS.Distro + " " + cur.OS.DistroVersion},
		{"kernel", prev.OS.Kernel, cur.OS.Kernel},
		{"hostname", prev.OS.Hostname, cur.OS.Hostname},
		{"timezone", prev.OS.Timezone, cur.OS.Timezone},
	} {
		if f.a != f.b {
			add(Change{Section: "os", Kind: "changed", Key: f.name, Detail: f.a + " -> " + f.b}, true)
		}
	}
	if cur.OS.BootTime != "" && prev.OS.BootTime != "" && cur.OS.BootTime != prev.OS.BootTime {
		add(Change{Section: "os", Kind: "changed", Key: "boot", Detail: "rebooted at " + cur.OS.BootTime}, true)
	}
	// Resources
	if prev.Resources.CPUs != cur.Resources.CPUs {
		add(Change{Section: "resources", Kind: "changed", Key: "cpus", Detail: fmt.Sprintf("%d -> %d", prev.Resources.CPUs, cur.Resources.CPUs)}, true)
	}
	if math.Abs(float64(prev.Resources.MemTotalMB-cur.Resources.MemTotalMB)) > 64 {
		add(Change{Section: "resources", Kind: "changed", Key: "memory", Detail: fmt.Sprintf("%d MB -> %d MB", prev.Resources.MemTotalMB, cur.Resources.MemTotalMB)}, true)
	}
	prevDisks := map[string]float64{}
	for _, dk := range prev.Resources.Disks {
		prevDisks[dk.Mount] = dk.Percent
	}
	for _, dk := range cur.Resources.Disks {
		p, ok := prevDisks[dk.Mount]
		switch {
		case !ok:
			add(Change{Section: "disks", Kind: "added", Key: dk.Mount, Detail: fmt.Sprintf("%s %.0f%% used", dk.Device, dk.Percent)}, true)
		case math.Abs(p-dk.Percent) >= 10 || (dk.Percent >= 90 && p < 90):
			add(Change{Section: "disks", Kind: "changed", Key: dk.Mount, Detail: fmt.Sprintf("%.0f%% -> %.0f%% used", p, dk.Percent)}, dk.Percent >= 90 || math.Abs(p-dk.Percent) >= 20)
		}
		delete(prevDisks, dk.Mount)
	}
	for m := range prevDisks {
		add(Change{Section: "disks", Kind: "removed", Key: m}, true)
	}
	// Keyed lists
	diffKeyed(&d, "ports", keyed(prev.Ports, func(p interface{ Key() string }) string { return p.Key() }), keyed(cur.Ports, func(p interface{ Key() string }) string { return p.Key() }), func(k string) string {
		for _, p := range cur.Ports {
			if p.Key() == k {
				return p.Process
			}
		}
		return ""
	})
	diffKeyed(&d, "services", keyedServices(prev), keyedServices(cur), nil)
	diffKeyed(&d, "cron", keyed(prev.Cron, func(p interface{ Key() string }) string { return p.Key() }), keyed(cur.Cron, func(p interface{ Key() string }) string { return p.Key() }), nil)
	diffKeyed(&d, "containers", keyedContainers(prev), keyedContainers(cur), nil)
	diffKeyed(&d, "accounts", keyedAccounts(prev), keyedAccounts(cur), nil)
	diffKeyed(&d, "dirs", keyedDirs(prev), keyedDirs(cur), nil)
	sort.SliceStable(d.Changes, func(i, j int) bool {
		if d.Changes[i].Section != d.Changes[j].Section {
			return d.Changes[i].Section < d.Changes[j].Section
		}
		return d.Changes[i].Key < d.Changes[j].Key
	})
	return d
}

// keyed builds key -> fingerprint for a slice of Key()ers.
func keyed[T interface{ Key() string }](items []T, _ func(interface{ Key() string }) string) map[string]string {
	out := map[string]string{}
	for _, it := range items {
		out[it.Key()] = it.Key()
	}
	return out
}

func keyedServices(s *Snapshot) map[string]string {
	out := map[string]string{}
	for _, x := range s.Services {
		out[x.Name] = x.Enabled + "/" + x.Active
	}
	return out
}

func keyedContainers(s *Snapshot) map[string]string {
	out := map[string]string{}
	for _, x := range s.Containers {
		state := "stopped"
		if strings.HasPrefix(x.Status, "Up") {
			state = "up"
		}
		out[x.Key()] = x.Image + " " + state
	}
	return out
}

func keyedAccounts(s *Snapshot) map[string]string {
	out := map[string]string{}
	for _, a := range s.Accounts {
		out[a.Username] = fmt.Sprintf("uid=%d shell=%s sudo=%v groups=%s keys=%d", a.UID, a.Shell, a.Sudo, strings.Join(a.Groups, ","), a.AuthorizedKeys)
	}
	return out
}

func keyedDirs(s *Snapshot) map[string]string {
	out := map[string]string{}
	for _, d := range s.Dirs {
		out[d.Path] = d.ContentHash
	}
	return out
}

func diffKeyed(d *Diff, section string, prev, cur map[string]string, detail func(string) string) {
	for k, v := range cur {
		pv, ok := prev[k]
		switch {
		case !ok:
			det := ""
			if detail != nil {
				det = detail(k)
			}
			d.Changes = append(d.Changes, Change{Section: section, Kind: "added", Key: k, Detail: det})
			d.Material = true
		case pv != v:
			det := pv + " -> " + v
			if section == "dirs" {
				det = "contents changed"
			}
			d.Changes = append(d.Changes, Change{Section: section, Kind: "changed", Key: k, Detail: det})
			d.Material = true
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			d.Changes = append(d.Changes, Change{Section: section, Kind: "removed", Key: k})
			d.Material = true
		}
	}
}

// Summary renders the diff as short lines.
func (d Diff) Summary(max int) []string {
	var out []string
	for _, c := range d.Changes {
		line := fmt.Sprintf("%s %s: %s", c.Kind, c.Section, c.Key)
		if c.Detail != "" {
			line += " (" + c.Detail + ")"
		}
		out = append(out, line)
		if max > 0 && len(out) >= max {
			if len(d.Changes) > max {
				out = append(out, fmt.Sprintf("… and %d more", len(d.Changes)-max))
			}
			break
		}
	}
	return out
}
