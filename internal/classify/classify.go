// Package classify decides how much a command matters: noise or meaningful,
// its family, a weight for batch scoring, and whether it is a milestone that
// should trigger a summary early. It also extracts file paths an editor or
// redirection touched.
package classify

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Tzurrr/codeument/internal/model"
)

// Result is the classification of one command line.
type Result struct {
	Kind         model.Kind
	Family       string
	Weight       int
	Milestone    bool
	FilesTouched []string
}

// Options extend the defaults from config.
type Options struct {
	Ignore   []string       // extra noise programs
	Unignore []string       // default noise programs to count anyway
	Weights  map[string]int // prefix -> weight override (weight >= 5 marks a milestone)
}

// Classifier applies the rules.
type Classifier struct {
	noise   map[string]bool
	weights []rule
}

type rule struct {
	prefix    string
	family    string
	weight    int
	milestone bool
	words     int
}

// New builds a classifier with the defaults plus opts.
func New(opts Options) *Classifier {
	c := &Classifier{noise: map[string]bool{}}
	for _, n := range defaultNoise {
		c.noise[n] = true
	}
	for _, n := range opts.Ignore {
		c.noise[strings.TrimSpace(n)] = true
	}
	for _, n := range opts.Unignore {
		delete(c.noise, strings.TrimSpace(n))
	}
	// User overrides first so they win on ties; longer prefixes first.
	for p, w := range opts.Weights {
		c.weights = append(c.weights, rule{prefix: p, family: firstWord(p), weight: w, milestone: w >= 5, words: len(strings.Fields(p))})
	}
	sort.SliceStable(c.weights, func(i, j int) bool { return c.weights[i].words > c.weights[j].words })
	c.weights = append(c.weights, defaultRules...)
	return c
}

// Default is a classifier with built-in rules only.
var Default = New(Options{})

var defaultNoise = []string{
	"ls", "ll", "la", "l", "cd", "pwd", "cat", "less", "more", "head", "tail", "clear", "history", "echo", "printf",
	"which", "type", "man", "help", "exit", "fg", "bg", "jobs", "top", "htop", "btop", "watch", "true", "false",
	"pushd", "popd", "dirs", "tree", "file", "stat", "du", "df", "date", "cal", "whoami", "id", "uname", "hostname",
	"alias", "unalias", "export", "source", ".", "bat", "exa", "eza", "fd", "z", "j", "wc", "sleep", "reset", "tput",
	"codeument", "tmux", "screen", "ps", "free", "uptime", "w", "who", "last", "env", "printenv", "set", "unset",
	"fc", "bind", "logout", "read", "test", "[", "ping", "dig", "nslookup", "host", "ip", "ifconfig", "netstat", "ss",
	"lsof", "ncdu", "less", "vimdiff", "diff", "cmp", "md5sum", "sha256sum", "base64", "xxd", "hexdump", "strings",
	"awk", "sed", "grep", "egrep", "fgrep", "rg", "ag", "ack", "sort", "uniq", "cut", "tr", "column", "jq", "yq",
	"xargs", "tee", "nl", "rev", "paste", "join", "comm", "seq", "yes", "basename", "dirname", "realpath", "readlink",
}

