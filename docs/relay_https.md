## HTTPS reverse proxy for the Wormzy mailbox

The `cmd/mailbox` binary is a thin HTTP façade in front of Redis. It does not
handle TLS itself; instead, run it on `127.0.0.1` and place a reverse proxy
such as [Caddy](https://caddyserver.com/) in front. This keeps the Go service
simple while letting the proxy manage certificates and automatic renewals.

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
`https://mailbox.example.com` as their relay.

### 3. Health checks

`cmd/wormzy info` now probes the relay by requesting `/healthz`. The mailbox
HTTP server exposes this endpoint and simply proxies a Redis `PING`, so you can
also point external monitoring at `https://mailbox.example.com/healthz`.

### 4. Point clients at HTTPS

Set `WORMZY_RELAY_URL=https://mailbox.example.com` on client hosts (or bake it
into your environment). They will continue to fall back to peer-to-peer
transfers, but rendezvous and relay fallback now speak HTTPS over the proxy.
