#!/usr/bin/env bash
set -Eeuo pipefail

# setup-nat-lab.sh
#
# One-command Linux network-namespace NAT lab for testing Wormzy NAT punching.
#
# Topology:
#
#   nsA (10.0.0.2) -- natA --+
#                            +-- inet (100.64.0.0/24)
#   nsB (10.0.1.2) -- natB --+
#
# natA lives in namespace natA, natB in namespace natB.
# inet can host tcpdump, STUN, relay, etc.
#
# Modes:
#   cone       basic MASQUERADE NAT on both sides
#   symmetric  randomized SNAT port ranges on both sides
#
# Usage:
#   sudo ./setup-nat-lab.sh up
#   sudo ./setup-nat-lab.sh up --mode symmetric
#   sudo ./setup-nat-lab.sh status
#   sudo ./setup-nat-lab.sh shell nsA
#   sudo ./setup-nat-lab.sh shell nsB
#   sudo ./setup-nat-lab.sh exec nsA ip a
#   sudo ./setup-nat-lab.sh down
#
# Typical Wormzy test:
#   sudo ./setup-nat-lab.sh up --mode cone
#   sudo ip netns exec nsA ./wormzy send ./file.bin
#   sudo ip netns exec nsB ./wormzy recv
#
# Optional: run tcpdump in inet namespace
#   sudo ip netns exec inet tcpdump -i any -nn udp
#
# Optional: run relay / mailbox / STUN in inet namespace
#   sudo ip netns exec inet <command>

LAB_TAG="wormzy-natlab"

# Namespaces
NSA="nsA"
NSB="nsB"
NATA="natA"
NATB="natB"
INET="inet"

# Links
A_IN="veth-a-in"
A_NAT="veth-a-nat"
A_WAN="veth-a-wan"
A_INET="veth-a-inet"

B_IN="veth-b-in"
B_NAT="veth-b-nat"
B_WAN="veth-b-wan"
B_INET="veth-b-inet"

# Addressing
NSA_IP="10.0.0.2/24"
NSA_GW="10.0.0.1"
NATA_LAN_IP="10.0.0.1/24"
NATA_WAN_IP="100.64.0.10/24"

NSB_IP="10.0.1.2/24"
NSB_GW="10.0.1.1"
NATB_LAN_IP="10.0.1.1/24"
NATB_WAN_IP="100.64.0.20/24"

INET_A_IP="100.64.0.1/24"
INET_B_IP="100.64.0.2/24"

MODE="cone"

