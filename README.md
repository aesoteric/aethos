# aethos

A self-hosted bridge that connects AI coding agents — Claude Code, Codex, Gemini, and anything else that speaks the [Agent Client Protocol](https://agentclientprotocol.com) — to messaging platforms. Written in Go; ships as **one static binary** with **one data directory**.

[![CI](https://github.com/aesoteric/aethos/actions/workflows/ci.yml/badge.svg)](https://github.com/aesoteric/aethos/actions/workflows/ci.yml)

> **Status: early development.** No release yet — the walking skeleton (one Prompt through a real agent) has just landed. The [v1 spec](https://github.com/aesoteric/aethos/issues/1) tracks what's coming.

## Why

Coding agents are locked to terminal REPLs and IDE integrations. aethos lets you drive them from your phone or from automation: create a Session, watch output stream in, approve or deny risky actions — while the operational surface stays boring: download one binary, run it under systemd or Docker, back up one directory.

## What v1 will include

- **Telegram**: each Session lives in its own forum Topic; agent output (thinking, tool calls, text) streams in as it happens; risky actions pause on approve/deny buttons.
- **REST/SSE**: automation clients create Sessions, send Prompts, and stream events with a bearer token.
- **Sessions that survive restarts**: state in a single SQLite database; live Sessions demote to dormant on idle and auto-resume on the next Prompt.
- **One config file**: commented TOML written by a first-run wizard, with env-var overrides for secrets.

## Design

- Go, cgo-free, statically cross-compiled.
- Channels (user-facing: Telegram, REST/SSE) and Modules (internal features) are compiled in behind explicit seams — no runtime plugin system ([ADR-0002](docs/adr/0002-compiled-in-modules-no-plugin-system.md)).
- The ACP SDK is quarantined in a single translation package; everything else consumes aethos-owned event types.
- A new product in OpenACP's category, not a port ([ADR-0001](docs/adr/0001-new-product-not-a-port.md)).

The domain glossary lives in [CONTEXT.md](CONTEXT.md); code and review use its vocabulary exactly.

## Configuration

Run `aethos` with no command. If `config.toml` does not exist, the first-run
wizard validates the Telegram bot token with Telegram, collects the forum
supergroup and allowlisted user IDs, a Workspace, and the default Agent command,
then writes a commented configuration file and starts the Telegram Channel.
Later starts load that file without prompting.

The Telegram group must be a supergroup with Topics enabled. Add the bot as an
administrator with permission to manage Topics, then give the wizard the
negative group ID (usually beginning with `-100`) and the numeric Telegram user
IDs that may use aethos. Messages from every other user are rejected and logged.

At startup, aethos creates or reuses the Assistant Topic and posts its status.
Send `/new` there to use the configured defaults, or
`/new /absolute/workspace | agent command` to choose both. The new Session gets
its own Topic; its first Prompt becomes the Session and Topic name. Plain
messages sent to General are redirected to Assistant.

The data directory defaults to `~/.aethos/`. Override it with
`aethos -data-dir /path/to/data` or `AETHOS_DATA_DIR`; configuration, database,
and log paths are all rooted there. Environment values override the file:

- `AETHOS_TELEGRAM_BOT_TOKEN` (keeps the token out of `config.toml`)
- `AETHOS_WORKSPACE`
- `AETHOS_DEFAULT_AGENT`

`idle_timeout` is a human-readable duration such as `"30m"` or `"2h"`. It
defaults to 30 minutes and controls how long a live Session with no Prompt work
keeps its Agent subprocess attached.

The permission gate denies unanswered requests after 10 minutes by default.
Exact Agent-reported tool kinds can be auto-approved in `config.toml`; an empty
list is the safest default and asks the Channel every time:

```toml
[permissions]
timeout = "10m"
auto_approve = ["read", "search"]
```

Auto-approved requests select a one-time allow option when the Agent offers one.
File edits, shell commands, and any tool kind not listed still pause the Prompt
for a Channel response.

Invalid TOML, unknown fields, and missing required values stop startup with an
actionable error.

## Session durability

Session records live in `aethos.db` under the data directory. Each record keeps
the Agent, Workspace, owner identity, lifecycle state, and activity timestamps.
Live Sessions become dormant during shutdown and transparently resume their ACP
Session on the next Prompt after a restart.

Prompts are processed in arrival order within each Session. A running Prompt can
be cancelled without discarding its Session. Agent crashes and idle timeouts
demote live Sessions to dormant and release the subprocess; the next Prompt
resumes them. Explicitly closed Sessions remain listed as archived records and
cannot be resumed by a plain Prompt.

The waiting queue is intentionally in memory: a restart drops Prompts that had
not started, while the durable Session record and Agent context remain available
for the next Prompt.

## Development

Requires Go 1.24+.

```sh
go test -race ./...
go build ./cmd/aethos
```

To push one Prompt through a real, locally installed ACP agent and watch its output stream (a development command; expect it to change):

```sh
./aethos dev prompt -agent "npx @zed-industries/claude-code-acp" -workspace . "say hello"
```

The command persists the Session under the normal data directory and logs its
ID. A later binary invocation resumes the same Agent context:

```sh
./aethos dev prompt -session <session-id> "continue where we left off"
```

Structured JSON logs go to stderr; set `AETHOS_LOG_LEVEL=debug` for protocol-level detail.

## License

[MIT](LICENSE)
