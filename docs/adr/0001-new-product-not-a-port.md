# Aethos is a new product, not a port of OpenACP

This repo contains `openacp-main/` (gitignored, ~100k lines of TypeScript) — it is reference material, not a porting target. Aethos is a ground-up Go product in the same category (self-hosted bridge from ACP coding agents to messaging platforms) with a different thesis: **better ops** — one static binary, one data directory, trivial systemd/Docker deployment. We deliberately carry over *no* compatibility with OpenACP: not its `~/.openacp` config/data formats, not its npm plugin ecosystem, not its feature checklist, not its date-based versioning.

## Why

A literal port would fight Go at every layer that made OpenACP what it is (dynamic npm plugin loading, ESM module graph, runtime config migration chains) and would inherit a backward-compatibility contract that only benefits users aethos doesn't have. Treating OpenACP as a well-documented spec — its adapter seams, session model, permission gate, and test conventions are all proven — gives us its lessons without its constraints.

## Consequences

- Feature gaps vs OpenACP are deliberate scope, not TODO items (voice, tunnels, usage tracking, web UI, Discord/Slack are all deferred by decision).
- Multi-user support is a reserved future direction: every session records an owner identity from day one, even while single-user.
- If code is ever translated directly from `openacp-main/` (MIT) rather than independently implemented, its copyright notice must be preserved in a NOTICE file.