// Rules are matched by prefix against the normalised segment (program plus
// arguments). Longer prefixes are listed before shorter ones for the same
// program.
var defaultRules = []rule{
	// Milestones (weight 5).
	r("git push", "git", 5, true), r("git commit", "git", 5, true), r("git merge", "git", 5, true), r("git tag", "git", 5, true),
	r("git rebase", "git", 4, false), r("git revert", "git", 5, true), r("git cherry-pick", "git", 4, false),
	r("terraform apply", "terraform", 5, true), r("terraform destroy", "terraform", 5, true), r("tofu apply", "terraform", 5, true),
	r("ansible-playbook", "ansible", 5, true), r("kubectl apply", "kubernetes", 5, true), r("kubectl rollout", "kubernetes", 5, true),
	r("kubectl delete", "kubernetes", 5, true), r("kubectl scale", "kubernetes", 4, false), r("kubectl edit", "kubernetes", 4, false),
	r("kubectl set", "kubernetes", 4, false), r("kubectl patch", "kubernetes", 4, false), r("kubectl exec", "kubernetes", 2, false),
	r("helm install", "helm", 5, true), r("helm upgrade", "helm", 5, true), r("helm uninstall", "helm", 5, true), r("helm rollback", "helm", 5, true),
	r("docker compose up", "docker", 5, true), r("docker-compose up", "docker", 5, true), r("docker compose down", "docker", 4, false),
	r("docker compose restart", "docker", 4, false), r("docker compose pull", "docker", 3, false), r("docker stack deploy", "docker", 5, true),
	r("docker service update", "docker", 5, true), r("systemctl restart", "service", 5, true), r("systemctl reload", "service", 4, false),
	r("service", "service", 4, false), r("certbot", "tls", 5, true), r("fly deploy", "deploy", 5, true), r("gcloud run deploy", "deploy", 5, true),
	r("gcloud app deploy", "deploy", 5, true), r("aws lambda update-function-code", "deploy", 5, true), r("aws ecs update-service", "deploy", 5, true),
	r("aws cloudformation deploy", "deploy", 5, true), r("sam deploy", "deploy", 5, true), r("cdk deploy", "deploy", 5, true),
	r("pulumi up", "deploy", 5, true), r("cap", "deploy", 5, true), r("vercel --prod", "deploy", 5, true), r("netlify deploy", "deploy", 5, true),
	r("wrangler deploy", "deploy", 5, true), r("wrangler publish", "deploy", 5, true), r("az webapp deploy", "deploy", 5, true),
	r("heroku", "deploy", 4, false), r("nomad job run", "deploy", 5, true), r("argocd app sync", "deploy", 5, true), r("flux reconcile", "deploy", 4, false),
	r("reboot", "system", 5, true), r("shutdown", "system", 5, true), r("dpkg-reconfigure", "system", 4, false),

	// Weight 3: system changes.
	r("apt-get install", "package", 3, false), r("apt install", "package", 3, false), r("apt-get remove", "package", 3, false), r("apt remove", "package", 3, false),
	r("apt-get purge", "package", 3, false), r("apt-get upgrade", "package", 3, false), r("apt upgrade", "package", 3, false), r("apt-get dist-upgrade", "package", 3, false),
	r("apt-get update", "package", 1, false), r("apt update", "package", 1, false), r("apt", "package", 2, false), r("apt-get", "package", 2, false),
	r("yum install", "package", 3, false), r("yum remove", "package", 3, false), r("yum update", "package", 3, false), r("yum", "package", 2, false),
	r("dnf install", "package", 3, false), r("dnf remove", "package", 3, false), r("dnf update", "package", 3, false), r("dnf upgrade", "package", 3, false), r("dnf", "package", 2, false),
	r("zypper install", "package", 3, false), r("zypper in", "package", 3, false), r("pacman -S", "package", 3, false), r("pacman -R", "package", 3, false), r("apk add", "package", 3, false), r("apk del", "package", 3, false),
	r("brew install", "package", 3, false), r("brew uninstall", "package", 3, false), r("brew upgrade", "package", 3, false), r("brew services", "service", 4, false), r("brew", "package", 1, false),
	r("snap install", "package", 3, false), r("snap remove", "package", 3, false), r("flatpak install", "package", 3, false),
	r("pip install", "package", 3, false), r("pip3 install", "package", 3, false), r("pipx install", "package", 3, false), r("uv pip install", "package", 3, false), r("uv add", "package", 3, false),
	r("npm install", "package", 3, false), r("npm i ", "package", 3, false), r("npm i", "package", 3, false), r("npm ci", "package", 2, false), r("npm uninstall", "package", 3, false),
	r("pnpm add", "package", 3, false), r("pnpm install", "package", 3, false), r("yarn add", "package", 3, false), r("yarn install", "package", 2, false),
	r("gem install", "package", 3, false), r("bundle install", "package", 2, false), r("cargo install", "package", 3, false), r("go install", "package", 3, false), r("go get", "package", 3, false),
	r("composer require", "package", 3, false), r("composer install", "package", 2, false),
	r("systemctl start", "service", 3, false), r("systemctl stop", "service", 3, false), r("systemctl enable", "service", 3, false), r("systemctl disable", "service", 3, false),
	r("systemctl mask", "service", 3, false), r("systemctl unmask", "service", 3, false), r("systemctl daemon-reload", "service", 3, false), r("systemctl edit", "config", 3, false),
	r("systemctl status", "service", 1, false), r("systemctl", "service", 1, false), r("journalctl", "service", 1, false),
	r("launchctl load", "service", 3, false), r("launchctl unload", "service", 3, false), r("launchctl", "service", 1, false),
	r("docker run", "docker", 3, false), r("docker build", "docker", 3, false), r("docker push", "docker", 3, false), r("docker pull", "docker", 2, false),
	r("docker rm", "docker", 3, false), r("docker rmi", "docker", 3, false), r("docker restart", "docker", 3, false), r("docker stop", "docker", 3, false), r("docker start", "docker", 3, false),
	r("docker network", "docker", 3, false), r("docker volume", "docker", 3, false), r("docker exec", "docker", 2, false), r("docker ps", "docker", 1, false), r("docker logs", "docker", 1, false),
	r("docker images", "docker", 1, false), r("docker inspect", "docker", 1, false), r("docker compose logs", "docker", 1, false), r("docker compose ps", "docker", 1, false),
	r("docker compose", "docker", 3, false), r("docker-compose", "docker", 3, false), r("docker", "docker", 2, false), r("podman", "docker", 2, false),
	r("chmod", "fs", 3, false), r("chown", "fs", 3, false), r("chgrp", "fs", 3, false), r("setfacl", "fs", 3, false), r("chattr", "fs", 3, false),
	r("useradd", "users", 3, false), r("usermod", "users", 3, false), r("userdel", "users", 3, false), r("groupadd", "users", 3, false), r("groupmod", "users", 3, false),
	r("adduser", "users", 3, false), r("deluser", "users", 3, false), r("passwd", "users", 3, false), r("chpasswd", "users", 3, false), r("visudo", "config", 4, false),
	r("iptables", "firewall", 3, false), r("ip6tables", "firewall", 3, false), r("nft", "firewall", 3, false), r("ufw", "firewall", 3, false), r("firewall-cmd", "firewall", 3, false),
	r("crontab -e", "cron", 3, false), r("crontab -r", "cron", 3, false), r("crontab", "cron", 1, false),
	r("mount", "fs", 3, false), r("umount", "fs", 3, false), r("mkfs", "fs", 4, false), r("fdisk", "fs", 4, false), r("parted", "fs", 4, false), r("lvcreate", "fs", 4, false),
	r("lvextend", "fs", 4, false), r("resize2fs", "fs", 4, false), r("xfs_growfs", "fs", 4, false), r("zfs", "fs", 3, false), r("zpool", "fs", 3, false),
	r("ip link set", "network", 3, false), r("ip addr add", "network", 3, false), r("ip route add", "network", 3, false), r("nmcli", "network", 3, false), r("netplan apply", "network", 4, false),
	r("nginx -s reload", "service", 4, false), r("nginx -t", "config", 2, false), r("apachectl", "service", 3, false), r("a2enmod", "config", 3, false), r("a2ensite", "config", 3, false),
	r("update-alternatives", "config", 3, false), r("update-grub", "system", 4, false), r("grub-install", "system", 4, false), r("sysctl -w", "config", 3, false), r("sysctl -p", "config", 3, false),
	r("modprobe", "system", 3, false), r("timedatectl set", "config", 3, false), r("hostnamectl set", "config", 3, false), r("localectl set", "config", 3, false),
	r("ssh-keygen", "ssh", 3, false), r("ssh-copy-id", "ssh", 3, false), r("gpg --gen-key", "crypto", 3, false), r("openssl req", "tls", 3, false), r("openssl x509", "tls", 2, false),
	r("terraform init", "terraform", 2, false), r("terraform plan", "terraform", 2, false), r("terraform import", "terraform", 4, false), r("terraform state", "terraform", 4, false), r("terraform", "terraform", 2, false),
	r("ansible", "ansible", 3, false), r("kubectl create", "kubernetes", 4, false), r("kubectl label", "kubernetes", 3, false), r("kubectl annotate", "kubernetes", 3, false),
	r("kubectl get", "kubernetes", 1, false), r("kubectl describe", "kubernetes", 1, false), r("kubectl logs", "kubernetes", 1, false), r("kubectl top", "kubernetes", 1, false), r("kubectl", "kubernetes", 2, false),
	r("helm", "helm", 2, false), r("aws s3 cp", "cloud", 2, false), r("aws s3 sync", "cloud", 3, false), r("aws", "cloud", 2, false), r("gcloud", "cloud", 2, false), r("az", "cloud", 2, false),
	r("psql", "database", 2, false), r("mysql", "database", 2, false), r("mongosh", "database", 2, false), r("redis-cli", "database", 2, false), r("sqlite3", "database", 2, false),
	r("pg_dump", "database", 3, false), r("pg_restore", "database", 4, false), r("mysqldump", "database", 3, false), r("mongodump", "database", 3, false), r("mongorestore", "database", 4, false),
	r("alembic upgrade", "database", 4, false), r("rails db:migrate", "database", 4, false), r("prisma migrate", "database", 4, false), r("flyway migrate", "database", 4, false), r("liquibase update", "database", 4, false),
	r("rsync", "transfer", 2, false), r("scp", "transfer", 2, false), r("sftp", "transfer", 2, false), r("wget", "transfer", 1, false), r("curl -X POST", "http", 2, false), r("curl -X PUT", "http", 2, false),
	r("curl -X DELETE", "http", 2, false), r("curl -d", "http", 2, false), r("curl --data", "http", 2, false), r("curl", "http", 1, false), r("http", "http", 1, false), r("httpie", "http", 1, false),
	r("ssh", "ssh", 2, false), r("mosh", "ssh", 2, false), r("tailscale", "network", 3, false), r("wg-quick", "network", 3, false), r("wg", "network", 2, false),
	r("git add", "git", 2, false), r("git checkout", "git", 2, false), r("git switch", "git", 2, false), r("git branch", "git", 2, false), r("git stash", "git", 2, false),
	r("git pull", "git", 2, false), r("git fetch", "git", 1, false), r("git clone", "git", 3, false), r("git init", "git", 3, false), r("git reset", "git", 3, false), r("git restore", "git", 2, false),
	r("git status", "git", 1, false), r("git log", "git", 1, false), r("git diff", "git", 1, false), r("git show", "git", 1, false), r("git blame", "git", 1, false), r("git remote", "git", 2, false), r("git", "git", 1, false),
	r("gh pr create", "git", 4, false), r("gh pr merge", "git", 5, true), r("gh release create", "git", 5, true), r("gh", "git", 1, false), r("glab", "git", 1, false),
	r("make", "build", 2, false), r("go build", "build", 2, false), r("go test", "build", 2, false), r("go run", "build", 2, false), r("go mod", "build", 2, false), r("go", "build", 1, false),
	r("cargo build", "build", 2, false), r("cargo test", "build", 2, false), r("cargo run", "build", 2, false), r("cargo", "build", 1, false),
	r("npm run", "build", 2, false), r("npm test", "build", 2, false), r("npm start", "build", 2, false), r("npm publish", "build", 5, true), r("npm", "build", 1, false),
	r("pnpm", "build", 1, false), r("yarn", "build", 1, false), r("npx", "build", 2, false), r("bun", "build", 1, false), r("deno", "build", 1, false),
	r("pytest", "build", 2, false), r("python -m pytest", "build", 2, false), r("python", "build", 1, false), r("python3", "build", 1, false), r("uv run", "build", 2, false),
	r("mvn", "build", 2, false), r("gradle", "build", 2, false), r("./gradlew", "build", 2, false), r("dotnet", "build", 2, false), r("cmake", "build", 2, false), r("ninja", "build", 2, false),
	r("tar -x", "archive", 2, false), r("tar x", "archive", 2, false), r("tar -c", "archive", 2, false), r("tar c", "archive", 2, false), r("tar", "archive", 1, false), r("unzip", "archive", 2, false), r("zip", "archive", 2, false), r("gunzip", "archive", 1, false),
	r("mkdir", "fs", 1, false), r("rm -rf", "fs", 3, false), r("rm -r", "fs", 3, false), r("rm", "fs", 2, false), r("mv", "fs", 2, false), r("cp", "fs", 2, false), r("ln", "fs", 2, false), r("touch", "fs", 1, false), r("truncate", "fs", 2, false),
	r("ln -s", "fs", 2, false), r("find", "fs", 1, false), r("locate", "fs", 1, false), r("dd", "fs", 4, false), r("kill", "process", 2, false), r("killall", "process", 2, false), r("pkill", "process", 2, false),
	r("vim", "edit", 2, false), r("vi", "edit", 2, false), r("nvim", "edit", 2, false), r("nano", "edit", 2, false), r("emacs", "edit", 2, false), r("micro", "edit", 2, false), r("hx", "edit", 2, false),
	r("code", "edit", 2, false), r("subl", "edit", 2, false), r("gedit", "edit", 2, false), r("ed", "edit", 2, false), r("visudo", "config", 4, false), r("update-ca-certificates", "tls", 3, false),
	r("crontab", "cron", 1, false), r("at", "cron", 2, false), r("tmux", "noise", 0, false),
}

