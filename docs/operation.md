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

## Recovery

- **Logged out** (the device was removed in WhatsApp, or the phone stayed offline for 14 days): the bridge notifies the owner and exits; on restart it sends a new QR code through the service bot.
- **Moving the session**: stop the bridge, copy the database file with its `-wal` and `-shm` files, start it elsewhere. Never run two bridges on one session.