usage() {
  cat <<'EOF'
Usage:
  setup-nat-lab.sh <command> [options]

Commands:
  up                  Create the NAT lab
  down                Tear down the NAT lab
  status              Show namespace / route / NAT status
  shell <ns>          Open a shell inside a namespace
  exec <ns> <cmd...>  Run a command inside a namespace

Options for "up":
  --mode cone|symmetric   NAT mode (default: cone)

Examples:
  sudo ./setup-nat-lab.sh up
  sudo ./setup-nat-lab.sh up --mode symmetric
  sudo ./setup-nat-lab.sh shell nsA
  sudo ./setup-nat-lab.sh exec inet tcpdump -i any -nn udp
  sudo ./setup-nat-lab.sh down

Namespaces created:
  nsA, natA, nsB, natB, inet
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

cleanup_link() {
  local ns="$1"
  local link="$2"
  if ns_exists "$ns"; then
    ip netns exec "$ns" ip link del "$link" 2>/dev/null || true
  fi
}

delete_ns() {
  if ns_exists "$1"; then
    ip netns del "$1" 2>/dev/null || true
  fi
}

create_ns() {
  local ns="$1"
  if ! ns_exists "$ns"; then
    ip netns add "$ns"
  fi
}

set_lo_up() {
  ip netns exec "$1" ip link set lo up
}

label_link() {
  local ns="$1"
  local link="$2"
  ip netns exec "$ns" ip link set dev "$link" alias "$LAB_TAG" 2>/dev/null || true
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

  label_link "$NSA" "$A_IN"
  label_link "$NATA" "$A_NAT"
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

  label_link "$NSB" "$B_IN"
  label_link "$NATB" "$B_NAT"
}

setup_natA_wan() {
  ip link add "$A_WAN" type veth peer name "$A_INET"
  ip link set "$A_WAN" netns "$NATA"
  ip link set "$A_INET" netns "$INET"

  ip netns exec "$NATA" ip addr add "$NATA_WAN_IP" dev "$A_WAN"
  ip netns exec "$NATA" ip link set "$A_WAN" up
  ip netns exec "$NATA" ip route add default via "${INET_A_IP%/*}"

  ip netns exec "$INET" ip addr add "$INET_A_IP" dev "$A_INET"
  ip netns exec "$INET" ip link set "$A_INET" up

  label_link "$NATA" "$A_WAN"
  label_link "$INET" "$A_INET"
}

setup_natB_wan() {
  ip link add "$B_WAN" type veth peer name "$B_INET"
  ip link set "$B_WAN" netns "$NATB"
  ip link set "$B_INET" netns "$INET"

  ip netns exec "$NATB" ip addr add "$NATB_WAN_IP" dev "$B_WAN"
  ip netns exec "$NATB" ip link set "$B_WAN" up
  ip netns exec "$NATB" ip route add default via "${INET_B_IP%/*}"

  ip netns exec "$INET" ip addr add "$INET_B_IP" dev "$B_INET"
  ip netns exec "$INET" ip link set "$B_INET" up

  label_link "$NATB" "$B_WAN"
  label_link "$INET" "$B_INET"
}

enable_forwarding() {
  ip netns exec "$NATA" sysctl -w net.ipv4.ip_forward=1 >/dev/null
  ip netns exec "$NATB" sysctl -w net.ipv4.ip_forward=1 >/dev/null
  ip netns exec "$INET" sysctl -w net.ipv4.ip_forward=1 >/dev/null
}

setup_filtering() {
  # Accept forwarding on the NATs
  ip netns exec "$NATA" iptables -P FORWARD DROP
  ip netns exec "$NATA" iptables -A FORWARD -i "$A_NAT" -o "$A_WAN" -j ACCEPT
  ip netns exec "$NATA" iptables -A FORWARD -i "$A_WAN" -o "$A_NAT" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

  ip netns exec "$NATB" iptables -P FORWARD DROP
  ip netns exec "$NATB" iptables -A FORWARD -i "$B_NAT" -o "$B_WAN" -j ACCEPT
  ip netns exec "$NATB" iptables -A FORWARD -i "$B_WAN" -o "$B_NAT" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

  # inet is the "public internet"; let traffic pass freely there
  ip netns exec "$INET" iptables -P FORWARD ACCEPT
}

setup_nat_cone() {
  ip netns exec "$NATA" iptables -t nat -A POSTROUTING -s 10.0.0.0/24 -o "$A_WAN" -j MASQUERADE
  ip netns exec "$NATB" iptables -t nat -A POSTROUTING -s 10.0.1.0/24 -o "$B_WAN" -j MASQUERADE
}

setup_nat_symmetric() {
  ip netns exec "$NATA" iptables -t nat -A POSTROUTING -s 10.0.0.0/24 -o "$A_WAN" \
    -j SNAT --to-source 100.64.0.10:40000-50000 --random
  ip netns exec "$NATB" iptables -t nat -A POSTROUTING -s 10.0.1.0/24 -o "$B_WAN" \
    -j SNAT --to-source 100.64.0.20:40000-50000 --random
}

setup_lab() {
  need_root
  need_cmd ip
  need_cmd iptables
  need_cmd sysctl
  need_cmd awk
  need_cmd grep

  teardown_lab >/dev/null 2>&1 || true

  create_ns "$NSA"
  create_ns "$NSB"
  create_ns "$NATA"
  create_ns "$NATB"
  create_ns "$INET"

  set_lo_up "$NSA"
  set_lo_up "$NSB"
  set_lo_up "$NATA"
  set_lo_up "$NATB"
  set_lo_up "$INET"

  setup_nsA
  setup_nsB
  setup_natA_wan
  setup_natB_wan
  enable_forwarding
  setup_filtering

  case "$MODE" in
    cone) setup_nat_cone ;;
    symmetric) setup_nat_symmetric ;;
    *)
      err "Unsupported mode: $MODE"
      exit 1
      ;;
  esac

  log "NAT lab is up."
  echo
  echo "Namespaces:"
  echo "  $NSA   client A"
  echo "  $NATA  NAT A"
  echo "  $NSB   client B"
  echo "  $NATB  NAT B"
  echo "  $INET  internet / relay / STUN"
  echo
  echo "Addresses:"
  echo "  $NSA -> ${NSA_IP%/*} via $NSA_GW"
  echo "  $NSB -> ${NSB_IP%/*} via $NSB_GW"
  echo "  $NATA WAN -> ${NATA_WAN_IP%/*}"
  echo "  $NATB WAN -> ${NATB_WAN_IP%/*}"
  echo "  $INET side A -> ${INET_A_IP%/*}"
  echo "  $INET side B -> ${INET_B_IP%/*}"
  echo
  echo "Mode: $MODE"
  echo
  echo "Useful commands:"
  echo "  sudo ip netns exec $NSA ip a"
  echo "  sudo ip netns exec $NSB ip a"
  echo "  sudo ip netns exec $INET tcpdump -i any -nn udp"
  echo "  sudo ip netns exec $NSA ping -c 2 ${INET_A_IP%/*}"
  echo "  sudo ip netns exec $NSB ping -c 2 ${INET_B_IP%/*}"
  echo
  echo "Typical Wormzy test:"
  echo "  sudo ip netns exec $NSA ./wormzy send ./file.bin"
  echo "  sudo ip netns exec $NSB ./wormzy recv"
}

teardown_lab() {
  need_root
  cleanup_link "$NSA" "$A_IN"
  cleanup_link "$NSB" "$B_IN"
  cleanup_link "$NATA" "$A_NAT"
  cleanup_link "$NATA" "$A_WAN"
  cleanup_link "$NATB" "$B_NAT"
  cleanup_link "$NATB" "$B_WAN"
  cleanup_link "$INET" "$A_INET"
  cleanup_link "$INET" "$B_INET"

  delete_ns "$NSA"
  delete_ns "$NSB"
  delete_ns "$NATA"
  delete_ns "$NATB"
  delete_ns "$INET"

  log "NAT lab removed."
}

status_lab() {
  need_root
  for ns in "$NSA" "$NATA" "$NSB" "$NATB" "$INET"; do
    if ns_exists "$ns"; then
      echo "===== $ns ====="
      ip netns exec "$ns" ip -br addr
      echo
      ip netns exec "$ns" ip route || true
      echo
      ip netns exec "$ns" iptables -S 2>/dev/null || true
      echo
      ip netns exec "$ns" iptables -t nat -S 2>/dev/null || true
      echo
    else
      echo "===== $ns ====="
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
    teardown_lab
    ;;
  status)
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