func r(prefix, family string, weight int, milestone bool) rule {
	return rule{prefix: prefix, family: family, weight: weight, milestone: milestone, words: len(strings.Fields(prefix))}
}

var (
	editors    = map[string]bool{"vim": true, "vi": true, "nvim": true, "nano": true, "emacs": true, "micro": true, "hx": true, "code": true, "subl": true, "gedit": true, "ed": true, "kak": true}
	configExts = map[string]bool{".yml": true, ".yaml": true, ".toml": true, ".conf": true, ".cfg": true, ".ini": true, ".env": true, ".tf": true, ".tfvars": true, ".json": true, ".xml": true, ".properties": true, ".service": true, ".timer": true, ".nix": true, ".hcl": true}
	rePrefix   = regexp.MustCompile(`^\s*(?:(?:sudo|doas)(?:\s+-\S+)*\s+|(?:env|time|nohup|command|exec|builtin|nice|ionice|strace|watch)\s+|[A-Za-z_][A-Za-z0-9_]*=\S*\s+|\\)*`)
	reSegment  = regexp.MustCompile(`\s*(?:\|\|?|&&|;)\s*`)
	reRedirect = regexp.MustCompile(`(?:^|\s)(?:>>|>|2>|&>)\s*([^\s;&|]+)`)
)

