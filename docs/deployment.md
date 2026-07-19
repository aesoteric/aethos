# Deployment

Aethos needs outbound HTTPS access to each configured Channel provider, the ACP
Agent registry, and the chosen Agent's provider. Keep its data directory and the
Workspace on storage you back up. The SQLite database, configuration, installed
Agent, Agent state, and Workspace are the only persistent state in the examples
below.

## Slack Channel

The Slack Channel uses Socket Mode, so Slack connects to aethos over an outbound
WebSocket and no public HTTP endpoint is required. One configured Slack channel
hosts the Assistant at its top level and one Session Topic, implemented as a
Slack thread, per Session.

### Create the Slack app

1. Open [Slack app settings](https://api.slack.com/apps), select **Create New
   App**, choose **From a manifest**, and select the workspace where aethos will
   run. Slack's [manifest guide](https://docs.slack.dev/app-manifests/configuring-apps-with-app-manifests/)
   describes the same creation flow.
2. Select JSON, paste this manifest, review the requested features and scopes,
   then create the app:

```json
{
  "_metadata": {
    "major_version": 1
  },
  "display_information": {
    "name": "aethos",
    "description": "Self-hosted bridge from Slack to ACP coding Agents"
  },
  "features": {
    "bot_user": {
      "display_name": "aethos",
      "always_online": false
    }
  },
  "oauth_config": {
    "scopes": {
      "bot": [
        "channels:history",
        "chat:write",
        "groups:history"
      ]
    }
  },
  "settings": {
    "event_subscriptions": {
      "bot_events": [
        "message.channels",
        "message.groups"
      ]
    },
    "interactivity": {
      "is_enabled": true
    },
    "org_deploy_enabled": false,
    "socket_mode_enabled": true,
    "token_rotation_enabled": false
  }
}
```

The history scopes and matching events let aethos receive Prompts from public
and private Slack channels; `chat:write` lets it post and update its own output.
See Slack's references for [`message.channels`](https://docs.slack.dev/reference/events/message.channels/),
[`message.groups`](https://docs.slack.dev/reference/events/message.groups/),
and [`chat:write`](https://docs.slack.dev/reference/scopes/chat.write/).
Interactivity delivers Cancel and permission button presses over the same
Socket Mode connection.

### Collect the credentials and IDs

1. On the app's **Basic Information** page, find **App-Level Tokens**, select
   **Generate Token and Scopes**, add `connections:write`, and generate the
   token. Save the `xapp-...` value as the app-level token. Slack documents this
   token as the credential for
   [`apps.connections.open`](https://docs.slack.dev/reference/scopes/connections.write/).
2. Open **OAuth & Permissions**, select **Install to Workspace**, approve the
   requested scopes, and copy the **Bot User OAuth Token** beginning with
   `xoxb-`. Slack's [Socket Mode setup](https://docs.slack.dev/apis/events-api/using-socket-mode/)
   distinguishes these two tokens and requires no Request URL.
3. Create or choose the Slack channel aethos will use, then invite the app's bot
   user with `/invite @aethos`. Membership lets the app read and write there
   without the broader `chat:write.public` scope.
4. Right-click the Slack channel name, select **View channel details**, and copy
   the channel ID at the bottom. It begins with `C` or `G`; it is also the last
   segment of the [channel URL](https://docs.slack.dev/messaging/formatting-message-text/#linking-to-channels).
5. For every person allowed to use aethos, open their Slack profile, select
   **More**, then **Copy member ID**. Slack user IDs normally begin with `U` or
   `W`. Only these IDs will be allowed to use the Assistant, send Prompts, or
   press Session controls.

Treat both tokens as passwords. The recommended setup keeps them out of
`config.toml`:

```sh
export AETHOS_SLACK_APP_TOKEN='xapp-your-app-level-token'
export AETHOS_SLACK_BOT_TOKEN='xoxb-your-bot-token'
```

For Docker, put the same `NAME=value` entries in a mode-`0600` file and pass it
with `--env-file` to both the wizard and background container. For systemd, use
`/etc/aethos/aethos.env` as described below.

### Configure and run aethos

Install an Agent before the first start, then run the wizard:

```sh
aethos agents
aethos agents install codex-acp
aethos
```

Authenticate the installed Agent according to its own documentation before
creating a Session.

Choose **Slack**, enter the channel ID and comma-separated allowed user IDs,
then select the default Agent and Workspace. The wizard validates both Slack
tokens live. When they came from the environment, it writes empty token values
so the secrets remain outside the file. The resulting Slack section has this
shape:

```toml
workspace = "/absolute/path/to/workspace"
default_agent = "codex-acp"

[slack]
app_token = ""
bot_token = ""
channel_id = "C0123456789"
allowed_user_ids = ["U0123456789", "U9876543210"]
```

Start aethos with the same token environment. At the top level of the configured
Slack channel, use `agents`, `new`, `sessions`, or `close <Session ID>` with the
Assistant. `new` creates a Session Topic; send the first Prompt as a reply in
that Topic. Agent output and lifecycle updates appear there, with Cancel and
permission controls when applicable.

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
exercises startup, Telegram and Slack delivery, restart, and Session durability
on real hosts before a release is accepted.
