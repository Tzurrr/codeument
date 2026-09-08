package collect

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Account is a login-capable user. It never carries a password hash; the
// credential column is filled in by the policy at render time.
type Account struct {
	Username       string   `json:"username"`
	UID            int      `json:"uid"`
	GID            int      `json:"gid"`
	Home           string   `json:"home"`
	Shell          string   `json:"shell"`
	Groups         []string `json:"groups"`
	Sudo           bool     `json:"sudo"`
	SudoSource     string   `json:"sudo_source,omitempty"`
	LastLogin      string   `json:"last_login,omitempty"`
	AuthorizedKeys int      `json:"authorized_keys"`
	KeyComments    []string `json:"key_comments,omitempty"`
	System         bool     `json:"system"`
}

// Key identifies the account for diffs.
func (a Account) Key() string { return a.Username }

var noLoginShells = map[string]bool{"/usr/sbin/nologin": true, "/sbin/nologin": true, "/bin/false": true, "/usr/bin/false": true, "/bin/sync": true, "/usr/bin/nologin": true}

// Accounts collects users with a login shell, their groups, sudo rights,
// last login and authorized key counts. /etc/shadow is never read.
func Accounts(ctx context.Context, e Env, partial Partial) []Account {
	lines, err := e.ReadLines("/etc/passwd")
	if err != nil {
		partial.Add("accounts", "passwd unreadable: "+err.Error())
		return nil
	}
	groupsOf := map[string][]string{}
	gidName := map[int]string{}
	if glines, err := e.ReadLines("/etc/group"); err == nil {
		for _, l := range glines {
			f := strings.Split(l, ":")
			if len(f) < 4 {
				continue
			}
			gid, _ := strconv.Atoi(f[2])
			gidName[gid] = f[0]
			for _, m := range strings.Split(f[3], ",") {
				if m = strings.TrimSpace(m); m != "" {
					groupsOf[m] = append(groupsOf[m], f[0])
				}
			}
		}
	}
	sudoUsers, sudoGroups := sudoers(e, partial)
	lastLogins := lastLogin(ctx, e)

	var out []Account
	for _, l := range lines {
		f := strings.Split(l, ":")
		if len(f) < 7 {
			continue
		}
		uid, _ := strconv.Atoi(f[2])
		gid, _ := strconv.Atoi(f[3])
		a := Account{Username: f[0], UID: uid, GID: gid, Home: f[5], Shell: f[6], System: uid < 1000 && uid != 0}
		if noLoginShells[a.Shell] || a.Shell == "" {
			continue
		}
		a.Groups = append([]string{}, groupsOf[a.Username]...)
		if g, ok := gidName[gid]; ok && !contains(a.Groups, g) {
			a.Groups = append([]string{g}, a.Groups...)
		}
		sort.Strings(a.Groups)
		if uid == 0 {
			a.Sudo, a.SudoSource = true, "root"
		} else if src, ok := sudoUsers[a.Username]; ok {
			a.Sudo, a.SudoSource = true, src
		} else {
			for _, g := range a.Groups {
				if src, ok := sudoGroups[g]; ok {
					a.Sudo, a.SudoSource = true, src+" (%"+g+")"
					break
				}
			}
		}
		a.LastLogin = lastLogins[a.Username]
		a.AuthorizedKeys, a.KeyComments = authorizedKeys(e, a.Home)
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UID == 0 || out[j].UID == 0 {
			return out[i].UID == 0
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// sudoers parses /etc/sudoers and sudoers.d for user and %group rules.
func sudoers(e Env, partial Partial) (users, groups map[string]string) {
	users, groups = map[string]string{}, map[string]string{}
	files := append([]string{"/etc/sudoers"}, e.Glob("/etc/sudoers.d/*")...)
	for _, f := range files {
		lines, err := e.ReadLines(f)
		if err != nil {
			if f == "/etc/sudoers" {
				partial.Add("accounts", "sudoers unreadable (run as root for sudo membership)")
			}
			continue
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "Defaults") || strings.HasPrefix(l, "#include") || strings.HasPrefix(l, "@include") {
				continue
			}
			fields := strings.Fields(l)
			if len(fields) < 2 || !strings.Contains(l, "=") {
				continue
			}
			subject := fields[0]
			switch {
			case strings.HasPrefix(subject, "%"):
				groups[strings.TrimPrefix(subject, "%")] = f
			case strings.HasPrefix(subject, "User_Alias") || strings.HasPrefix(subject, "Host_Alias") || strings.HasPrefix(subject, "Cmnd_Alias") || strings.HasPrefix(subject, "Runas_Alias"):
			default:
				users[subject] = f
			}
		}
	}
	for _, g := range []string{"sudo", "wheel", "admin"} {
		if _, ok := groups[g]; !ok && e.Exists("/etc/sudoers") {
			continue
		}
	}
	return users, groups
}

func lastLogin(ctx context.Context, e Env) map[string]string {
	out := map[string]string{}
	o, err := e.Command(ctx, "last", "-w", "-n", "300", "--time-format", "iso")
	if err != nil {
		o, err = e.Command(ctx, "last", "-n", "300")
		if err != nil {
			return out
		}
	}
	for _, line := range strings.Split(o, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] == "reboot" || f[0] == "wtmp" || f[0] == "shutdown" {
			continue
		}
		if _, ok := out[f[0]]; ok {
			continue
		}
		// Find the first field that looks like a date.
		for _, x := range f[2:] {
			if len(x) >= 10 && x[4] == '-' && x[7] == '-' {
				out[f[0]] = x[:10]
				break
			}
		}
		if _, ok := out[f[0]]; !ok && len(f) >= 7 {
			out[f[0]] = strings.Join(f[3:6], " ")
		}
	}
	return out
}

func authorizedKeys(e Env, home string) (int, []string) {
	if home == "" {
		return 0, nil
	}
	lines, err := e.ReadLines(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		return 0, nil
	}
	var comments []string
	n := 0
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n++
		if len(f) >= 3 {
			comments = append(comments, f[len(f)-1])
		}
	}
	return n, comments
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
