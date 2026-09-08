package collect

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OSInfo describes the operating system.
type OSInfo struct {
	Hostname       string `json:"hostname"`
	MachineID      string `json:"machine_id"`
	Distro         string `json:"distro"`
	DistroVersion  string `json:"distro_version"`
	Kernel         string `json:"kernel"`
	Arch           string `json:"arch"`
	Uptime         string `json:"uptime"`
	BootTime       string `json:"boot_time,omitempty"`
	Timezone       string `json:"timezone"`
	Virtualization string `json:"virtualization,omitempty"`
	Product        string `json:"product,omitempty"`
	Cloud          string `json:"cloud,omitempty"`
}

// OS collects OSInfo.
func OS(ctx context.Context, e Env, partial Partial) OSInfo {
	var info OSInfo
	info.Hostname, _ = os.Hostname()
	if data, err := e.ReadFile("/etc/hostname"); err == nil && strings.TrimSpace(string(data)) != "" && e.Root != "/" {
		info.Hostname = strings.TrimSpace(string(data))
	}
	info.MachineID = MachineID(e)
	info.Arch = runtime.GOARCH
	if lines, err := e.ReadLines("/etc/os-release"); err == nil {
		kv := parseKV(lines)
		info.Distro = firstNonEmpty(kv["PRETTY_NAME"], kv["NAME"])
		info.DistroVersion = firstNonEmpty(kv["VERSION_ID"], kv["VERSION"])
	} else if runtime.GOOS == "darwin" {
		if out, err := e.Command(ctx, "sw_vers", "-productVersion"); err == nil {
			info.Distro, info.DistroVersion = "macOS", strings.TrimSpace(out)
		}
	} else {
		partial.Add("os", "os-release unreadable: "+err.Error())
	}
	if out, err := e.Command(ctx, "uname", "-srm"); err == nil {
		info.Kernel = strings.TrimSpace(out)
	}
	if data, err := e.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(data)); len(f) > 0 {
			if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
				info.Uptime = humanUptime(time.Duration(secs) * time.Second)
				info.BootTime = e.Now().Add(-time.Duration(secs) * time.Second).Format("2006-01-02 15:04")
			}
		}
	} else if out, err := e.Command(ctx, "uptime"); err == nil {
		info.Uptime = strings.TrimSpace(out)
	}
	if data, err := e.ReadFile("/etc/timezone"); err == nil {
		info.Timezone = strings.TrimSpace(string(data))
	} else if tz, err := os.Readlink(e.Path("/etc/localtime")); err == nil {
		if i := strings.Index(tz, "zoneinfo/"); i >= 0 {
			info.Timezone = tz[i+len("zoneinfo/"):]
		}
	}
	if info.Timezone == "" {
		info.Timezone, _ = e.Now().Zone()
	}
	if out, err := e.Command(ctx, "systemd-detect-virt"); err == nil {
		if v := strings.TrimSpace(out); v != "" && v != "none" {
			info.Virtualization = v
		}
	}
	if data, err := e.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		info.Product = strings.TrimSpace(string(data))
	}
	if data, err := e.ReadFile("/sys/class/dmi/id/sys_vendor"); err == nil {
		vendor := strings.ToLower(strings.TrimSpace(string(data)))
		switch {
		case strings.Contains(vendor, "amazon"):
			info.Cloud = "aws"
		case strings.Contains(vendor, "google"):
			info.Cloud = "gcp"
		case strings.Contains(vendor, "microsoft"):
			info.Cloud = "azure"
		case strings.Contains(vendor, "digitalocean"):
			info.Cloud = "digitalocean"
		case strings.Contains(vendor, "hetzner"):
			info.Cloud = "hetzner"
		}
	}
	return info
}

// MachineID returns a stable identifier for the machine.
func MachineID(e Env) string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if data, err := e.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := e.Command(context.Background(), "ioreg", "-rd1", "-c", "IOPlatformExpertDevice"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "IOPlatformUUID") {
					if i := strings.LastIndex(line, "\""); i > 0 {
						start := strings.LastIndex(line[:i], "\"")
						if start >= 0 {
							return strings.ToLower(line[start+1 : i])
						}
					}
				}
			}
		}
	}
	h, _ := os.Hostname()
	return "host:" + h
}

// Resources is CPU, memory, load and disks.
type Resources struct {
	CPUs        int     `json:"cpus"`
	CPUModel    string  `json:"cpu_model,omitempty"`
	CPUPercent  float64 `json:"cpu_percent"`
	Load1       float64 `json:"load_1"`
	Load5       float64 `json:"load_5"`
	Load15      float64 `json:"load_15"`
	MemTotalMB  int64   `json:"mem_total_mb"`
	MemUsedMB   int64   `json:"mem_used_mb"`
	MemPercent  float64 `json:"mem_percent"`
	SwapTotalMB int64   `json:"swap_total_mb"`
	SwapUsedMB  int64   `json:"swap_used_mb"`
	Disks       []Disk  `json:"disks"`
}

// Disk is one mounted filesystem.
type Disk struct {
	Mount   string  `json:"mount"`
	Device  string  `json:"device"`
	FSType  string  `json:"fstype"`
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
	Percent float64 `json:"percent"`
	Options string  `json:"options,omitempty"`
}

