#!/usr/bin/env bash
set -Eeuo pipefail

# setup-nat-lab-uplink.sh
#
# One-command NAT lab for Wormzy where BOTH clients are behind separate NATs
# but still have outbound access to the REAL Internet via the host uplink.
#
# Topology:
#
#   nsA (10.10.0.2) -- natA (10.10.0.1 / 172.31.0.2) --+
#                                                       |
#                                                    host bridge br-wormzy (172.31.0.1)
#                                                       |
#   nsB (10.20.0.2) -- natB (10.20.0.1 / 172.31.0.3) --+
#                                                       |
#                                                     host uplink (eth0 / wlan0 / ...)
#                                                       |
#                                                  real Internet / relay.wormzy.io
#
# NAT A and NAT B are independent Linux namespaces.
# The host does final egress NAT to the real uplink so nsA and nsB can reach:
#   - relay.wormzy.io
#   - public STUN servers
#   - anything else on the real Internet
#
# Modes:
#   cone       MASQUERADE on natA/natB
#   symmetric  randomized SNAT port ranges on natA/natB
#
# Usage:
#   sudo ./setup-nat-lab-uplink.sh up --uplink eth0
#   sudo ./setup-nat-lab-uplink.sh up --uplink wlan0 --mode symmetric
#   sudo ./setup-nat-lab-uplink.sh status
#   sudo ./setup-nat-lab-uplink.sh shell nsA
#   sudo ./setup-nat-lab-uplink.sh exec nsA ./wormzy send ./file.bin
#   sudo ./setup-nat-lab-uplink.sh down
#
# Notes:
#   - Linux only
#   - Requires iproute2 + iptables
#   - The host's FORWARD policy may be adjusted while the lab is active
#   - This script tags host-side iptables rules with a unique comment for cleanup

LAB_TAG="wormzy-natlab-uplink"
BRIDGE="br-wormzy"
UPLINK=""

NSA="nsA"
NSB="nsB"
NATA="natA"
NATB="natB"

A_IN="veth-a-in"
A_NAT="veth-a-nat"
A_HOST="veth-a-host"
A_WAN="veth-a-wan"

B_IN="veth-b-in"
B_NAT="veth-b-nat"
B_HOST="veth-b-host"
B_WAN="veth-b-wan"

NSA_IP="10.10.0.2/24"
NSA_GW="10.10.0.1"
NATA_LAN_IP="10.10.0.1/24"
NATA_WAN_IP="172.31.0.2/24"

NSB_IP="10.20.0.2/24"
NSB_GW="10.20.0.1"
NATB_LAN_IP="10.20.0.1/24"
NATB_WAN_IP="172.31.0.3/24"

HOST_BRIDGE_IP="172.31.0.1/24"
HOST_NET_CIDR="172.31.0.0/24"

MODE="cone"

usage() {
  cat <<'EOF'
Usage:
  setup-nat-lab-uplink.sh <command> [options]

Commands:
  up                     Create the NAT lab with real Internet uplink
  down                   Tear down the NAT lab
  status                 Show namespace / route / NAT status
  shell <ns>             Open a shell inside a namespace
  exec <ns> <cmd...>     Run a command inside a namespace

Options for "up":
  --uplink IFACE         REQUIRED host uplink interface (example: eth0, wlan0, enp3s0)
  --mode cone|symmetric  NAT mode for natA/natB (default: cone)

Examples:
  sudo ./setup-nat-lab-uplink.sh up --uplink eth0
  sudo ./setup-nat-lab-uplink.sh up --uplink wlan0 --mode symmetric
  sudo ./setup-nat-lab-uplink.sh shell nsA
  sudo ./setup-nat-lab-uplink.sh exec nsA curl -I https://relay.wormzy.io
  sudo ./setup-nat-lab-uplink.sh exec nsA ./wormzy send ./file.bin
  sudo ./setup-nat-lab-uplink.sh exec nsB ./wormzy recv
  sudo ./setup-nat-lab-uplink.sh down

Namespaces created:
  nsA, natA, nsB, natB
EOF
}

log() { echo "[+] $*"; }
err() { echo "[-] $*" >&2; }

need_root() {
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    err "Run as root (sudo)."
    exit 1
  fi
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    err "Missing required command: $1"
    exit 1
  }
}

ns_exists() {
  ip netns list | awk '{print $1}' | grep -Fxq "$1"
}

