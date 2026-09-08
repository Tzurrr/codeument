package collect

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Port is a listening socket.
type Port struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// Key identifies the port for diffs.
func (p Port) Key() string { return p.Proto + ":" + p.Address + ":" + strconv.Itoa(p.Port) }

// Ports collects listening TCP/UDP sockets.
func Ports(ctx context.Context, e Env, partial Partial) []Port {
	if runtime.GOOS == "darwin" && e.Root == "/" {
		return darwinPorts(ctx, e)
	}
	out, err := e.Command(ctx, "ss", "-tulnpH")
	if err != nil {
		partial.Add("ports", "ss unavailable: "+err.Error()+" (falling back to /proc/net)")
		return procPorts(e)
	}
	var ports []Port
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		// `ss -H` prints: proto state recv send local peer [users]. Older
		// builds omit the state column, so find the local address by shape
		// rather than by a fixed index.
		proto := f[0]
		local := f[4]
		if !strings.Contains(local, ":") && len(f) > 3 {
			local = f[3]
		}
		addr, portStr := splitHostPort(local)
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		p := Port{Proto: proto, Address: addr, Port: port}
		if i := strings.Index(line, "users:(("); i >= 0 {
			rest := line[i+len("users:(("):]
			parts := strings.Split(rest, ",")
			if len(parts) >= 2 {
				p.Process = strings.Trim(parts[0], "\"")
				if _, pid, ok := strings.Cut(parts[1], "="); ok {
					p.PID, _ = strconv.Atoi(strings.TrimRight(pid, ")"))
				}
			}
		}
		if !seen[p.Key()] {
			seen[p.Key()] = true
			ports = append(ports, p)
		}
	}
	sortPorts(ports)
	return ports
}

func splitHostPort(s string) (string, string) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, ""
	}
	host := strings.Trim(s[:i], "[]")
	if host == "*" || host == "" || host == "0.0.0.0" || host == "::" {
		host = "*"
	}
	return host, s[i+1:]
}

func procPorts(e Env) []Port {
	var ports []Port
	for _, spec := range []struct{ file, proto string }{{"/proc/net/tcp", "tcp"}, {"/proc/net/tcp6", "tcp"}, {"/proc/net/udp", "udp"}, {"/proc/net/udp6", "udp"}} {
		lines, err := e.ReadLines(spec.file)
		if err != nil {
			continue
		}
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) < 4 {
				continue
			}
			if spec.proto == "tcp" && f[3] != "0A" { // LISTEN
				continue
			}
			if spec.proto == "udp" && f[3] != "07" { // UDP unconnected
				continue
			}
			addr, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			host := "*"
			if addr != "00000000" && addr != "00000000000000000000000000000000" {
				host = hexIPv4(addr)
			}
			ports = append(ports, Port{Proto: spec.proto, Address: host, Port: int(port)})
		}
	}
	sortPorts(ports)
	return ports
}

func hexIPv4(h string) string {
	if len(h) != 8 {
		return h
	}
	var parts []string
	for i := 6; i >= 0; i -= 2 {
		n, _ := strconv.ParseInt(h[i:i+2], 16, 32)
		parts = append(parts, strconv.FormatInt(n, 10))
	}
	return strings.Join(parts, ".")
}

func darwinPorts(ctx context.Context, e Env) []Port {
	out, err := e.Command(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-iUDP")
	if err != nil {
		return nil
	}
	var ports []Port
	seen := map[string]bool{}
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 9 {
			continue
		}
		proto := strings.ToLower(f[7])
		addr, portStr := splitHostPort(strings.TrimSuffix(f[8], " (LISTEN)"))
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		p := Port{Proto: proto, Address: addr, Port: port, Process: f[0], PID: pid}
		if !seen[p.Key()] {
			seen[p.Key()] = true
			ports = append(ports, p)
		}
	}
	sortPorts(ports)
	return ports
}

func sortPorts(ports []Port) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return ports[i].Key() < ports[j].Key()
	})
}

// Service is a system service.
type Service struct {
	Name        string `json:"name"`
	Enabled     string `json:"enabled"` // enabled | disabled | static | ...
	Active      string `json:"active"`  // running | exited | failed | inactive
	Description string `json:"description,omitempty"`
}

// Key identifies the service for diffs.
func (s Service) Key() string { return s.Name }

// Services collects enabled and running services.
func Services(ctx context.Context, e Env, partial Partial) []Service {
	if runtime.GOOS == "darwin" && e.Root == "/" {
		return darwinServices(ctx, e)
	}
	byName := map[string]*Service{}
	out, err := e.Command(ctx, "systemctl", "list-unit-files", "--type=service", "--no-legend", "--no-pager", "--plain")
	if err != nil {
		partial.Add("services", "systemctl unavailable: "+err.Error())
		return nil
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		name := strings.TrimSuffix(f[0], ".service")
		if f[1] == "enabled" || f[1] == "enabled-runtime" || f[1] == "static" || f[1] == "indirect" {
			byName[name] = &Service{Name: name, Enabled: f[1], Active: "inactive"}
		}
	}
	out, err = e.Command(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
				continue
			}
			name := strings.TrimSuffix(f[0], ".service")
			active := f[3]
			if f[2] == "failed" {
				active = "failed"
			}
			s, ok := byName[name]
			if !ok {
				if active != "running" && active != "failed" {
					continue
				}
				s = &Service{Name: name, Enabled: "unknown"}
				byName[name] = s
			}
			s.Active = active
			if len(f) > 4 {
				s.Description = strings.Join(f[4:], " ")
			}
		}
	}
	var services []Service
	for _, s := range byName {
		if s.Enabled == "static" && s.Active != "running" && s.Active != "failed" {
			continue
		}
		services = append(services, *s)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services
}

