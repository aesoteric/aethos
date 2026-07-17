# Release smoke checklist

Run this checklist against the published artifacts for every release. It is not
complete when only a source checkout or snapshot passes. Record the exact tag,
commit, hosts, and Agent below so another operator can reproduce the result.

## Execution record

- Release tag: `v0.1.0`
- Commit from `aethos version`: `a4b6c34`
- Date and tester: 2026-07-17, Codex automated publication smoke
- Binary host and architecture: macOS 26.5.2, arm64
- Docker host and architecture: Docker Desktop, Linux arm64
- systemd host and distribution: pending real-host smoke
- ACP Agent ID and version: `opencode` 1.18.3 (installation and execution verified)
- Telegram bot and forum group (non-secret identifiers only): pending real-session smoke

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

- [ ] Create a fresh data directory, install the recorded Agent, and complete
      the wizard with a real bot token, forum supergroup, allowlisted user, and
      Workspace.
- [ ] Confirm the Assistant Topic appears and posts startup status.
- [ ] Send `/new` in Assistant, then send a first Prompt in the new Topic.
- [ ] Confirm the real Agent starts, the Session receives streamed output, and
      the Prompt finishes without protocol errors.
- [ ] Trigger a tool permission, approve or deny it in Telegram, and confirm the
      Agent receives that choice.
- [ ] Restart aethos, send another Prompt in the same Topic, and confirm the
      dormant Session resumes its Agent context.

## One-volume Docker deployment

- [ ] Follow the Docker commands in [deployment.md](deployment.md) from a clean
      host directory, using exactly one bind mount at `/data`.
- [ ] Complete the wizard and first Session using the published image, not a
      locally built image.
- [ ] Recreate the container with the same mount; confirm configuration, Agent
      authentication, Workspace files, and Session history remain available.
- [ ] Confirm the container process is nonroot and no second writable mount is
      needed.

## Real systemd host

- [ ] Follow the systemd commands in [deployment.md](deployment.md) on the
      recorded host using the downloaded release binary.
- [ ] `systemd-analyze verify /etc/systemd/system/aethos.service` succeeds.
- [ ] `systemctl enable --now aethos` reaches `active (running)` and logs appear
      in `journalctl -u aethos`.
- [ ] Create and use a real Telegram Session while the service owns the process.
- [ ] `systemctl restart aethos` succeeds and the same Session resumes.
- [ ] Stop the service and confirm no aethos or Agent subprocess remains.

## Release result

- [x] GitHub Actions [release run](https://github.com/aesoteric/aethos/actions/runs/29585298144)
      and [GitHub Release](https://github.com/aesoteric/aethos/releases/tag/v0.1.0).
- [x] Automated artifact checks passed. The published image installed and ran
      `opencode` 1.18.3 from a clean, single bind mount and retained it across
      container recreation. The real Telegram Session, full one-volume wizard
      and Session recreation, and real systemd-host checks remain pending.
- [ ] Mark the release accepted only after every box above is checked.
