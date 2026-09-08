# Security and privacy

codeument watches shell work, so what it keeps and what it sends matters more
than most tools. This page is the complete answer; `codeument status
--privacy` prints the short version for your current configuration.

## Nothing leaves the machine until you say so

A fresh install is `mode: local`: commands are journaled locally and nothing
is summarized or published. `codeument init` (direct mode) or `codeument
enroll` (relay mode) changes that deliberately and prints what will be sent.

## Redaction happens before anything is written

Every command line passes the redaction pipeline before it reaches the
journal. The stored text carries `<redacted:kind>` markers, and only the kind
is recorded, never the value. Rules cover:

- token shapes: AWS access keys, GitHub (`ghp_`, `github_pat_`), Slack, GitLab,
  Google, Anthropic and OpenAI style keys, Vault tokens, npm and PyPI tokens,
  SendGrid, DigitalOcean, and JSON web tokens
- flags: `--password`, `--token`, `--api-key`, `--secret`, `mysql -p<pw>`,
  `curl -u user:pass`, `-H "Authorization: ..."`
- environment assignments whose name contains PASS, PASSWORD, SECRET, TOKEN,
  API_KEY, PRIVATE_KEY, ACCESS_KEY or CREDENTIAL
- config-style `key: value` pairs with the same keywords (YAML, ini, compose)
- URL user info (`scheme://user:password@host`)
- whole argument lists of sensitive programs: `vault`, `pass`, `gpg`,
  `passwd`, `chpasswd`, `sshpass`, `htpasswd`, `openssl enc`, `docker login`,
  `kubectl create secret`, `aws secretsmanager`, and anything you add in
  `redact.extra_sensitive_commands`
- the producer side of a pipe into one of those programs (`echo x | chpasswd`)
- heredoc bodies and PEM private key blocks

The rules are table-tested against a corpus and fuzzed. Add your own with
`redact.extra_patterns`.

## What is never captured

Command output (stdout and stderr), environment variables, file contents,
keystrokes, password hashes, and private key material. The snapshot collector
refuses to read `/etc/shadow`, `/etc/gshadow`, `*.pem`, `*.key`, private SSH
keys and Let's Encrypt private material; the refusal is enforced by an
allow-list check and covered by a test that greps the rendered page and the
stored JSON for hash-shaped strings.

## What leaves the machine, by mode

| Mode | Sent | To |
|---|---|---|
| local | nothing | |
| direct | redacted commands, exit codes, timestamps, working directories, git branch and diff stat (file names and line counts only), touched file paths, hostname, username | your LLM provider |
| direct | the draft text you approved | your docs provider |
| relay | the same batch data and approved drafts, over TLS with a per-client token | your company relay, which forwards to the providers it holds |

Server snapshots additionally send: OS and kernel, resource usage, mounts,
listening ports and owning process names, services, cron jobs and timers,
containers, account names with their groups and sudo rights, directory
listings, and redacted first lines of README and configuration files.

## Credentials on server pages

The credential policy decides what the accounts table shows. In relay mode
the relay's policy wins and is pushed to clients.

| Mode | Page shows | Password stored |
|---|---|---|
| `reference` (default) | a reference string such as `vault://infra/web-01/root` | nowhere by codeument |
| `manager` | a link to the entry in the company password manager | in Vault, 1Password, Bitwarden or via your webhook |
| `inline` | the password itself | on the page; restrict it with `credentials.inline.page_restriction_groups` |

Passwords come from you at review time (masked input), or, when
`credentials.capture_from_commands` is on, from command lines the redactor
already scrubbed. Captured values go to an encrypted local cache
(`credcache.db`, key file 0600), never to the journal, and only reach the
relay or manager over TLS when you attach them to an account.

## File permissions

Config file 0600, data directory 0700, relay token 0600, credential cache key
0600. `codeument config validate` warns when any of them is loose.

## Relay

TLS is required unless the relay binds loopback or the operator sets
`insecure_http` deliberately behind a terminating proxy; optional mTLS with
`tls.client_ca`. Enrollment codes are single-use and expire. Tokens are
stored hashed. Requests are size- and rate-limited per client. The audit log
records who called what, when, with how many events and tokens, and stores no
command payloads unless `audit.store_payloads` is turned on. The credential
endpoint logs only host and username.

## Your controls

```sh
codeument pause 1h          # stop capturing for a while
CODEUMENT_OFF=1             # per-shell kill switch
codeument forget --last 5   # delete recent events
codeument forget --since 1h
codeument credentials forget
codeument purge --all       # wipe the journal
codeument status --privacy  # what leaves this machine right now
```

Deleting events marks any draft built on them stale.

## Supply chain

Single static binary, `CGO_ENABLED=0`, `-trimpath` builds, `go.sum` verified
in CI, `govulncheck` on every push, and a small dependency set.
