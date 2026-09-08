# The codeument relay

The relay is the same `codeument` binary run with `codeument relay serve`. IT
runs one per company (or per site). It holds the LLM key and the Confluence
credentials, applies the company credential policy for server pages, pushes
shared defaults to clients, and keeps an audit log of who used it. Clients
never see any of those secrets; they hold a per-client bearer token.

```
laptop ──┐                              ┌── Claude API / Ollama
server ──┼── TLS, bearer token ── relay ─┼── Confluence Cloud / Markdown
server ──┘                              └── Vault / 1Password / Bitwarden / webhook
```

## Install

```sh
codeument relay init                 # writes /etc/codeument/relay.yaml
$EDITOR /etc/codeument/relay.yaml    # set listen, tls, llm, docs, credentials
codeument relay check                # validates and pings the providers
codeument relay serve                # or the docker-compose in deploy/relay
```

The generated config listens on loopback only. To serve the LAN either set
`tls.cert` and `tls.key`, or terminate TLS in a reverse proxy and set
`insecure_http: true` deliberately. Optional `tls.client_ca` enables mTLS.

Install as a service with systemd:

```ini
[Unit]
Description=codeument relay
After=network-online.target
[Service]
ExecStart=/usr/local/bin/codeument relay serve
User=codeument
Restart=on-failure
[Install]
WantedBy=multi-user.target
```

## Enrolling clients

```sh
# on the relay host
codeument relay client add --name alice-laptop        # prints a one-time code
codeument relay client add --name web-01 --ttl 1h

# on the client
codeument enroll --relay https://relay.corp:8443 --code XXXX-XXXX-XXXX
sudo codeument enroll --relay https://relay.corp:8443 --code YYYY-YYYY-YYYY --scope system   # servers: root snapshot job
```

Enrollment codes are single-use and expire (24h by default). Tokens are
stored hashed on the relay and can be revoked with
`codeument relay client revoke NAME`.

## What the relay pushes to clients (`client_defaults`)

- hints for the summarizer (naming conventions, glossary)
- default page locations for runbooks and server pages
- extra noise commands, weights and redaction patterns
- the credential policy (`reference`, `manager`, `inline`) and reference templates
- a minimum client version (older clients get HTTP 426)

Clients refresh these on every `tick` and merge them under their local config.

## LLM choice

`llm.provider: anthropic` uses the Claude API with the relay's key.
`llm.provider: ollama` uses an Ollama server next to the relay (the
docker-compose file bundles one); clients then reach a company model without
any data leaving the network. The relay can be switched between the two at
any time; clients do not change.

## Audit log

`codeument relay audit` shows who called which endpoint, when, with how many
events and tokens. Command payloads are not stored unless
`audit.store_payloads: true`.
