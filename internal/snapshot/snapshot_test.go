package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tzurrr/codeument/internal/explain"
	llmfake "github.com/Tzurrr/codeument/internal/llm/fake"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/snapshot/collect"
)

// fixture builds a fake root filesystem.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"etc/hostname":                    "web-01\n",
		"etc/machine-id":                  "abcdef0123456789\n",
		"etc/os-release":                  "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nVERSION_ID=\"24.04\"\n",
		"etc/timezone":                    "Europe/Berlin\n",
		"proc/uptime":                     "864000.12 100.0\n",
		"proc/loadavg":                    "0.52 0.40 0.30 1/200 12345\n",
		"proc/meminfo":                    "MemTotal:       16384000 kB\nMemFree:         4000000 kB\nMemAvailable:    8192000 kB\nBuffers: 100 kB\nCached: 200 kB\nSwapTotal:       2048000 kB\nSwapFree:        2048000 kB\n",
		"proc/cpuinfo":                    "processor\t: 0\nmodel name\t: Fake CPU 3.0GHz\nprocessor\t: 1\nmodel name\t: Fake CPU 3.0GHz\n",
		"proc/stat":                       "cpu  100 0 100 800 0 0 0 0 0 0\n",
		"proc/mounts":                     "/dev/sda1 / ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n/dev/sdb1 /data xfs rw 0 0\n",
		"etc/passwd":                      "root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\nalice:x:1000:1000:Alice:/home/alice:/bin/zsh\ndeploy:x:1001:1001::/home/deploy:/bin/bash\n",
		"etc/group":                       "root:x:0:\nsudo:x:27:alice\ndocker:x:999:alice,deploy\nalice:x:1000:\ndeploy:x:1001:\n",
		"etc/sudoers":                     "Defaults env_reset\nroot ALL=(ALL:ALL) ALL\n%sudo ALL=(ALL:ALL) ALL\n",
		"etc/sudoers.d/deploy":            "deploy ALL=(ALL) NOPASSWD: /bin/systemctl restart app\n",
		"etc/shadow":                      "root:$6$SHOULDNEVERBEREAD:19000:0:99999:7:::\n",
		"etc/crontab":                     "SHELL=/bin/sh\n17 *\t* * *\troot    cd / && run-parts --report /etc/cron.hourly\n",
		"etc/cron.d/backup":               "0 3 * * * root /usr/local/bin/backup.sh --full\n",
		"var/spool/cron/crontabs/deploy":  "@daily /home/deploy/app/cleanup.sh\n",
		"home/alice/.ssh/authorized_keys": "ssh-ed25519 AAAAC3Nza alice@laptop\nssh-rsa AAAAB3Nza alice@desktop\n",
		"opt/app/docker-compose.yml":      "services:\n  web:\n    image: acme/web:1.2\n    environment:\n      DB_PASSWORD: supersecret\n",
		"opt/app/README.md":               "# App\nThe customer portal.\n",
		"opt/app/data/blob.bin":           "\x00\x01\x02",
		"etc/nginx/nginx.conf":            "user www-data;\nworker_processes auto;\n",
		"etc/nginx/sites-enabled/app":     "server { listen 80; }\n",
		"home/deploy/app/Makefile":        "run:\n\tgo run .\n",
	}
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fakeRun(ctx context.Context, name string, args ...string) (string, error) {
	switch name {
	case "uname":
		return "Linux 6.8.0-45-generic x86_64\n", nil
	case "ss":
		return "tcp   LISTEN 0      511          0.0.0.0:80        0.0.0.0:*    users:((\"nginx\",pid=1234,fd=6))\ntcp   LISTEN 0      128             [::]:22           [::]:*    users:((\"sshd\",pid=800,fd=3))\nudp   UNCONN 0      0          127.0.0.53%lo:53        0.0.0.0:*    users:((\"systemd-resolve\",pid=500,fd=13))\n", nil
	case "systemctl":
		if len(args) > 0 && args[0] == "list-unit-files" {
			return "nginx.service enabled enabled\nssh.service enabled enabled\napp.service enabled enabled\ncups.service disabled disabled\n", nil
		}
		if len(args) > 0 && args[0] == "list-units" {
			return "nginx.service loaded active running A high performance web server\nssh.service loaded active running OpenBSD Secure Shell server\napp.service loaded failed failed Customer portal\n", nil
		}
		return "[]", nil
	case "docker":
		return `{"Names":"app-web-1","Image":"acme/web:1.2","Status":"Up 3 days","Ports":"0.0.0.0:8080->80/tcp","Labels":"com.docker.compose.project=app"}` + "\n", nil
	case "last":
		return "alice    pts/0        10.0.0.5         2026-09-07T09:12:00+0000   still logged in\ndeploy   pts/1        10.0.0.9         2026-08-30T22:01:00+0000 - 2026-08-30T22:30:00+0000  (00:29)\nreboot   system boot  6.8.0            2026-08-01T00:00:00+0000   still running\n", nil
	}
	return "", errors.New(name + ": not installed")
}