// Classify scores a full command line. cwd resolves relative file paths.
func (c *Classifier) Classify(cmd, cwd string) Result {
	res := Result{Kind: model.KindNoise, Family: "noise"}
	seen := map[string]bool{}
	for _, seg := range reSegment.Split(cmd, -1) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		sr := c.classifySegment(seg, cwd)
		if sr.Weight > res.Weight || (res.Family == "noise" && sr.Family != "noise") {
			res.Weight = sr.Weight
			res.Family = sr.Family
		}
		res.Milestone = res.Milestone || sr.Milestone
		for _, f := range sr.FilesTouched {
			if !seen[f] {
				seen[f] = true
				res.FilesTouched = append(res.FilesTouched, f)
			}
		}
	}
	if strings.Contains(cmd, "sudo ") && res.Weight > 0 && res.Weight < 5 {
		res.Weight++
	}
	if res.Weight > 0 {
		res.Kind = model.KindMeaningful
	}
	if res.Family == "noise" && res.Weight > 0 {
		res.Family = "other"
	}
	return res
}

func (c *Classifier) classifySegment(seg, cwd string) Result {
	body := seg
	if loc := rePrefix.FindStringIndex(seg); loc != nil {
		body = seg[loc[1]:]
	}
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return Result{Family: "noise"}
	}
	prog := filepath.Base(fields[0])
	fields[0] = prog
	joined := strings.Join(fields, " ")
	res := Result{Family: "other", Weight: 1}

	matched := false
	for _, rl := range c.weights {
		if joined == rl.prefix || strings.HasPrefix(joined, rl.prefix+" ") || (rl.words == 1 && prog == rl.prefix) {
			res.Family, res.Weight, res.Milestone = rl.family, rl.weight, rl.milestone
			matched = true
			break
		}
	}
	if !matched && c.noise[prog] {
		res.Family, res.Weight = "noise", 0
	}
	if matched && res.Weight == 0 {
		res.Family = "noise"
	}
	// A noise program that writes to a file is still a change.
	files := touchedFiles(prog, fields[1:], cwd)
	if m := reRedirect.FindAllStringSubmatch(seg, -1); m != nil {
		for _, mm := range m {
			target := mm[1]
			if target == "/dev/null" || strings.HasPrefix(target, "&") {
				continue
			}
			files = append(files, absPath(target, cwd))
			if res.Weight < 2 {
				res.Weight = 2
				if res.Family == "noise" {
					res.Family = "fs"
				}
			}
		}
	}
	if editors[prog] && len(files) > 0 {
		for _, f := range files {
			if isConfigPath(f) {
				res.Family = "config"
				if res.Weight < 3 {
					res.Weight = 3
				}
			}
		}
	}
	res.FilesTouched = files
	return res
}

