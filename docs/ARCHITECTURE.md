# How codeument is put together

One Go binary. The same executable is the shell agent, the review UI, the
snapshot collector and the company relay; which one you get depends on the
subcommand. Nothing is a long-running process unless you ask for it.

## The path a command takes

```
your shell
  │  preexec/precmd hook (bash, zsh, fish)
  ▼
codeument ingest         redact → classify → journal → update the open batch
  │                      (detached; the hook waits for nothing)
  ▼  a trigger fires
codeument work           batch → prompt → LLM → Draft → notification file
  │
  ▼  at your next prompt: "codeument: 2 drafts ready"
codeument review         list → detail → edit in $EDITOR → accept → publish
  │
  ▼
docs provider            Markdown files, or Confluence Cloud
```

`codeument tick` runs the same worker on a timer for anything the hook did
not trigger: idle batches, retries, pruning, and server snapshots.

## Packages

| Package | Responsibility |
|---|---|
| `capture` | shell hook snippets and the record parser; reads `.git/HEAD` for repo context |
| `redact` | ordered secret-scrubbing rules, applied before anything is stored |
| `classify` | noise vs meaningful, family, weight, milestone, touched files |
| `journal` | SQLite store: events, sessions, batches, drafts, page mappings, snapshots |
| `batch` | session and batch boundaries, trigger evaluation |
| `summarize` | prompt assembly, structured Draft output, validation and retry |
| `llm` | provider interface; `anthropic`, `ollama`, `fake` |
| `docs` | provider interface; `markdown`, `confluence`, `render` (storage XHTML) |
| `engine` | the seam: `Local` runs providers in-process, `relay/client` forwards to the relay |
| `worker` | batch → draft → accept → publish, with a retry queue |
| `tui` | the review screen |
| `snapshot` | machine state collection, LLM directory explanations, diffing, page rendering |
| `secrets` + `credcache` | credential policy, the four password managers, the encrypted local cache |
| `relay` | the company server: enrollment, auth, the same summarize/publish paths, audit |
| `scheduler` | tick and the systemd/launchd units |

## The engine seam

Everything above `engine.Engine` is identical in direct and relay mode: the
same batching, the same prompts, the same review screen, the same snapshot
code. Only the implementation behind the interface changes.

```go
type Engine interface {
    Summarize(ctx, summarize.Input) (*summarize.Output, error)
    Explain(ctx, explain.Input) ([]explain.Explanation, error)
    Publish(ctx, docs.Document) (docs.PageRef, error)
    FindDoc(ctx, id string) (*docs.PageRef, error)
    GetDoc(ctx, docs.PageRef) (*docs.Document, error)
    Defaults(ctx) (*Defaults, error)
    StoreCredential(ctx, secrets.Credential) (secrets.Reference, error)
    Ping(ctx) error
}
```

`engine.Local` holds an `llm.Provider` and a `docs.Provider`. `relay/client`
speaks the same interface over HTTP. `engine.Unavailable` is what local mode
gets, so the review screen still works and publishing fails with one clear
message. That is why moving a company from Markdown to Confluence, or from
Claude to a local Ollama, is a relay-side change no client notices.

## Documents are idempotent

Every page carries a codeument document id. The Markdown provider keeps it in
YAML front matter; the Confluence provider keeps it in a content property
alongside the original Markdown, so a page round-trips exactly and updates
land on the same page instead of creating a new one. A second draft aimed at
the same doc id is appended as a dated section rather than replacing what is
there. Server pages use a stable id derived from the machine id, so a
hostname change does not fork the page.

## Why SQLite, why no daemon

The hook path must be fast and concurrent: several shells write at once, and
the worker reads open batches while they do. That needs queries and locking,
not an append-only file. The pure-Go driver keeps the binary cgo-free.

There is no required background process. The hook spawns a detached worker
only when a trigger fires, and a timer runs `tick` for periodic work. A
server with no interactive session still gets its snapshot from the
system-scope timer.

## Testing

Unit tests everywhere, plus: golden files for prompts and rendered pages,
`httptest` stubs for Confluence, the relay, and all four password managers,
`/proc`-shaped fixtures for the snapshot collectors, and an integration test
that drives real bash, zsh and fish through a pty and asserts what landed in
the journal. `make test` runs the unit suite; `make integration` adds the
shell tests.
