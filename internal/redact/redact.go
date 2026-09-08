// Package redact scrubs secrets out of command lines before anything is
// persisted. The rules run in order; each replaces matches with a
// <redacted:kind> marker and records the kind. Values can optionally be
// captured (for the credential cache) but the returned text never contains
// them.
package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Captured is a secret value extracted from a command, offered to the
// credential cache when the policy allows capture. It is never persisted by
// this package.
type Captured struct {
	Kind     string // password, token, basic_auth, ...
	Program  string // program the secret was passed to, when known
	Username string // associated user, when known
	Value    string
}

// Result is the outcome of Redact.
type Result struct {
	Text     string
	Kinds    []string
	Captured []Captured
}

// Redacted reports whether anything was replaced.
func (r Result) Redacted() bool { return len(r.Kinds) > 0 }

// Options tune the redactor.
type Options struct {
	ExtraPatterns          []string
	ExtraSensitiveCommands []string
	// Capture keeps extracted values in Result.Captured.
	Capture bool
}

// Redactor applies the rules.
type Redactor struct {
	extra     []*regexp.Regexp
	sensitive map[string]bool
	capture   bool
}

// New builds a redactor; invalid extra patterns are reported as an error.
func New(opts Options) (*Redactor, error) {
	r := &Redactor{sensitive: map[string]bool{}, capture: opts.Capture}
	for _, p := range opts.ExtraPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact.extra_patterns %q: %w", p, err)
		}
		r.extra = append(r.extra, re)
	}
	for k := range sensitivePrograms {
		r.sensitive[k] = true
	}
	for _, c := range opts.ExtraSensitiveCommands {
		r.sensitive[strings.TrimSpace(c)] = true
	}
	return r, nil
}

// Default is a redactor with the built-in rules only.
var Default = mustNew()

func mustNew() *Redactor {
	r, err := New(Options{})
	if err != nil {
		panic(err)
	}
	return r
}

// Marker renders the replacement marker for a kind.
func Marker(kind string) string { return "<redacted:" + kind + ">" }

// Programs whose argument lists are always hidden entirely.
var sensitivePrograms = map[string]bool{
	"vault": true, "pass": true, "gpg": true, "gpg2": true, "htpasswd": true, "chpasswd": true,
	"passwd": true, "op": true, "bw": true, "sshpass": true, "cryptsetup": true, "keyctl": true,
	"secret-tool": true, "security": true, "kinit": true, "smbpasswd": true, "ldappasswd": true,
}

// Two-word programs treated as sensitive.
var sensitivePrefixes = []string{
	"openssl enc", "openssl pkcs12", "openssl rsa", "kubectl create secret", "aws secretsmanager put-secret-value",
	"aws secretsmanager create-secret", "aws ssm put-parameter", "docker login", "podman login", "helm registry login",
	"gh auth login", "az login", "gcloud auth activate-service-account", "mysql_config_editor", "useradd", "usermod",
	"net user", "samba-tool user",
}

// Programs that read a secret from stdin, so the producer segment before the
// pipe is redacted too.
var stdinConsumers = []string{"chpasswd", "passwd", "docker login", "podman login", "sudo -S", "htpasswd -i", "vault", "op ", "bw ", "gpg", "kubectl create secret", "helm registry login", "secret-tool", "cryptsetup"}

var (
	rePrivateKey = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
	reHeredoc    = regexp.MustCompile(`(?s)(<<-?\s*['"]?[A-Za-z_][A-Za-z0-9_]*['"]?)(.*)$`)
	reURLUser    = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@'"]+):([^@\s/'"]+)@`)
	reEnvAssign  = regexp.MustCompile(`(?i)(^|[\s"'=])([A-Z0-9_]*(?:PASS|PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|AUTH|CREDENTIALS?)[A-Z0-9_]*)=("[^"]*"|'[^']*'|\S+)`)
	reFlag       = regexp.MustCompile(`(?i)(--?(?:password|passwd|pass|pwd|token|api-key|apikey|api_key|secret|access-key|secret-key|client-secret|auth-token|private-key|bearer|credentials?|key)(?:=|\s+))("[^"]*"|'[^']*'|\S+)`)
	reMysqlP     = regexp.MustCompile(`(\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlimport|mycli)\b[^|;&]*?\s-p)([^\s-]\S*)`)
	reAuthHeader = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic|token|apikey)?\s*)([^\s"']+)`)
	reBasicAuth  = regexp.MustCompile(`(\s(?:-u|--user|--proxy-user)[\s=]+)(['"]?)([^:\s'"]+):([^\s'"]+)`)
	reUserPass   = regexp.MustCompile(`(?i)(\b(?:user(?:name)?|login)\s*[:=]\s*["']?)([A-Za-z0-9._@-]+)(["']?\s*[,;&\s]+\s*(?:pass(?:word)?|pwd)\s*[:=]\s*["']?)([^\s"',;&]+)`)
	reGeneric    = regexp.MustCompile(`(?i)\b(token|secret|password|passwd|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret)\b(\s*[:=]\s*["']?)([A-Za-z0-9+/_=.-]{12,})`)
	reTokens     = []*regexp.Regexp{
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
		regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
		regexp.MustCompile(`\bhvs\.[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bhvb\.[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bs\.[A-Za-z0-9]{24}\b`),
		regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`),
		regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{30,}\b`),
		regexp.MustCompile(`\bdop_v1_[a-f0-9]{64}\b`),
		regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
	}
	reSudoEnvPrefix = regexp.MustCompile(`^\s*(?:(?:sudo|doas)(?:\s+-\S+)*\s+|(?:env|time|nohup|command|exec|builtin)\s+|[A-Za-z_][A-Za-z0-9_]*=\S*\s+|\\)*`)
	reSegment       = regexp.MustCompile(`\s*(\|\|?|&&|;)\s*`)
)

