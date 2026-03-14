#!/usr/bin/env bash
# Sets up the Wormzy mailbox service to listen on localhost and configures Caddy
# as an HTTPS reverse proxy in front of it. Run this on the relay host.
set -euo pipefail

MAILBOX_UNIT=/etc/systemd/system/wormzy-mailbox.service
ENV_FILE=/etc/wormzy/mailbox.env
CADDYFILE=/etc/caddy/Caddyfile

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root" >&2
  exit 1
fi

# Ensure env file exists
mkdir -p /etc/wormzy
if [[ ! -f $ENV_FILE ]]; then
  touch $ENV_FILE
  chmod 640 $ENV_FILE
fi

# Install Caddy if missing
if ! command -v caddy >/dev/null 2>&1; then
  apt-get update
  apt-get install -y caddy
fi

cat <<'UNIT' > $MAILBOX_UNIT
[Unit]
Description=Wormzy mailbox proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=wormzy
Group=wormzy
EnvironmentFile=/etc/wormzy/mailbox.env
ExecStart=/usr/local/bin/mailbox -listen 127.0.0.1:9200 -redis "${WORMZY_MAILBOX_REDIS}"
Restart=on-failure
RestartSec=5s
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

cat <<'CADDY' > $CADDYFILE
mailbox.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:9200
    handle_path /healthz {
        reverse_proxy 127.0.0.1:9200
    }
}
CADDY

systemctl daemon-reload
systemctl enable --now wormzy-mailbox
systemctl reload caddy || systemctl restart caddy

echo "Reverse proxy setup complete. Update mailbox.example.com DNS and set WORMZY_RELAY_URL=https://mailbox.example.com"
