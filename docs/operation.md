# Operation

## Deployment

A merge to `main` runs the CI checks, builds `ghcr.io/vsem-azamat/wa-tg-mirror:<short sha>` and deploys it over SSH with [`deploy/compose.yaml`](../deploy/compose.yaml) into `~/deploy/wa-tg-mirror` on the host. The workflow needs the repository secrets `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY` and `DEPLOY_KNOWN_HOSTS`.

The host directory holds files the workflow never writes:

| File | Contents |
| --- | --- |
| `config.yaml` | The configuration, tokens as `${NAME}`; readable by UID 65532 (distroless `nonroot`) |
| `.env` | The bot tokens, mode `0600`; read by Docker Compose, not by the container user |
| `data/` | The SQLite database and the heartbeat file; owned by UID 65532 |

Prepare them once; the workflow stops when one is missing. Without root on the host, ownership can be set through Docker: `docker run --rm -v "$PWD:/w" alpine chown -R 65532:65532 /w/data /w/config.yaml`.

The deploy user's login shell must understand `$'…'` quoting (bash or zsh, not dash): the workflow passes the remote script that way.

The container publishes no ports, runs read-only without capabilities and needs outgoing HTTPS only.

## Health

While WhatsApp is connected, `serve -heartbeat /data/heartbeat` touches the file every 30 seconds; `healthcheck` fails when it is older than 90 seconds, and Docker marks the container unhealthy. `serve` removes the file at start, so a new container waiting for a QR scan is not taken for healthy. Telegram polling errors (a revoked token, a second poller) appear in the log.

## Logs

`docker compose logs -f` in the deploy directory. Logs carry chat identifiers and errors, never message text or tokens.

## Loading history

WhatsApp hands its chat history to a device only when the device is linked, so `backfill` links the bridge again:

```sh
docker compose --env-file image.env stop
docker compose --env-file image.env run -d --name wa-tg-mirror-backfill wa-tg-mirror \
  backfill -config /config/config.yaml -days 30
docker logs -f wa-tg-mirror-backfill        # until "history loaded"
docker rm wa-tg-mirror-backfill
docker compose --env-file image.env up -d
```

1. It unlinks the current device (WhatsApp then lists it as gone on the phone) and links a new one that asks for the full history of the last days (at most 35).
2. The service bot sends you a pairing code (with `-qr`, QR codes as photos instead, to scan from a second screen such as Telegram on a computer): WhatsApp → Settings → Linked devices → Link a device → Link with phone number instead. Have the phone at hand: a code lives under three minutes, after which a new one is sent, five times at most. If pairing fails for good, the old device is already unlinked: run the command again with `-phone <your number>`.
3. Once the full sync reports completion (or no history arrived for five minutes, at most 30 minutes), the messages of the mirrored chats from the last days are posted oldest first, each with its time and a day marker when the day changes. Messages mirrored before are skipped, so the command can be run again.
4. Messages that arrive while it posts are posted by it too, in later passes, until a pass finds nothing new; then it disconnects, and anything later waits on the WhatsApp server for `serve`.

Telegram allows a bot about 20 messages a minute in a group, so a busy month takes a while. Media WhatsApp no longer keeps arrives as a note.

## Recovery

- **Logged out** (the device was removed in WhatsApp, or the phone stayed offline for 14 days): the bridge notifies the owner and exits; on restart it sends a new QR code through the service bot.
- **Moving the session**: stop the bridge, copy the database file with its `-wal` and `-shm` files, start it elsewhere. Never run two bridges on one session.
