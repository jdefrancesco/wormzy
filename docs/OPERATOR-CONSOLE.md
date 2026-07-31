# Wormzy Operator Console

The `dashboard` binary is a privileged terminal console for observing and controlling a Wormzy deployment. It combines three distinct sources of information:

- mailbox process heartbeats and HTTP request activity;
- UDP/QUIC relay connections, pairs, forwarded bytes, and errors;
- client-reported transfer outcomes stored in short-lived rendezvous sessions.

## Start the console

Build the binaries and provide the production Redis URL:

```bash
make build
WORMZY_METRICS_REDIS='rediss://user:password@redis.example:6379' ./bin/dashboard
```

The console also accepts `-redis`, `-prefix`, and `-refresh` flags. Redis credentials are the authorization boundary for operator actions. Do not expose the Redis service publicly, embed its URL in source control, or share an operator terminal with untrusted users.

## Server telemetry

The mailbox publishes telemetry automatically through its configured `WORMZY_MAILBOX_REDIS` connection. The relay uses the first configured value from:

1. `WORMZY_RELAY_REDIS`
2. `WORMZY_METRICS_REDIS`
3. `WORMZY_MAILBOX_REDIS`

The packaged `wormzy-relay.service` loads `/etc/wormzy/mailbox.env` when present so both services can use the same Redis deployment. A missing service heartbeat is displayed as `no heartbeat`; it can mean the process is down, the new binary is not deployed, or relay telemetry has not been configured.

Service counters cover the current process lifetime. Transfer telemetry covers the current Redis session-TTL window and is not a durable all-time total.

## Controls

| Key | Action |
| --- | --- |
| `j` / `k` | Select an unresolved session |
| `x` | Stage removal of the selected rendezvous session |
| `d` | Stage drain/resume of new sessions |
| `y` / `n` | Confirm or cancel the staged action |
| `r` | Refresh immediately |
| `v` | Toggle compact/verbose display |
| `h` | Show help |
| `q` | Quit |

Drain mode prevents new senders from creating sessions. Receivers may still join codes that were issued before draining, and established transfers are left alone. Drain state persists in Redis until an operator resumes intake.

Removing a session interrupts clients that still depend on mailbox rendezvous. It cannot terminate an already-established direct P2P connection because that traffic bypasses the server. Relay connections already carrying data are also not terminated by session removal.

## Interpreting activity

- `Live / unresolved` means the session has no final client report. It does not prove that a client process is still connected.
- `P2P` and `Relay` results are reported by clients after transfer completion.
- Relay connection and byte counters come from the UDP relay itself.
- `TTL left` is read from Redis, so it reflects the key's actual remaining lifetime.

For host-level diagnostics, continue to use systemd logs alongside the console:

```bash
sudo systemctl status wormzy-mailbox wormzy-relay
sudo journalctl -u wormzy-mailbox -u wormzy-relay -f
```