func testEnv(root string) collect.Env {
	return collect.Env{Root: root, Run: fakeRun, Now: func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }}
}

func TestCollectFromFixture(t *testing.T) {
	root := fixture(t)
	s := Collect(context.Background(), Options{Env: testEnv(root)})
	if s.Hostname != "web-01" || s.MachineID != "abcdef0123456789" {
		t.Fatalf("identity: %+v", s.OS)
	}
	if s.OS.Distro != "Ubuntu 24.04.1 LTS" || s.OS.Kernel != "Linux 6.8.0-45-generic x86_64" || s.OS.Uptime != "10d 0h" || s.OS.Timezone != "Europe/Berlin" {
		t.Fatalf("os: %+v", s.OS)
	}
	if s.Resources.CPUs != 2 || s.Resources.MemTotalMB != 16000 || s.Resources.MemUsedMB != 8000 || len(s.Resources.Disks) != 2 {
		t.Fatalf("resources: %+v", s.Resources)
	}
	if len(s.Ports) != 3 || s.Ports[0].Port != 22 || s.Ports[2].Port != 80 || s.Ports[2].Process != "nginx" || s.Ports[2].PID != 1234 {
		t.Fatalf("ports: %+v", s.Ports)
	}
	byName := map[string]collect.Service{}
	for _, x := range s.Services {
		byName[x.Name] = x
	}
	if byName["nginx"].Active != "running" || byName["app"].Active != "failed" || byName["cups"].Name != "" {
		t.Fatalf("services: %+v", s.Services)
	}
	if len(s.Cron) != 3 {
		t.Fatalf("cron: %+v", s.Cron)
	}
	if len(s.Containers) != 1 || s.Containers[0].Project != "app" {
		t.Fatalf("containers: %+v", s.Containers)
	}
	users := map[string]collect.Account{}
	for _, a := range s.Accounts {
		users[a.Username] = a
	}
	if len(s.Accounts) != 3 || s.Accounts[0].Username != "root" {
		t.Fatalf("accounts: %+v", s.Accounts)
	}
	if !users["alice"].Sudo || !strings.Contains(users["alice"].SudoSource, "%sudo") || users["alice"].AuthorizedKeys != 2 || users["alice"].LastLogin != "2026-09-07" {
		t.Fatalf("alice: %+v", users["alice"])
	}
	if !users["deploy"].Sudo || !strings.HasSuffix(users["deploy"].SudoSource, "sudoers.d/deploy") || users["deploy"].LastLogin != "2026-08-30" {
		t.Fatalf("deploy: %+v", users["deploy"])
	}
	paths := map[string]Dir{}
	for _, d := range s.Dirs {
		paths[d.Path] = d
	}
	if _, ok := paths["/opt/app"]; !ok {
		t.Fatalf("expected /opt/app in dirs: %v", keys(paths))
	}
	if _, ok := paths["/home/deploy/app"]; !ok {
		t.Fatalf("expected /home/deploy/app in dirs: %v", keys(paths))
	}
	app := paths["/opt/app"]
	heads := ""
	for _, h := range app.Heads {
		heads += h.Name + ":" + h.Content + "\n"
	}
	if !strings.Contains(heads, "The customer portal") || strings.Contains(heads, "supersecret") || !strings.Contains(heads, "<redacted:") {
		t.Fatalf("heads not redacted:\n%s", heads)
	}
	// The whole snapshot must never contain the shadow hash.
	raw, _ := s.Marshal()
	if strings.Contains(string(raw), "SHOULDNEVERBEREAD") || strings.Contains(string(raw), "supersecret") {
		t.Fatal("snapshot leaked a secret")
	}
	if _, err := testEnv(root).ReadFile("/etc/shadow"); !errors.Is(err, collect.ErrDenied) {
		t.Fatalf("shadow must be denied, got %v", err)
	}
}

func keys(m map[string]Dir) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

type memCache struct{ m map[string][2]any }

func (c *memCache) Get(_ context.Context, path string) (string, *explain.Explanation, bool) {
	v, ok := c.m[path]
	if !ok {
		return "", nil, false
	}
	e := v[1].(explain.Explanation)
	return v[0].(string), &e, true
}
func (c *memCache) Put(_ context.Context, path, hash string, e explain.Explanation) error {
	c.m[path] = [2]any{hash, e}
	return nil
}