bridge_exists() {
  ip link show "$1" >/dev/null 2>&1
}

create_ns() {
  local ns="$1"
  if ! ns_exists "$ns"; then
    ip netns add "$ns"
  fi
}

delete_ns() {
  if ns_exists "$1"; then
    ip netns del "$1" 2>/dev/null || true
  fi
}

set_lo_up() {
  ip netns exec "$1" ip link set lo up
}

delete_host_link() {
  ip link del "$1" 2>/dev/null || true
}

cleanup_bridge() {
  if bridge_exists "$BRIDGE"; then
    ip link set "$BRIDGE" down 2>/dev/null || true
    ip link del "$BRIDGE" type bridge 2>/dev/null || true
  fi
}

enable_forwarding() {
  sysctl -w net.ipv4.ip_forward=1 >/dev/null
  ip netns exec "$NATA" sysctl -w net.ipv4.ip_forward=1 >/dev/null
  ip netns exec "$NATB" sysctl -w net.ipv4.ip_forward=1 >/dev/null
}

host_rule_exists() {
  local table="$1"
  shift
  iptables ${table:+-t "$table"} -C "$@" >/dev/null 2>&1
}

add_host_rule() {
  local table="$1"
  shift
  if ! iptables ${table:+-t "$table"} -C "$@" >/dev/null 2>&1; then
    iptables ${table:+-t "$table"} -A "$@"
  fi
}

del_host_rule() {
  local table="$1"
  shift
  while iptables ${table:+-t "$table"} -C "$@" >/dev/null 2>&1; do
    iptables ${table:+-t "$table"} -D "$@"
  done
}

create_bridge() {
  ip link add name "$BRIDGE" type bridge
  ip addr add "$HOST_BRIDGE_IP" dev "$BRIDGE"
  ip link set "$BRIDGE" up
}

setup_nsA() {
  ip link add "$A_IN" type veth peer name "$A_NAT"
  ip link set "$A_IN" netns "$NSA"
  ip link set "$A_NAT" netns "$NATA"

  ip netns exec "$NSA" ip addr add "$NSA_IP" dev "$A_IN"
  ip netns exec "$NSA" ip link set "$A_IN" up
  ip netns exec "$NSA" ip route add default via "$NSA_GW"

  ip netns exec "$NATA" ip addr add "$NATA_LAN_IP" dev "$A_NAT"
  ip netns exec "$NATA" ip link set "$A_NAT" up
}

setup_nsB() {
  ip link add "$B_IN" type veth peer name "$B_NAT"
  ip link set "$B_IN" netns "$NSB"
  ip link set "$B_NAT" netns "$NATB"

  ip netns exec "$NSB" ip addr add "$NSB_IP" dev "$B_IN"
  ip netns exec "$NSB" ip link set "$B_IN" up
  ip netns exec "$NSB" ip route add default via "$NSB_GW"

  ip netns exec "$NATB" ip addr add "$NATB_LAN_IP" dev "$B_NAT"
  ip netns exec "$NATB" ip link set "$B_NAT" up
}

setup_natA_uplink() {
  ip link add "$A_HOST" type veth peer name "$A_WAN"
  ip link set "$A_WAN" netns "$NATA"

  ip link set "$A_HOST" master "$BRIDGE"
  ip link set "$A_HOST" up

  ip netns exec "$NATA" ip addr add "$NATA_WAN_IP" dev "$A_WAN"
  ip netns exec "$NATA" ip link set "$A_WAN" up
  ip netns exec "$NATA" ip route add default via "${HOST_BRIDGE_IP%/*}"
}

setup_natB_uplink() {
  ip link add "$B_HOST" type veth peer name "$B_WAN"
  ip link set "$B_WAN" netns "$NATB"

  ip link set "$B_HOST" master "$BRIDGE"
  ip link set "$B_HOST" up

  ip netns exec "$NATB" ip addr add "$NATB_WAN_IP" dev "$B_WAN"
  ip netns exec "$NATB" ip link set "$B_WAN" up
  ip netns exec "$NATB" ip route add default via "${HOST_BRIDGE_IP%/*}"
}

