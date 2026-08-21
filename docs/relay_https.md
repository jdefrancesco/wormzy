## HTTPS reverse proxy for the Wormzy mailbox (control plane)

The `cmd/mailbox` binary is a thin HTTP façade in front of Redis for rendezvous,
pairing, and stats. It does not handle TLS itself; instead, run it on
`127.0.0.1` and place a reverse proxy such as [Caddy](https://caddyserver.com/)
in front. This keeps the Go service simple while letting the proxy manage
certificates and automatic renewals.

The file data path is separate:
- direct path: UDP/QUIC peer-to-peer
- fallback path: `cmd/relay` on UDP/3478 forwarding encrypted QUIC streams

The official service is trusted for availability and metadata handling, not
for file confidentiality: pairing secrets are generated on the clients and
file bytes remain end-to-end encrypted. An operator of a custom endpoint can
observe client IP addresses, timing, candidate metadata, and transfer activity,
and can delay, drop, or reorder control traffic. Only use a custom endpoint
when that metadata/availability tradeoff is acceptable. Keep Redis private;
production clients must use the HTTPS mailbox API rather than direct Redis
credentials.
Mailbox rate-limit keys use SHA-256 identifiers so raw client addresses do not
appear in Redis keys, but this is pseudonymization rather than IP anonymity: an
offline Redis snapshot can still be dictionary-matched. Treat those keys and
Redis backups as sensitive operational metadata.

Remote mailbox endpoints must use HTTPS. Plain HTTP is accepted only for
loopback development addresses such as `127.0.0.1` or `localhost`; clients
reject plaintext HTTP to any remote host before sending capabilities or
candidate metadata. Direct Redis mailbox connections are also restricted to
loopback addresses or local Unix sockets, so production clients cannot bypass
the HTTPS control plane with Redis credentials.

Current deployments require only the v2 HTTPS mailbox and UDP relay. Remove or
disable any legacy `wormzy-rendezvous` systemd unit and do not expose its TCP
port 9999; `make deploy` performs that retirement automatically.

Store `WORMZY_MAILBOX_REDIS` in `/etc/wormzy/mailbox.env` with restricted file
permissions. The mailbox reads it directly from its environment so credentials
do not appear in `ExecStart`, process arguments, startup logs, or systemd status.

Mailbox capability authentication and opaque session routing are a breaking
protocol revision, as are the direct peer-key-confirmation protocol and the
relay's dedicated v2 ALPN. Deploy the updated
mailbox and UDP relay before distributing updated clients. New clients fail
closed against an old service instead of silently downgrading, and old clients
will stop working when the v1 service is retired. Treat this as a coordinated
cutover rather than a backward-compatible rolling upgrade.

The mailbox and relay publish process heartbeats and activity counters to Redis for the operator console. Configure the relay with `WORMZY_RELAY_REDIS`, `WORMZY_METRICS_REDIS`, or the same `WORMZY_MAILBOX_REDIS` used by the mailbox. The Redis endpoint remains private; no public administration route is added.

The mailbox admits at most 2,048 active sessions and at most 256 KiB of queued
messages per session, bounding queued payload data to roughly 512 MiB plus
Redis/JSON overhead. Size Redis with additional headroom, monitor memory, and
use a `noeviction` policy so memory pressure fails a request instead of silently
discarding an active pairing session.

Relay pairs close after five minutes without application bytes and have a
12-hour absolute lifetime. Operators can tune these with the `relay`
binary's `--pair-idle` and `--pair-lifetime` flags (or the corresponding
systemd environment values). These limits bound idle occupancy; because
the public relay intentionally accepts opaque PAKE-derived registrations, use
host/provider traffic shaping and monitoring to control sustained bandwidth
abuse.

### 1. Bind the mailbox locally

The systemd unit under `deploy/systemd/wormzy-mailbox.service` now defaults to
`127.0.0.1:9200`. Update `/etc/wormzy/mailbox.env` if you prefer a different
port, then reload:

```bash
sudo systemctl daemon-reload
sudo systemctl restart wormzy-mailbox
```

### 2. Configure Caddy (example)

Install Caddy (`sudo apt install caddy`) and create `/etc/caddy/Caddyfile`
similar to:

```
mailbox.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:9200
    header /healthz * {
        Cache-Control "no-store"
    }
}
```

Reload Caddy (`sudo systemctl reload caddy`). Caddy issues/renews Let's Encrypt
certificates automatically, so `wormzy` clients can use
`https://mailbox.example.com` as their mailbox endpoint.

### 3. Health checks

`cmd/wormzy info` probes the mailbox endpoint by requesting `/healthz`. The mailbox
HTTP server exposes this endpoint and simply proxies a Redis `PING`, so you can
also point external monitoring at `https://mailbox.example.com/healthz`.

### 4. Point clients at HTTPS

Set `WORMZY_RELAY_URL=https://mailbox.example.com` on client hosts (or bake it
into your environment). Direct transfers still use UDP/QUIC. If direct race
fails, clients can use the QUIC relay fallback (`cmd/relay`) on UDP/3478.

Clients may additionally pass `--relay-pin` with standard padded base64 of
`SHA-256(RawSubjectPublicKeyInfo)` for the HTTPS mailbox certificate. The pin is
checked only after the normal CA-chain and hostname verification succeeds, so
it cannot make an otherwise invalid certificate trusted. Remote HTTP endpoints
are always rejected; Redis and other non-HTTPS endpoints are rejected when a
pin is present. Coordinate pin changes
with TLS key rotation or clients carrying the old pin will fail closed. Publish
the pin through a trusted channel independent of the connection it protects.