type fakeExplainer struct{ calls int }

func (f *fakeExplainer) Explain(ctx context.Context, in explain.Input) ([]explain.Explanation, error) {
	f.calls++
	return explain.Run(ctx, llmfake.New(), in)
}

func TestExplainCacheDiffAndRender(t *testing.T) {
	root := fixture(t)
	ctx := context.Background()
	s := Collect(ctx, Options{Env: testEnv(root)})
	cache := &memCache{m: map[string][2]any{}}
	ex := &fakeExplainer{}
	n, err := Explain(ctx, s, ex, cache, nil)
	if err != nil || n != len(s.Dirs) || ex.calls != 1 {
		t.Fatalf("explain: %v n=%d calls=%d", err, n, ex.calls)
	}
	for _, d := range s.Dirs {
		if d.Explanation == nil || d.Explanation.Path != d.Path {
			t.Fatalf("missing explanation for %s", d.Path)
		}
	}
	// Second run with unchanged content uses the cache only.
	s2 := Collect(ctx, Options{Env: testEnv(root)})
	n, err = Explain(ctx, s2, ex, cache, nil)
	if err != nil || n != 0 || ex.calls != 1 {
		t.Fatalf("cache not used: %v n=%d calls=%d", err, n, ex.calls)
	}

	d := Compare(nil, s)
	if !d.Material || len(d.Changes) != 1 {
		t.Fatalf("first diff: %+v", d)
	}
	d = Compare(s, s2)
	if d.Material || len(d.Changes) != 0 {
		t.Fatalf("identical snapshots must not differ: %+v", d.Changes)
	}
	// Change things: new port, service failed->running, new user, dir content.
	s2.Ports = append(s2.Ports, collect.Port{Proto: "tcp", Address: "*", Port: 5432, Process: "postgres"})
	for i := range s2.Services {
		if s2.Services[i].Name == "app" {
			s2.Services[i].Active = "running"
		}
	}
	s2.Accounts = append(s2.Accounts, collect.Account{Username: "bob", UID: 1002, Shell: "/bin/bash"})
	s2.Dirs[0].ContentHash = "changed"
	d = Compare(s, s2)
	if !d.Material || len(d.Changes) != 4 {
		t.Fatalf("diff: %+v", d.Summary(0))
	}
	// Disk creeping by 3 points is reported but not material.
	s3 := Collect(ctx, Options{Env: testEnv(root)})
	s3.Resources.Disks[0].Percent += 12
	d = Compare(s, s3)
	if d.Material || len(d.Changes) != 1 {
		t.Fatalf("disk noise: %+v material=%v", d.Summary(0), d.Material)
	}

	pol := secrets.Policy{Mode: secrets.ModeReference, References: map[string]string{"default": "vault://infra/{hostname}/{username}"}}
	md := Render(s, RenderOptions{
		Credential: func(a collect.Account) secrets.Reference { return pol.ReferenceOnly(s.Hostname, a.Username) },
		History:    []HistoryEntry{{At: time.Now(), Changes: []string{"added ports: tcp:*:5432"}}},
	})
	for _, must := range []string{"## Overview", "Ubuntu 24.04.1 LTS", "## Directories", "### `/opt/app`", "## Services", "**failed**", "## Listening ports", "| 80 | tcp |", "## Scheduled jobs", "backup.sh", "## Containers", "app-web-1", "## Accounts and access", "`vault://infra/web-01/alice`", "alice@laptop", "## Recent changes"} {
		if !strings.Contains(md, must) {
			t.Errorf("rendered page missing %q", must)
		}
	}
	if regexp.MustCompile(`\$6\$`).MatchString(md) || strings.Contains(md, "supersecret") {
		t.Fatal("rendered page leaked a secret")
	}
	inline := Render(s, RenderOptions{Credential: func(a collect.Account) secrets.Reference {
		if a.Username == "root" {
			return secrets.Reference{Mode: secrets.ModeInline, Password: "Pl4in!"}
		}
		return secrets.Reference{Mode: secrets.ModeInline}
	}})
	if !strings.Contains(inline, "`Pl4in!`") || !strings.Contains(inline, "_not recorded_") {
		t.Fatalf("inline rendering:\n%s", inline)
	}
	manager := credentialCell(secrets.Reference{Mode: secrets.ModeManager, Ref: "op://Infra/web-01 root", URL: "https://op/x"}, "1Password")
	if manager != "[1Password: op://Infra/web-01 root](https://op/x)" {
		t.Fatalf("manager cell = %q", manager)
	}
}