setup_nat_filters() {
  ip netns exec "$NATA" iptables -P FORWARD DROP
  ip netns exec "$NATA" iptables -A FORWARD -i "$A_NAT" -o "$A_WAN" -j ACCEPT
  ip netns exec "$NATA" iptables -A FORWARD -i "$A_WAN" -o "$A_NAT" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

  ip netns exec "$NATB" iptables -P FORWARD DROP
  ip netns exec "$NATB" iptables -A FORWARD -i "$B_NAT" -o "$B_WAN" -j ACCEPT
  ip netns exec "$NATB" iptables -A FORWARD -i "$B_WAN" -o "$B_NAT" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
}

setup_nat_cone() {
  ip netns exec "$NATA" iptables -t nat -A POSTROUTING -s 10.10.0.0/24 -o "$A_WAN" -j MASQUERADE
  ip netns exec "$NATB" iptables -t nat -A POSTROUTING -s 10.20.0.0/24 -o "$B_WAN" -j MASQUERADE
}

setup_nat_symmetric() {
  ip netns exec "$NATA" iptables -t nat -A POSTROUTING -s 10.10.0.0/24 -o "$A_WAN" \
    -j SNAT --to-source 172.31.0.2:40000-50000 --random
  ip netns exec "$NATB" iptables -t nat -A POSTROUTING -s 10.20.0.0/24 -o "$B_WAN" \
    -j SNAT --to-source 172.31.0.3:50001-60000 --random
}

setup_host_rules() {
  # Forward between bridge and uplink
  add_host_rule "" FORWARD -i "$BRIDGE" -o "$UPLINK" -m comment --comment "$LAB_TAG" -j ACCEPT
  add_host_rule "" FORWARD -i "$UPLINK" -o "$BRIDGE" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$LAB_TAG" -j ACCEPT

  # Final egress NAT from bridge net to real Internet
  add_host_rule "nat" POSTROUTING -s "$HOST_NET_CIDR" -o "$UPLINK" -m comment --comment "$LAB_TAG" -j MASQUERADE
}

remove_host_rules() {
  del_host_rule "nat" POSTROUTING -s "$HOST_NET_CIDR" -o "$UPLINK" -m comment --comment "$LAB_TAG" -j MASQUERADE || true
  del_host_rule "" FORWARD -i "$UPLINK" -o "$BRIDGE" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$LAB_TAG" -j ACCEPT || true
  del_host_rule "" FORWARD -i "$BRIDGE" -o "$UPLINK" -m comment --comment "$LAB_TAG" -j ACCEPT || true
}

detect_uplink() {
  ip route get 1.1.1.1 2>/dev/null | awk '
    {
      for (i=1; i<=NF; i++) {
        if ($i == "dev" && (i+1) <= NF) {
          print $(i+1)
          exit
        }
      }
    }'
}

setup_lab() {
  need_root
  need_cmd ip
  need_cmd iptables
  need_cmd sysctl
  need_cmd awk
  need_cmd grep

  if [[ -z "$UPLINK" ]]; then
    UPLINK="$(detect_uplink || true)"
  fi
  [[ -n "$UPLINK" ]] || { err "Could not auto-detect uplink. Pass --uplink IFACE."; exit 1; }
  ip link show "$UPLINK" >/dev/null 2>&1 || { err "Uplink interface not found: $UPLINK"; exit 1; }

  teardown_lab >/dev/null 2>&1 || true

  create_ns "$NSA"
  create_ns "$NSB"
  create_ns "$NATA"
  create_ns "$NATB"

  set_lo_up "$NSA"
  set_lo_up "$NSB"
  set_lo_up "$NATA"
  set_lo_up "$NATB"

  create_bridge
  setup_nsA
  setup_nsB
  setup_natA_uplink
  setup_natB_uplink
  enable_forwarding
  setup_nat_filters

  case "$MODE" in
    cone) setup_nat_cone ;;
    symmetric) setup_nat_symmetric ;;
    *)
      err "Unsupported mode: $MODE"
      exit 1
      ;;
  esac

  setup_host_rules

  log "NAT uplink lab is up."
  echo
  echo "Namespaces:"
  echo "  $NSA   client A"
  echo "  $NATA  NAT A"
  echo "  $NSB   client B"
  echo "  $NATB  NAT B"
  echo
  echo "Host bridge: $BRIDGE (${HOST_BRIDGE_IP%/*})"
  echo "Host uplink: $UPLINK"
  echo "Mode:        $MODE"
  echo
  echo "Addresses:"
  echo "  $NSA  -> ${NSA_IP%/*} via $NSA_GW"
  echo "  $NATA -> LAN ${NATA_LAN_IP%/*}, WAN ${NATA_WAN_IP%/*}"
  echo "  $NSB  -> ${NSB_IP%/*} via $NSB_GW"
  echo "  $NATB -> LAN ${NATB_LAN_IP%/*}, WAN ${NATB_WAN_IP%/*}"
  echo
  echo "Quick tests:"
  echo "  sudo ip netns exec $NSA ping -c 2 1.1.1.1"
  echo "  sudo ip netns exec $NSB ping -c 2 1.1.1.1"
  echo "  sudo ip netns exec $NSA curl -I https://relay.wormzy.io"
  echo "  sudo ip netns exec $NSB curl -I https://relay.wormzy.io"
  echo
  echo "Wormzy test:"
  echo "  sudo ip netns exec $NSA ./wormzy send ./file.bin"
  echo "  sudo ip netns exec $NSB ./wormzy recv"
  echo
  echo "Observability:"
  echo "  sudo ip netns exec $NATA tcpdump -i any -nn udp"
  echo "  sudo ip netns exec $NATB tcpdump -i any -nn udp"
  echo "  sudo tcpdump -i $UPLINK -nn udp"
}

