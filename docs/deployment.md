# Deployment

Aethos needs outbound HTTPS access to each configured Channel provider, the ACP
Agent registry, and the chosen Agent's provider. Keep its data directory and the
Workspace on storage you back up. The SQLite database, configuration, installed
Agent, Agent state, and Workspace are the only persistent state in the examples
below.

## Docker

The published image is `ghcr.io/aesoteric/aethos:<version>`. It is a
multi-architecture (`linux/amd64` and `linux/arm64`) distroless Debian 13 image
running as `nonroot`. It has no shell, package manager, Node.js, `npx`, or
`uvx`, so install an Agent with a `binary` distribution. `opencode` is a
registry example that has binaries for both image architectures.

The image keeps all writable state beneath `/data`:

- `/data/aethos` — aethos config, database, and installed Agent catalog
- `/data/home` — the Agent's home directory and authentication state
- `/data/workspace` — the Workspace the Agent reads and writes

Create those directories on the host, then install the Agent. Running with the
host UID and GID lets the nonroot process write the bind mount without changing
its ownership. These commands use exactly one volume mount:

```sh
mkdir -p "$PWD/aethos-data/aethos" "$PWD/aethos-data/home" "$PWD/aethos-data/workspace"

docker run --rm \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$PWD/aethos-data,dst=/data" \
  ghcr.io/aesoteric/aethos:0.1.0 agents install opencode
```

Configure any credentials required by that Agent under `aethos-data/home`, or
pass its provider environment variables to `docker run`. Then run the wizard in
a terminal. Enter `/data/workspace` when it asks for the Workspace:

```sh
docker run --rm -it \
  --name aethos \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$PWD/aethos-data,dst=/data" \
  ghcr.io/aesoteric/aethos:0.1.0
```

After the wizard writes `config.toml`, aethos starts immediately. Stop it with
`Ctrl+C`, then run it in the background with the same single mount:

```sh
docker run -d \
  --name aethos \
  --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$PWD/aethos-data,dst=/data" \
  ghcr.io/aesoteric/aethos:0.1.0
```

Use `docker logs -f aethos` for structured logs. To upgrade, pull a new image,
remove the old container, and recreate it with the same bind mount. Do not
delete `aethos-data`.

The REST Channel binds to loopback by default. To expose it from Docker, set
`AETHOS_REST_LISTEN_ADDRESS=0.0.0.0:8080`, publish port 8080, and protect that
port with an appropriate firewall or reverse proxy.

## systemd

The example unit runs aethos as a dedicated unprivileged user and limits writes
to `/var/lib/aethos`. Its Workspace is deliberately inside that directory so
the Agent can edit it under `ProtectSystem=strict`.

Install the release binary first, then create the user and persistent paths:

```sh
sudo useradd --system --home-dir /var/lib/aethos --shell /usr/sbin/nologin aethos
sudo install -d -o aethos -g aethos -m 0700 \
  /var/lib/aethos/data /var/lib/aethos/home /var/lib/aethos/workspace
sudo install -m 0755 aethos /usr/local/bin/aethos
sudo install -m 0644 deploy/systemd/aethos.service /etc/systemd/system/aethos.service
```

Install an Agent and run the wizard once as the service user. The example uses
a binary Agent so it does not depend on a system Node.js installation:

```sh
sudo -u aethos env \
  HOME=/var/lib/aethos/home \
  AETHOS_DATA_DIR=/var/lib/aethos/data \
  /usr/local/bin/aethos agents install opencode

sudo -u aethos env \
  HOME=/var/lib/aethos/home \
  AETHOS_DATA_DIR=/var/lib/aethos/data \
  /usr/local/bin/aethos
```

Enter `/var/lib/aethos/workspace` for the Workspace. Once the wizard has
written the config and aethos has started, stop it with `Ctrl+C`.

Secrets may instead live in `/etc/aethos/aethos.env`, which the unit loads when
present. Use `NAME=value` lines and restrict the file to root:

```sh
sudo install -d -m 0755 /etc/aethos
sudo install -m 0600 /dev/null /etc/aethos/aethos.env
sudoedit /etc/aethos/aethos.env
```

Supported secret entries include `AETHOS_TELEGRAM_BOT_TOKEN`,
`AETHOS_SLACK_APP_TOKEN`, `AETHOS_SLACK_BOT_TOKEN`,
`AETHOS_REST_BEARER_TOKEN`, and provider variables required by the Agent.

Verify and start the service:

```sh
systemd-analyze verify /etc/systemd/system/aethos.service
sudo systemctl daemon-reload
sudo systemctl enable --now aethos
sudo systemctl status aethos
sudo journalctl -u aethos -f
```

CI verifies the unit with `systemd-analyze` on Ubuntu and the release checklist
exercises startup, Telegram delivery, restart, and Session durability on a real
systemd host before a release is accepted.
