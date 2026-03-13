#!/usr/bin/env bash
set -euo pipefail

# Installs the freshly built relay binaries (mailbox + rendezvous) into /usr/local/bin
# and restarts their systemd units if present. Run this on the droplet after `make build`.

BIN_DIR="/usr/local/bin"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NEEDED_BINS=("mailbox" "rendezvous")

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root (sudo ./hack/install-relay.sh)" >&2
  exit 1
fi

for bin in "${NEEDED_BINS[@]}"; do
  src="${PROJECT_ROOT}/${bin}"
  dest="${BIN_DIR}/${bin}"
  if [[ ! -x "${src}" ]]; then
    echo "error: ${src} not found or not executable. Run 'make build' first." >&2
    exit 1
  fi
  install -m 0755 "${src}" "${dest}"
  echo "Installed ${dest}"
done

systemctl daemon-reload

for unit in wormzy-mailbox.service wormzy-rendezvous.service; do
  if systemctl list-unit-files | grep -q "^${unit}"; then
    systemctl restart "${unit}"
    systemctl status --no-pager "${unit}" || true
  fi
done

echo "Relay components updated."
