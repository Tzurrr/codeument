# codeument

Documentation that writes itself from the work you do in the shell.

codeument is a small Go binary that lives in your terminal. It records the
commands you run (redacted, locally), groups them into meaningful batches,
has an LLM draft a documentation entry for each batch, and shows you only the
finished draft to accept, edit or dismiss. Accepted drafts are published to
your documentation platform (Confluence Cloud, or a folder of Markdown). On
servers it also maintains a living "Server: hostname" page with the important
folders, resource usage, services, ports, cron jobs and accounts.

Nothing leaves the machine until you switch the client from `local` to
`direct` (it talks to the LLM and docs platform itself) or `relay` (it talks
to a company relay run by IT that holds all credentials).

## Install

```sh
go install github.com/Tzurrr/codeument/cmd/codeument@latest
codeument hook --install        # adds one line to ~/.bashrc / ~/.zshrc / fish conf.d
exec $SHELL                     # start capturing
```

The first run writes a commented config to `~/.config/codeument/config.yaml`.
Edit it or use `codeument config set key value`.

## Daily use

| Command | What it does |
|---|---|
| `codeument status` | What has been captured, pending batches, drafts waiting |
| `codeument review` | Review drafts: edit, accept, dismiss, publish |
| `codeument now -m "fixing nginx 502s"` | Summarize the current session right now |
| `codeument pause 1h` / `codeument resume` | Stop capturing for a while (`CODEUMENT_OFF=1` also works) |
| `codeument forget --last 5` | Delete recent events |
| `codeument status --privacy` | Exactly what leaves the machine in the current mode |
| `codeument snapshot --publish` | Snapshot this server into its page |
| `codeument enroll --relay https://relay.corp --code XXXX` | Join a company relay |
| `codeument relay serve` | Run the relay (IT) |

## Modes

- **local** (default): capture only. `codeument status` shows what would be summarized.
- **direct**: `codeument init` asks for an LLM provider (Claude or Ollama) and a docs
  provider (Markdown folder or Confluence Cloud). Credentials stay in your config.
- **relay**: `codeument enroll` joins a relay on your network. The relay holds the
  LLM and Confluence credentials, pushes shared defaults, and applies the
  company credential policy for server pages. See `docs/RELAY.md`.

## Security

Command lines are redacted before they are stored: tokens, passwords, auth
headers, key material, URL passwords and the arguments of sensitive programs
(`vault`, `passwd`, `docker login`, ...). Command output, environment and file
contents are never captured. See `docs/SECURITY.md`.

## Development

```sh
make build test lint            # unit tests and linter
make integration                # drives the real shell hooks through a pty
```

Architecture notes live in `docs/ARCHITECTURE.md`.
