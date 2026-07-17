# aethos

A self-hosted bridge that connects AI coding agents — Claude Code, Codex, Gemini, and anything else that speaks the [Agent Client Protocol](https://agentclientprotocol.com) — to messaging platforms. Written in Go; ships as **one static binary** with **one data directory**.

[![CI](https://github.com/aesoteric/aethos/actions/workflows/ci.yml/badge.svg)](https://github.com/aesoteric/aethos/actions/workflows/ci.yml)

> **Current release: v0.1.0.** Release archives are available for Linux and
> macOS on amd64 and arm64, together with a multi-architecture distroless image.
> The [v1 spec](https://github.com/aesoteric/aethos/issues/1) tracks what comes next.

## Why

Coding agents are locked to terminal REPLs and IDE integrations. aethos lets you drive them from your phone or from automation: create a Session, watch output stream in, approve or deny risky actions — while the operational surface stays boring: download one binary, run it under systemd or Docker, back up one directory.

## Quickstart: first Telegram Session

Before starting, create a Telegram bot with BotFather and a private supergroup
with Topics enabled. Add the bot as an administrator allowed to manage Topics.
You also need your negative group ID and the positive numeric Telegram user IDs
that may use aethos.

Download the archive for the current machine and verify its checksum:

```sh
version=0.1.0
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
archive="aethos_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/aesoteric/aethos/releases/download/v${version}"
curl -fLO "${base_url}/${archive}"
curl -fLO "${base_url}/checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  grep " ${archive}$" checksums.txt | sha256sum -c -
else
  grep " ${archive}$" checksums.txt | shasum -a 256 -c -
fi
tar -xzf "$archive"
./aethos version
sudo install -m 0755 aethos /usr/local/bin/aethos
```

List the ACP registry, choose an Agent whose runtime is installed on the host,
and install it. `codex-acp` is an `npx` example and therefore requires Node.js:

```sh
aethos agents
aethos agents install codex-acp
```

Authenticate the chosen Agent according to its own documentation, then start
aethos:

```sh
aethos
```

The first-run wizard validates the Telegram bot token and asks for the forum
group, allowlisted users, Workspace, default Agent, and REST bearer token. At
the end it writes `~/.aethos/config.toml` and starts the Telegram Channel.

In Telegram, open the Assistant Topic and send `/new`. Aethos creates a Session
Topic; open it and send the first Prompt. The Agent's streamed output and any
permission buttons appear in that Topic.

For Docker, systemd, upgrades, and backup paths, see
[Deployment](docs/deployment.md). Release operators use the
[release smoke checklist](docs/release-smoke.md) with real Telegram and Agent
credentials before accepting a release.

## What v0.1.0 includes

- **Telegram**: each Session lives in its own forum Topic; agent output (thinking, tool calls, text) streams in as it happens; risky actions pause on approve/deny buttons.
- **REST/SSE**: automation clients create Sessions, send Prompts, and stream events with a bearer token.
- **Sessions that survive restarts**: state in a single SQLite database; live Sessions demote to dormant on idle and auto-resume on the next Prompt.
- **One config file**: commented TOML written by a first-run wizard, with env-var overrides for secrets.
- **Installable Agents**: browse the ACP registry from the CLI and install `npx` or platform-native binary Agents without replacing aethos.

## Design

- Go, cgo-free, statically cross-compiled.
- Channels (user-facing: Telegram, REST/SSE) and Modules (internal features) are compiled in behind explicit seams — no runtime plugin system ([ADR-0002](docs/adr/0002-compiled-in-modules-no-plugin-system.md)).
- The ACP SDK is quarantined in a single translation package; everything else consumes aethos-owned event types.
- A new product in OpenACP's category, not a port ([ADR-0001](docs/adr/0001-new-product-not-a-port.md)).

The domain glossary lives in [CONTEXT.md](CONTEXT.md); code and review use its vocabulary exactly.

## Agents

Browse the official ACP registry, then install an Agent by its registry ID:

```sh
aethos agents
aethos agents install codex-acp
```

Use `-data-dir` on either command, or `AETHOS_DATA_DIR`, to target a non-default
data directory. Installed metadata is written to `agents.json`; binary Agents
are downloaded beneath `agents/`. `npx` entries are pinned to the registry
package version and downloaded by `npx` when first launched.

Registry access is needed only while listing or installing. Startup and Session
creation resolve installed Agents from the local catalog, including Agents
installed by another CLI process while aethos is already running. A registry
outage therefore does not prevent previously installed Agents from launching.

Binary downloads are selected for the current operating system and CPU. aethos
verifies a published SHA-256 checksum when present and supports raw binaries,
`.zip`, `.tar.gz`, `.tgz`, `.tar.bz2`, and `.tbz2` archives.

## Configuration

Install at least one Agent, then run `aethos` with no command. If `config.toml`
does not exist, the first-run wizard validates the Telegram bot token with
Telegram, collects the forum supergroup and allowlisted user IDs and a
Workspace, offers the installed Agents for the default, then writes a commented
configuration file and starts the Telegram Channel. Later starts load that file
without prompting. `default_agent` stores an installed registry ID, not a shell
command.

The Telegram group must be a supergroup with Topics enabled. Add the bot as an
administrator with permission to manage Topics, then give the wizard the
negative group ID (usually beginning with `-100`) and the numeric Telegram user
IDs that may use aethos. Messages from every other user are rejected and logged.

At startup, aethos creates or reuses the Assistant Topic and posts its status.
Send `/agents` there to see the installed Agent IDs available for Session
creation. Send `/new` to use the configured defaults, or
`/new /absolute/workspace | agent-id` to choose both. The new Session gets
its own Topic; its first Prompt becomes the Session and Topic name. Plain
messages sent to General are redirected to Assistant.

Send `/sessions` in Assistant to list every Session with its lifecycle state,
Agent, name, and ID. Archive one deliberately with `/close <Session ID>`; closed
Sessions remain listed and cannot be resumed by a plain Prompt. Send `/cancel`
inside a Session Topic to stop its current Prompt without closing the Session.

The data directory defaults to `~/.aethos/`. Override it with
`aethos -data-dir /path/to/data` or `AETHOS_DATA_DIR`; configuration, database,
Agent catalog, installed binaries, and log paths are all rooted there.
Environment values override the file:

- `AETHOS_TELEGRAM_BOT_TOKEN` (keeps the token out of `config.toml`)
- `AETHOS_REST_BEARER_TOKEN` (keeps the REST Channel token out of `config.toml`)
- `AETHOS_REST_LISTEN_ADDRESS` (overrides the default `127.0.0.1:8080` socket)
- `AETHOS_WORKSPACE`
- `AETHOS_DEFAULT_AGENT` (an installed Agent registry ID)

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
for Approve/Deny buttons in the Session Topic. Answered and timed-out requests
replace those buttons with their outcome; unanswered requests deny fail-safe.

Invalid TOML, unknown fields, and missing required values stop startup with an
actionable error.

## REST automation

The REST Channel listens on `127.0.0.1:8080` by default. `GET /health` is public
and reports whether Session control is ready. Every other route requires
`Authorization: Bearer <token>` using `[rest].bearer_token` or
`AETHOS_REST_BEARER_TOKEN`:

- `POST /sessions` with `{"agent":"...","workspace":"/absolute/path"}`
- `GET /agents` (installed Agent choices for `POST /sessions`)
- `GET /sessions`
- `GET /sessions/{id}`
- `GET /sessions/{id}/events` (SSE)
- `POST /sessions/{id}/prompt` with `{"prompt":"..."}`
- `POST /sessions/{id}/cancel`
- `POST /permissions/{request-id}` with `{"option_id":"..."}`

REST-created Sessions record `rest/api` as their owner. Prompt requests wait
for the Agent turn to finish and return its stop reason. Failures use JSON error
bodies with 400 (validation), 401 (authentication), 404 (unknown Session), 409
(conflicting Session state), or 500 (internal failure), never a successful
status for failed work.

The event stream is scoped to one Session and uses named SSE events. It emits
`prompt_started` and `prompt_finished`, `thought` and `message` output,
`tool_call_began` and `tool_call_progressed`, `permission_requested` and
`permission_resolved`, `session_state_changed`, and `crashed`. Permission
request data includes the request ID, tool description and input, and every
Agent-provided option; send the selected option ID to the permission endpoint.
Repeated answers to an already completed request are successful and harmless.

SSE delivery is live-only in v1. A reconnect receives events published after
the new connection is established; events missed while disconnected are not
replayed. A `session_state_changed` event with `{"state":"closed"}` is the final
event when a Session is deliberately closed, after which the server ends the
stream cleanly.

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