// Redact scrubs the command line.
func (r *Redactor) Redact(cmd string) Result {
	res := Result{Text: cmd}
	kinds := map[string]bool{}
	mark := func(kind string) string {
		kinds[kind] = true
		return Marker(kind)
	}

	// 1. Private key blobs, heredocs.
	res.Text = rePrivateKey.ReplaceAllStringFunc(res.Text, func(string) string { return mark("private_key") })
	if m := reHeredoc.FindStringSubmatchIndex(res.Text); m != nil {
		res.Text = res.Text[:m[3]] + " " + mark("heredoc")
	}

	// 2. Sensitive programs: hide their whole argument list, and the producer
	// of anything piped into a stdin-consuming one.
	res.Text = r.redactSegments(res.Text, mark, &res)

	// 3. URL userinfo.
	res.Text = reURLUser.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reURLUser.FindStringSubmatch(s)
		r.capturef(&res, "url_password", "", userFromURL(m[1]), m[2])
		return m[1] + ":" + mark("url_password") + "@"
	})

	// 4. Env assignments and flags.
	res.Text = reEnvAssign.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reEnvAssign.FindStringSubmatch(s)
		if strings.EqualFold(m[2], "PWD") || strings.EqualFold(m[2], "OLDPWD") {
			return s
		}
		r.capturef(&res, "env", "", "", unquote(m[3]))
		return m[1] + m[2] + "=" + mark("env")
	})
	res.Text = reFlag.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reFlag.FindStringSubmatch(s)
		if strings.HasPrefix(m[2], "<redacted:") || strings.HasPrefix(m[2], "-") {
			return s
		}
		r.capturef(&res, "flag", "", "", unquote(m[2]))
		return m[1] + mark("flag")
	})
	res.Text = reMysqlP.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reMysqlP.FindStringSubmatch(s)
		r.capturef(&res, "password", "mysql", "", m[2])
		return m[1] + mark("password")
	})

	// 5. Auth headers and basic auth.
	res.Text = reAuthHeader.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reAuthHeader.FindStringSubmatch(s)
		if strings.HasPrefix(m[2], "<redacted:") {
			return s
		}
		r.capturef(&res, "auth_header", "", "", m[2])
		return m[1] + mark("auth_header")
	})
	res.Text = reBasicAuth.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reBasicAuth.FindStringSubmatch(s)
		r.capturef(&res, "basic_auth", "", m[3], m[4])
		return m[1] + m[2] + m[3] + ":" + mark("basic_auth")
	})
	res.Text = reUserPass.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reUserPass.FindStringSubmatch(s)
		if strings.HasPrefix(m[4], "<redacted:") {
			return s
		}
		r.capturef(&res, "password", "", m[2], m[4])
		return m[1] + m[2] + m[3] + mark("password")
	})

	// 6. Well-known token shapes and generic key=value secrets.
	for _, re := range reTokens {
		res.Text = re.ReplaceAllStringFunc(res.Text, func(s string) string {
			r.capturef(&res, "token", "", "", s)
			return mark("token")
		})
	}
	res.Text = reGeneric.ReplaceAllStringFunc(res.Text, func(s string) string {
		m := reGeneric.FindStringSubmatch(s)
		if strings.HasPrefix(m[3], "<redacted:") {
			return s
		}
		r.capturef(&res, "secret", "", "", m[3])
		return m[1] + m[2] + mark("secret")
	})

	// 7. User-defined patterns.
	for _, re := range r.extra {
		res.Text = re.ReplaceAllStringFunc(res.Text, func(string) string { return mark("custom") })
	}

	for k := range kinds {
		res.Kinds = append(res.Kinds, k)
	}
	sort.Strings(res.Kinds)
	return res
}

func (r *Redactor) capturef(res *Result, kind, program, user, value string) {
	if !r.capture || value == "" || strings.HasPrefix(value, "<redacted:") {
		return
	}
	res.Captured = append(res.Captured, Captured{Kind: kind, Program: program, Username: user, Value: value})
}

