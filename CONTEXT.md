# Aethos

A self-hosted bridge, written in Go, that connects AI coding agents (via the Agent Client Protocol) to messaging platforms. Ships as a single static binary; uses OpenACP (TypeScript) as reference material, not as a compatibility target.

## Language

**Channel**:
A user-facing head through which people or machines talk to aethos — e.g. Telegram, REST/SSE. Compiled into the binary.
_Avoid_: adapter, plugin, platform, head

**Module**:
An internal feature compiled into the binary behind an explicit seam — e.g. permission gate, access control. Not runtime-installable.
_Avoid_: plugin, service, extension

**Agent**:
An ACP-compatible coding agent (Claude Code, Codex, Gemini, …) that aethos spawns as a subprocess and drives on the user's behalf.
_Avoid_: bot, assistant, model

**Session**:
One conversation between a user and one agent, bound to a workspace and (on Telegram) a topic. Prompts within a session are strictly serial. Either live or dormant; never auto-deleted.
_Avoid_: chat, thread, conversation

**Live** / **Dormant**:
The two session states. Live: an agent subprocess is attached. Dormant: only the persisted record exists; the next prompt auto-resumes it. Idle timeout demotes live → dormant.
_Avoid_: active/expired, running/dead

**Workspace**:
The directory an agent reads and writes for a session.
_Avoid_: project, working directory, repo

**Topic**:
The Telegram forum thread bound to a session — the user-visible face of that session.
_Avoid_: thread, channel

**Assistant**:
The reserved Telegram topic for meta-conversation with aethos itself (creating sessions, status). Bound to no session.

**Prompt**:
One user message dispatched to a session's agent. Queued serially per session; queued prompts do not survive a restart.
_Avoid_: message, request, task
