# Compiled-in modules, no runtime plugin system

OpenACP's defining architectural bet is "everything is a plugin, installed from npm at runtime." Aethos keeps the *discipline* (features live behind explicit seams: the `Channel` interface for user-facing heads, `Module` seams for internal features) but drops dynamic loading entirely: every channel and module is compiled into the single binary. Third-party extension means a PR, a fork, or a custom build — not a package install.

## Why

The product thesis is single-binary ops; every honest mechanism for runtime extensibility in Go undermines it. We considered:

- **`hashicorp/go-plugin` subprocess plugins** — real extensibility, but adds versioned RPC contracts, process supervision, and a second failure domain before the product has a single user.
- **WASM plugins (extism)** — sandboxed, but the ecosystem is immature and long-lived I/O (a Telegram websocket) fits WASM badly.
- **`buildmode=plugin`** — rejected outright: platform-fragile and version-locked.

Compiled-in modules cost nothing now and keep the subprocess option open: if third-party demand ever materializes, the existing seams are exactly where an RPC boundary would go.

## Consequences

- The words **plugin** and **adapter** are banned from the codebase; OpenACP's "plugin" concept splits into **channels** (user-facing) and **modules** (internal) — see `CONTEXT.md`.
- Adding Discord/Slack later means implementing the `Channel` interface in-repo, not shipping a package.
- There is no plugin SDK, plugin registry, plugin settings tree, or plugin lifecycle/permission system to build or maintain.