func touchedFiles(prog string, args []string, cwd string) []string {
	var out []string
	add := func(a string) {
		if a == "" || strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+") {
			return
		}
		out = append(out, absPath(a, cwd))
	}
	switch {
	case editors[prog]:
		for _, a := range args {
			add(a)
		}
	case prog == "touch" || prog == "tee":
		for _, a := range args {
			add(a)
		}
	case prog == "cp" || prog == "mv" || prog == "install":
		var pos []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				pos = append(pos, a)
			}
		}
		if len(pos) >= 2 {
			add(pos[len(pos)-1])
		}
	case prog == "chmod" || prog == "chown" || prog == "chgrp":
		var pos []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				pos = append(pos, a)
			}
		}
		if len(pos) >= 2 {
			for _, p := range pos[1:] {
				add(p)
			}
		}
	case prog == "systemctl":
		for i, a := range args {
			if a == "edit" && i+1 < len(args) {
				add("/etc/systemd/system/" + args[i+1] + ".d/override.conf")
			}
		}
	case prog == "crontab":
		for _, a := range args {
			if a == "-e" {
				out = append(out, "crontab")
			}
		}
	case prog == "visudo":
		out = append(out, "/etc/sudoers")
	}
	return out
}

func absPath(p, cwd string) string {
	p = strings.Trim(p, `"'`)
	if strings.HasPrefix(p, "~/") {
		return p
	}
	if filepath.IsAbs(p) || cwd == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

func isConfigPath(p string) bool {
	if strings.HasPrefix(p, "/etc/") || strings.Contains(p, "/.config/") || strings.HasSuffix(p, "rc") {
		return true
	}
	return configExts[strings.ToLower(filepath.Ext(p))] || strings.HasPrefix(filepath.Base(p), ".env")
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	return filepath.Base(f[0])
}