// ResourcesInfo collects Resources.
func ResourcesInfo(ctx context.Context, e Env, partial Partial) Resources {
	var r Resources
	r.CPUs = runtime.NumCPU()
	if lines, err := e.ReadLines("/proc/cpuinfo"); err == nil {
		n := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "processor") {
				n++
			}
			if r.CPUModel == "" && strings.HasPrefix(l, "model name") {
				if _, v, ok := strings.Cut(l, ":"); ok {
					r.CPUModel = strings.TrimSpace(v)
				}
			}
		}
		if n > 0 {
			r.CPUs = n
		}
	}
	if data, err := e.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(data))
		if len(f) >= 3 {
			r.Load1, _ = strconv.ParseFloat(f[0], 64)
			r.Load5, _ = strconv.ParseFloat(f[1], 64)
			r.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	if lines, err := e.ReadLines("/proc/meminfo"); err == nil {
		kb := map[string]int64{}
		for _, l := range lines {
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			f := strings.Fields(v)
			if len(f) == 0 {
				continue
			}
			n, _ := strconv.ParseInt(f[0], 10, 64)
			kb[k] = n
		}
		r.MemTotalMB = kb["MemTotal"] / 1024
		avail := kb["MemAvailable"]
		if avail == 0 {
			avail = kb["MemFree"] + kb["Buffers"] + kb["Cached"]
		}
		r.MemUsedMB = (kb["MemTotal"] - avail) / 1024
		if r.MemTotalMB > 0 {
			r.MemPercent = round1(float64(r.MemUsedMB) / float64(r.MemTotalMB) * 100)
		}
		r.SwapTotalMB = kb["SwapTotal"] / 1024
		r.SwapUsedMB = (kb["SwapTotal"] - kb["SwapFree"]) / 1024
	} else if runtime.GOOS == "darwin" {
		if out, err := e.Command(ctx, "sysctl", "-n", "hw.memsize"); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
				r.MemTotalMB = n / 1024 / 1024
			}
		}
	} else {
		partial.Add("resources", "meminfo unreadable")
	}
	r.CPUPercent = cpuSample(e)
	r.Disks = disks(e, partial)
	return r
}

// cpuSample measures CPU usage over a short interval from /proc/stat.
func cpuSample(e Env) float64 {
	read := func() (idle, total float64, ok bool) {
		data, err := e.ReadFile("/proc/stat")
		if err != nil {
			return 0, 0, false
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			f := strings.Fields(line)[1:]
			for i, v := range f {
				n, _ := strconv.ParseFloat(v, 64)
				total += n
				if i == 3 || i == 4 {
					idle += n
				}
			}
			return idle, total, true
		}
		return 0, 0, false
	}
	i1, t1, ok := read()
	if !ok {
		return 0
	}
	if e.Root != "/" {
		if t1 == 0 {
			return 0
		}
		return round1((1 - i1/t1) * 100)
	}
	time.Sleep(500 * time.Millisecond)
	i2, t2, ok := read()
	if !ok || t2 == t1 {
		return 0
	}
	return round1((1 - (i2-i1)/(t2-t1)) * 100)
}

var pseudoFS = map[string]bool{"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "cgroup": true, "cgroup2": true, "securityfs": true, "pstore": true, "debugfs": true, "tracefs": true, "configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true, "bpf": true, "autofs": true, "binfmt_misc": true, "rpc_pipefs": true, "nsfs": true, "overlay": false, "squashfs": true, "ramfs": true, "efivarfs": true}

func disks(e Env, partial Partial) []Disk {
	lines, err := e.ReadLines("/proc/mounts")
	if err != nil {
		if runtime.GOOS == "darwin" {
			return darwinDisks(e)
		}
		partial.Add("resources", "mounts unreadable")
		return nil
	}
	seen := map[string]bool{}
	var out []Disk
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 4 || pseudoFS[f[2]] || seen[f[1]] {
			continue
		}
		if strings.HasPrefix(f[1], "/snap/") || strings.HasPrefix(f[1], "/proc") || strings.HasPrefix(f[1], "/sys") || strings.HasPrefix(f[1], "/dev") || strings.HasPrefix(f[1], "/run") {
			continue
		}
		seen[f[1]] = true
		d := Disk{Mount: f[1], Device: f[0], FSType: f[2], Options: f[3]}
		if e.Root == "/" {
			var st syscall.Statfs_t
			if err := syscall.Statfs(f[1], &st); err == nil && st.Blocks > 0 {
				total := float64(st.Blocks) * float64(st.Bsize)
				free := float64(st.Bavail) * float64(st.Bsize)
				d.TotalGB = round1(total / 1e9)
				d.UsedGB = round1((total - free) / 1e9)
				d.Percent = round1((total - free) / total * 100)
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func darwinDisks(e Env) []Disk {
	out, err := e.Command(context.Background(), "df", "-k")
	if err != nil {
		return nil
	}
	var disks []Disk
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 9 || !strings.HasPrefix(f[0], "/dev/") {
			continue
		}
		total, _ := strconv.ParseFloat(f[1], 64)
		used, _ := strconv.ParseFloat(f[2], 64)
		d := Disk{Device: f[0], Mount: f[len(f)-1], FSType: "apfs", TotalGB: round1(total / 1e6), UsedGB: round1(used / 1e6)}
		if total > 0 {
			d.Percent = round1(used / total * 100)
		}
		disks = append(disks, d)
	}
	return disks
}

func parseKV(lines []string) map[string]string {
	out := map[string]string{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func humanUptime(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
