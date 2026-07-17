# Release smoke checklist

Run this checklist against the published artifacts for every release. It is not
complete when only a source checkout or snapshot passes. Record the exact tag,
commit, hosts, and Agent below so another operator can reproduce the result.

## Execution record

- Release tag:
- Commit from `aethos version`:
- Date and tester:
- Binary host and architecture:
- Docker host and architecture:
- systemd host and distribution:
- ACP Agent ID and version:
- Telegram bot and forum group (non-secret identifiers only):

## Published artifacts

- [ ] The GitHub Release contains Linux and macOS archives for amd64 and arm64,
      plus `checksums.txt`.
- [ ] `sha256sum -c checksums.txt` (or `shasum -a 256 -c checksums.txt`) passes
      for every downloaded archive.
- [ ] The archive for the binary host contains `aethos`, `README.md`, and
      `LICENSE`; `aethos version` reports the release version and tagged commit.
- [ ] `ghcr.io/aesoteric/aethos:<version>` resolves to amd64 and arm64 images,
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

- [ ] Paste links to the GitHub Actions release run and GitHub Release here.
- [ ] Record any deviations or follow-up issues here.
- [ ] Mark the release accepted only after every box above is checked.