// redactSegments splits on pipes/&&/; and hides arguments of sensitive
// programs. Segments feeding a stdin consumer are hidden as well.
func (r *Redactor) redactSegments(text string, mark func(string) string, res *Result) string {
	locs := reSegment.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return r.redactSegment(text, false, mark, res)
	}
	var out strings.Builder
	prev := 0
	segments := make([]string, 0, len(locs)+1)
	seps := make([]string, 0, len(locs))
	for _, l := range locs {
		segments = append(segments, text[prev:l[0]])
		seps = append(seps, text[l[0]:l[1]])
		prev = l[1]
	}
	segments = append(segments, text[prev:])
	for i, seg := range segments {
		feedsConsumer := false
		if i+1 < len(segments) && strings.TrimSpace(seps[i]) == "|" && r.isStdinConsumer(segments[i+1]) {
			feedsConsumer = true
		}
		out.WriteString(r.redactSegment(seg, feedsConsumer, mark, res))
		if i < len(seps) {
			out.WriteString(seps[i])
		}
	}
	return out.String()
}

func (r *Redactor) isStdinConsumer(seg string) bool {
	head := strings.TrimSpace(stripPrefix(seg))
	for _, c := range stdinConsumers {
		if strings.HasPrefix(head, c) {
			return true
		}
	}
	return false
}

func (r *Redactor) redactSegment(seg string, feedsConsumer bool, mark func(string) string, res *Result) string {
	prefixLen := len(seg) - len(stripPrefix(seg))
	body := seg[prefixLen:]
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return seg
	}
	prog := baseName(fields[0])
	sensitive := r.sensitive[prog]
	prefixWords := 0
	if !sensitive {
		joined := strings.Join(fields, " ")
		for _, p := range sensitivePrefixes {
			if joined == p || strings.HasPrefix(joined, p+" ") {
				sensitive = true
				prefixWords = len(strings.Fields(p))
				break
			}
		}
	}
	if feedsConsumer && (prog == "echo" || prog == "printf" || prog == "cat") {
		if len(fields) > 1 {
			r.captureProducer(res, fields[1:])
			return seg[:prefixLen] + fields[0] + " " + mark("argv")
		}
		return seg
	}
	if !sensitive || len(fields) == 1 {
		return seg
	}
	keep := 1
	// Keep the subcommand words so the journal still says what happened
	// (e.g. "vault write <redacted:argv>").
	if len(fields) > 2 && !strings.HasPrefix(fields[1], "-") {
		keep = 2
	}
	if prefixWords > keep && prefixWords < len(fields) {
		keep = prefixWords
	}
	if prog == "useradd" || prog == "usermod" {
		return seg[:prefixLen] + redactUserAdd(fields, mark, r, res)
	}
	return seg[:prefixLen] + strings.Join(fields[:keep], " ") + " " + mark("argv")
}

// redactUserAdd keeps useradd/usermod readable but hides -p values.
func redactUserAdd(fields []string, mark func(string) string, r *Redactor, res *Result) string {
	out := make([]string, 0, len(fields))
	user := fields[len(fields)-1]
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-p" || f == "--password":
			out = append(out, f, mark("password"))
			if i+1 < len(fields) {
				r.capturef(res, "password", fields[0], user, unquote(fields[i+1]))
				i++
			}
		case strings.HasPrefix(f, "--password="):
			r.capturef(res, "password", fields[0], user, unquote(strings.TrimPrefix(f, "--password=")))
			out = append(out, "--password="+mark("password"))
		case strings.HasPrefix(f, "-p") && len(f) > 2:
			r.capturef(res, "password", fields[0], user, unquote(f[2:]))
			out = append(out, "-p"+mark("password"))
		default:
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// captureProducer extracts user:password pairs from an echo feeding chpasswd.
func (r *Redactor) captureProducer(res *Result, args []string) {
	if !r.capture {
		return
	}
	joined := unquote(strings.Join(args, " "))
	joined = strings.TrimPrefix(joined, "-e ")
	joined = strings.TrimPrefix(joined, "-n ")
	for _, line := range strings.Split(joined, "\\n") {
		line = strings.TrimSpace(unquote(line))
		if line == "" {
			continue
		}
		if u, p, ok := strings.Cut(line, ":"); ok && u != "" && p != "" {
			res.Captured = append(res.Captured, Captured{Kind: "password", Program: "chpasswd", Username: u, Value: p})
		} else {
			res.Captured = append(res.Captured, Captured{Kind: "password", Program: "stdin", Value: line})
		}
	}
}

func stripPrefix(seg string) string {
	loc := reSudoEnvPrefix.FindStringIndex(seg)
	if loc == nil {
		return seg
	}
	return seg[loc[1]:]
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func userFromURL(prefix string) string {
	if i := strings.LastIndex(prefix, "//"); i >= 0 {
		return prefix[i+2:]
	}
	return ""
}