func darwinServices(ctx context.Context, e Env) []Service {
	out, err := e.Command(ctx, "launchctl", "list")
	if err != nil {
		return nil
	}
	var services []Service
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 || strings.HasPrefix(f[2], "com.apple.") {
			continue
		}
		active := "inactive"
		if f[0] != "-" {
			active = "running"
		}
		services = append(services, Service{Name: f[2], Enabled: "loaded", Active: active})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services
}

// CronEntry is a scheduled job.
type CronEntry struct {
	Source   string `json:"source"` // file or "crontab:user" or "timer"
	User     string `json:"user,omitempty"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
}

// Key identifies the entry for diffs.
func (c CronEntry) Key() string { return c.Source + "|" + c.User + "|" + c.Schedule + "|" + c.Command }

// Cron collects cron jobs and systemd timers.
func Cron(ctx context.Context, e Env, partial Partial) []CronEntry {
	var out []CronEntry
	add := func(source, user string, lines []string, hasUser bool) {
		for _, l := range lines {
			if strings.Contains(l, "=") && !strings.HasPrefix(l, "@") && !strings.ContainsAny(l[:1], "0123456789*") {
				continue // VAR=value
			}
			f := strings.Fields(l)
			need := 6
			if hasUser {
				need = 7
			}
			if strings.HasPrefix(l, "@") {
				need = 2
				if hasUser {
					need = 3
				}
			}
			if len(f) < need {
				continue
			}
			ent := CronEntry{Source: source, User: user}
			if strings.HasPrefix(l, "@") {
				ent.Schedule = f[0]
				rest := f[1:]
				if hasUser {
					ent.User, rest = f[1], f[2:]
				}
				ent.Command = strings.Join(rest, " ")
			} else {
				ent.Schedule = strings.Join(f[:5], " ")
				rest := f[5:]
				if hasUser {
					ent.User, rest = f[5], f[6:]
				}
				ent.Command = strings.Join(rest, " ")
			}
			out = append(out, ent)
		}
	}
	if lines, err := e.ReadLines("/etc/crontab"); err == nil {
		add("/etc/crontab", "", lines, true)
	}
	for _, f := range e.Glob("/etc/cron.d/*") {
		if lines, err := e.ReadLines(f); err == nil {
			add(f, "", lines, true)
		}
	}
	for _, dir := range []string{"/var/spool/cron/crontabs", "/var/spool/cron"} {
		for _, f := range e.Glob(dir + "/*") {
			lines, err := e.ReadLines(f)
			if err != nil {
				partial.Add("cron", filepath.Base(f)+": "+err.Error())
				continue
			}
			add("crontab", filepath.Base(f), lines, false)
		}
	}
	for _, period := range []string{"hourly", "daily", "weekly", "monthly"} {
		for _, f := range e.Glob("/etc/cron." + period + "/*") {
			out = append(out, CronEntry{Source: "/etc/cron." + period, Schedule: "@" + period, Command: filepath.Base(f)})
		}
	}
	if o, err := e.Command(ctx, "systemctl", "list-timers", "--all", "--no-legend", "--no-pager", "--output=json"); err == nil {
		var timers []struct {
			Unit      string `json:"unit"`
			Activates string `json:"activates"`
			Next      string `json:"next"`
		}
		if json.Unmarshal([]byte(o), &timers) == nil {
			for _, t := range timers {
				out = append(out, CronEntry{Source: "timer", Schedule: t.Unit, Command: t.Activates})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Container is a running or stopped container.
type Container struct {
	Runtime string `json:"runtime"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	Ports   string `json:"ports,omitempty"`
	Project string `json:"project,omitempty"`
}

// Key identifies the container for diffs.
func (c Container) Key() string { return c.Runtime + ":" + c.Name }

// Containers collects docker/podman containers.
func Containers(ctx context.Context, e Env, partial Partial) []Container {
	var out []Container
	for _, rt := range []string{"docker", "podman"} {
		o, err := e.Command(ctx, rt, "ps", "-a", "--format", "{{json .}}")
		if err != nil {
			if rt == "docker" && e.Exists("/var/run/docker.sock") {
				partial.Add("containers", "docker socket present but docker ps failed: "+err.Error())
			}
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(o), "\n") {
			if line == "" {
				continue
			}
			var c struct {
				Names, Image, Status, Ports, Labels string
			}
			if json.Unmarshal([]byte(line), &c) != nil {
				continue
			}
			ct := Container{Runtime: rt, Name: c.Names, Image: c.Image, Status: c.Status, Ports: c.Ports}
			for _, l := range strings.Split(c.Labels, ",") {
				if k, v, ok := strings.Cut(l, "="); ok && k == "com.docker.compose.project" {
					ct.Project = v
				}
			}
			out = append(out, ct)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}
