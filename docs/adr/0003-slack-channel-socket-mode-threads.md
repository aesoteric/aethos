# Slack Channel: Socket Mode, threads in one channel, opaque Topic keys

The Slack Channel connects over Socket Mode (an outbound websocket opened with an app-level token) and maps Sessions onto threads in a single operator-configured Slack channel: each Session is a thread rooted at a bot-posted message that carries the Session's name and state, and top-level messages in the channel are the Assistant surface. Absorbing this without per-channel schema growth generalizes the Session↔Topic binding from a Telegram-shaped `int64` to an opaque string key that only the owning Channel can interpret (Telegram: forum topic ID digits; Slack: the root message `ts`).

## Why

- **Socket Mode over the Events API.** The Events API requires a public HTTPS endpoint — a reverse proxy, a certificate, and a reachability contract — which is exactly the ops burden the single-binary thesis rejects. Socket Mode is outbound-only, mirroring the Telegram Channel's long-polling. Its one real limitation (Socket Mode apps cannot be distributed on the Slack Marketplace) is irrelevant to a self-hosted bridge.
- **Threads in one channel over channel-per-Session or DMs.** A channel per Session needs `channels:manage`, pollutes the workspace channel list, and Slack's archive/unarchive semantics fight the live/dormant/closed Session lifecycle. DM threads fight the reserved multi-user future. One configured channel mirrors `telegram.chat_id`, and a thread per Session mirrors a Topic per Session.
- **Opaque Topic keys over per-channel binding fields.** Slack thread identity is a string (`thread_ts`), so `TopicID int64` cannot hold it. A nullable field per channel would grow the Session record forever; an opaque owner-scoped key keeps one concept and one column for any future Channel.
- **Hand-rolled client over `slack-go/slack`.** The Channel needs roughly five Web API endpoints; the Telegram client is already hand-rolled over `net/http`. The only new dependency is `github.com/coder/websocket` for the socket itself.

## Consequences

- Slash-prefixed commands cannot exist on Slack (the client swallows them), so the Assistant speaks bare keywords (`new`, `sessions`, `close <id>`) and in-thread controls are Block Kit buttons (Cancel on the streaming draft, Approve/Deny on permission requests).
- Channel config sections become opt-in: a present section is validated fail-closed in full, and at least one Channel must be configured. Slack-only deployments are now possible.
- The sessions table migrates `topic_id` from `INTEGER` to an owner-scoped `TEXT` topic key; Topic lookup is scoped by owning Channel.
- Supersedes the "Slack deferred by decision" consequence of ADR 0001.