teardown_lab() {
  need_root
  if [[ -z "$UPLINK" ]]; then
    UPLINK="$(detect_uplink || true)"
  fi

  if [[ -n "$UPLINK" ]] && ip link show "$UPLINK" >/dev/null 2>&1; then
    remove_host_rules || true
  fi

  delete_host_link "$A_HOST"
  delete_host_link "$B_HOST"

  cleanup_bridge

  delete_ns "$NSA"
  delete_ns "$NSB"
  delete_ns "$NATA"
  delete_ns "$NATB"

  log "NAT uplink lab removed."
}

status_lab() {
  need_root
  echo "Host"
  echo "===="
  echo "Uplink: ${UPLINK:-$(detect_uplink || true)}"
  ip -br addr show "$BRIDGE" 2>/dev/null || true
  echo
  ip route | sed -n '1,20p'
  echo
  iptables -S | grep "$LAB_TAG" || true
  echo
  iptables -t nat -S | grep "$LAB_TAG" || true
  echo

  for ns in "$NSA" "$NATA" "$NSB" "$NATB"; do
    echo "===== $ns ====="
    if ns_exists "$ns"; then
      ip netns exec "$ns" ip -br addr
      echo
      ip netns exec "$ns" ip route || true
      echo
      ip netns exec "$ns" iptables -S 2>/dev/null || true
      echo
      ip netns exec "$ns" iptables -t nat -S 2>/dev/null || true
      echo
    else
      echo "not present"
      echo
    fi
  done
}

shell_ns() {
  need_root
  local ns="${1:-}"
  [[ -n "$ns" ]] || { err "Usage: $0 shell <namespace>"; exit 1; }
  ns_exists "$ns" || { err "Namespace not found: $ns"; exit 1; }
  ip netns exec "$ns" bash
}

exec_ns() {
  need_root
  local ns="${1:-}"
  shift || true
  [[ -n "$ns" ]] || { err "Usage: $0 exec <namespace> <command...>"; exit 1; }
  [[ $# -gt 0 ]] || { err "Missing command for exec"; exit 1; }
  ns_exists "$ns" || { err "Namespace not found: $ns"; exit 1; }
  ip netns exec "$ns" "$@"
}

COMMAND="${1:-}"
shift || true

case "$COMMAND" in
  up)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --uplink)
          UPLINK="${2:-}"
          shift 2
          ;;
        --mode)
          MODE="${2:-}"
          shift 2
          ;;
        -h|--help)
          usage
          exit 0
          ;;
        *)
          err "Unknown option for up: $1"
          usage
          exit 1
          ;;
      esac
    done
    setup_lab
    ;;
  down)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --uplink)
          UPLINK="${2:-}"
          shift 2
          ;;
        *)
          err "Unknown option for down: $1"
          usage
          exit 1
          ;;
      esac
    done
    teardown_lab
    ;;
  status)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --uplink)
          UPLINK="${2:-}"
          shift 2
          ;;
        *)
          err "Unknown option for status: $1"
          usage
          exit 1
          ;;
      esac
    done
    status_lab
    ;;
  shell)
    shell_ns "$@"
    ;;
  exec)
    exec_ns "$@"
    ;;
  -h|--help|"")
    usage
    ;;
  *)
    err "Unknown command: $COMMAND"
    usage
    exit 1
    ;;
esac
