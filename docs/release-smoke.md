# Release smoke checklist

Run this checklist against the published artifacts for every release. It is not
complete when only a source checkout or snapshot passes. Record the exact tag,
commit, hosts, and Agent below so another operator can reproduce the result.

## Execution record

- Release tag: `v0.1.0`
- Commit from `aethos version`: `a4b6c34`
- Date and tester: 2026-07-17, Liam manual smoke and Codex automated
  publication smoke
- Binary host and architecture: macOS 26.5.2, arm64
- Docker host and architecture: Docker Desktop on macOS 26.5.2, Linux arm64
- systemd host and distribution: Parallels VM, Ubuntu 24.04.3 LTS, arm64
- ACP Agent ID and version: `opencode` 1.18.3
- Telegram bot and forum group (non-secret identifiers only): `@aesoteric_bot`,
  `-1004460169089`
- Slack workspace, app, and channel (non-secret identifiers only): not exercised
  in v0.1.0; record these for the first release containing the Slack Channel

## Published artifacts

- [x] The GitHub Release contains Linux and macOS archives for amd64 and arm64,
      plus `checksums.txt`.
- [x] `sha256sum -c checksums.txt` (or `shasum -a 256 -c checksums.txt`) passes
      for every downloaded archive.
- [x] The archive for the binary host contains `aethos`, `README.md`, and
      `LICENSE`; `aethos version` reports the release version and tagged commit.
- [x] `ghcr.io/aesoteric/aethos:<version>` resolves to amd64 and arm64 images,
      and the stable release is also available as `latest`.

## Real Telegram and Agent

The Agent must be configured to emit ACP permission requests. OpenCode permits
tools by default, so its smoke Workspace used this `opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "edit": "ask"
  }
}
```

- [x] Create a fresh data directory, install the recorded Agent, and complete
      the wizard with a real bot token, forum supergroup, allowlisted user, and
      Workspace.
- [x] Confirm the Assistant Topic appears and posts startup status.
- [x] Send `/new` in Assistant, then send a first Prompt in the new Topic.
- [x] Confirm the real Agent starts, the Session receives streamed output, and
      the Prompt finishes without protocol errors.
- [x] Trigger a tool permission, approve or deny it in Telegram, and confirm the
      Agent receives that choice.
- [x] Restart aethos, send another Prompt in the same Topic, and confirm the
      dormant Session resumes its Agent context.

## Real Slack and Agent

The recorded v0.1.0 run predates the Slack Channel. These checks are the release
gate for the first Slack-capable release and remain required for every later
release.

- [ ] From a clean Slack app, follow the manifest walkthrough in
      [deployment.md](deployment.md#slack-channel); confirm Socket Mode, both
      channel message events, interactivity, and only the documented bot scopes
      are enabled.
- [ ] Generate the `connections:write` app-level token, install the app to obtain
      its bot token, invite the bot user to the chosen Slack channel, and record
      the non-secret workspace, app, channel, and allowlisted user IDs above.
- [ ] Create a fresh aethos data directory, install the recorded Agent, and run
      the wizard for a Slack-only deployment using environment-sourced tokens.
      Confirm the generated `config.toml` contains neither token.
- [ ] Confirm aethos connects through Socket Mode without a public endpoint and
      `agents` at the top level receives the installed Agent list from the
      Assistant.
- [ ] Send `new`, confirm a Session Topic appears, then send a first Prompt in
      that Topic. Confirm the Agent starts, streams output, and reports the
      Prompt's terminal result there.
- [ ] Start another Prompt, press Cancel before it finishes, and confirm the
      Agent receives cancellation and the stale control disappears.
- [ ] Trigger an Agent permission request, approve or deny it with the Slack
      buttons, and confirm the Agent receives that choice and the buttons are
      replaced by the outcome.
- [ ] Confirm `sessions` lists the Session with state, Agent, name, and ID; then
      use `close <Session ID>` and confirm the closed Session rejects a plain
      Prompt in its Topic.
- [ ] From a Slack user absent from `allowed_user_ids`, try the Assistant, a
      Session Prompt, and a button press; confirm aethos produces no reaction.
- [ ] Restart aethos, send a Prompt in a dormant Session Topic, and confirm the
      same Agent context resumes.

## One-volume Docker deployment

- [x] Follow the Docker commands in [deployment.md](deployment.md) from a clean
      host directory, using exactly one bind mount at `/data`.
- [x] Complete the wizard and first Session using the published image, not a
      locally built image.
- [x] Recreate the container with the same mount; confirm configuration, Agent
      authentication, Workspace files, and Session history remain available.
- [x] Confirm the container process is nonroot and no second writable mount is
      needed.

## Real systemd host

- [x] Follow the systemd commands in [deployment.md](deployment.md) on the
      recorded host using the downloaded release binary.
- [x] `systemd-analyze verify /etc/systemd/system/aethos.service` succeeds.
- [x] `systemctl enable --now aethos` reaches `active (running)` and logs appear
      in `journalctl -u aethos`.
- [x] Create and use a real Telegram Session while the service owns the process.
- [x] `systemctl restart aethos` succeeds and the same Session resumes.
- [x] Stop the service and confirm no aethos or Agent subprocess remains.

## Release result

- [x] GitHub Actions [release run](https://github.com/aesoteric/aethos/actions/runs/29585298144)
      and [GitHub Release](https://github.com/aesoteric/aethos/releases/tag/v0.1.0).
- [x] Automated and manual checks passed. OpenCode permits tools by default, so
      the smoke Workspace used `permission.edit = "ask"` in `opencode.json` to
      exercise Telegram approval. The distroless image has no shell as
      documented, so file operations used OpenCode's `write` and `read` tools.
      `systemd-analyze` emitted only an unrelated Parallels Tools warning about
      its legacy `/var/run` PID path.
- [x] Mark the release accepted only after every box above is checked.
