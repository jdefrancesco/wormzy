#!/usr/bin/env bash
set -Eeuo pipefail

# p2p-rate.sh
#
# Run repeated send/recv trials and report how often Wormzy lands on direct P2P
# vs relay fallback. Useful for NAT-lab tuning.

usage() {
  cat <<'USAGE'
Usage:
  scripts/p2p-rate.sh [options]

Options:
  --trials N            Number of transfer attempts (default: 20)
  --payload-kib N       Payload size in KiB (default: 64)
  --wormzy PATH         Wormzy binary path (default: ./bin/wormzy)
  --relay URL           Relay/mailbox endpoint (default: https://relay.wormzy.io)
  --turn URLS           Optional TURN list passed to wormzy --turn
  --send-ns NAME        Sender network namespace (optional)
  --recv-ns NAME        Receiver network namespace (optional)
  --trial-timeout SEC   Timeout per sender/receiver process (default: 90)
  --code-timeout SEC    Time to wait for sender to emit pairing code (default: 20)
  --workdir DIR         Output directory (default: mktemp)
  --keep                Keep workdir even on success
  -h, --help            Show this help

Examples:
  scripts/p2p-rate.sh --trials 30

  scripts/p2p-rate.sh \
    --trials 40 \
    --send-ns nsA \
    --recv-ns nsB \
    --relay https://relay.wormzy.io
USAGE
}

TRIALS=20
PAYLOAD_KIB=64
WORMZY_BIN="./bin/wormzy"
RELAY="https://relay.wormzy.io"
TURN_URLS=""
SEND_NS=""
RECV_NS=""
TRIAL_TIMEOUT_S=90
CODE_TIMEOUT_S=20
WORKDIR=""
KEEP_WORKDIR=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --trials)
      TRIALS="$2"
      shift 2
      ;;
    --payload-kib)
      PAYLOAD_KIB="$2"
      shift 2
      ;;
    --wormzy)
      WORMZY_BIN="$2"
      shift 2
      ;;
    --relay)
      RELAY="$2"
      shift 2
      ;;
    --turn)
      TURN_URLS="$2"
      shift 2
      ;;
    --send-ns)
      SEND_NS="$2"
      shift 2
      ;;
    --recv-ns)
      RECV_NS="$2"
      shift 2
      ;;
    --trial-timeout)
      TRIAL_TIMEOUT_S="$2"
      shift 2
      ;;
    --code-timeout)
      CODE_TIMEOUT_S="$2"
      shift 2
      ;;
    --workdir)
      WORKDIR="$2"
      shift 2
      ;;
    --keep)
      KEEP_WORKDIR=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown option $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if ! [[ "$TRIALS" =~ ^[0-9]+$ ]] || [[ "$TRIALS" -le 0 ]]; then
  echo "error: --trials must be a positive integer" >&2
  exit 2
fi
if ! [[ "$PAYLOAD_KIB" =~ ^[0-9]+$ ]] || [[ "$PAYLOAD_KIB" -le 0 ]]; then
  echo "error: --payload-kib must be a positive integer" >&2
  exit 2
fi
if ! [[ "$TRIAL_TIMEOUT_S" =~ ^[0-9]+$ ]] || [[ "$TRIAL_TIMEOUT_S" -le 0 ]]; then
  echo "error: --trial-timeout must be a positive integer" >&2
  exit 2
fi
if ! [[ "$CODE_TIMEOUT_S" =~ ^[0-9]+$ ]] || [[ "$CODE_TIMEOUT_S" -le 0 ]]; then
  echo "error: --code-timeout must be a positive integer" >&2
  exit 2
fi
if [[ -n "$SEND_NS" || -n "$RECV_NS" ]]; then
  if [[ -z "$SEND_NS" || -z "$RECV_NS" ]]; then
    echo "error: provide both --send-ns and --recv-ns" >&2
    exit 2
  fi
  command -v ip >/dev/null 2>&1 || { echo "error: ip command not found" >&2; exit 1; }
fi
if [[ ! -x "$WORMZY_BIN" ]]; then
  echo "error: wormzy binary not executable: $WORMZY_BIN" >&2
  echo "hint: run make build" >&2
  exit 1
fi

if [[ -z "$WORKDIR" ]]; then
  WORKDIR="$(mktemp -d -t wormzy-p2p-rate-XXXXXX)"
else
  mkdir -p "$WORKDIR"
fi

PAYLOAD_FILE="$WORKDIR/payload.bin"
RESULTS_CSV="$WORKDIR/results.csv"

cleanup() {
  if [[ "${send_pid:-}" != "" ]]; then
    kill "$send_pid" >/dev/null 2>&1 || true
  fi
  if [[ "${recv_pid:-}" != "" ]]; then
    kill "$recv_pid" >/dev/null 2>&1 || true
  fi
  if [[ "$KEEP_WORKDIR" -eq 0 ]]; then
    rm -rf "$WORKDIR"
  fi
}
trap cleanup EXIT INT TERM

run_in_ns() {
  local ns="$1"
  shift
  if [[ -n "$ns" ]]; then
    ip netns exec "$ns" "$@"
  else
    "$@"
  fi
}

wait_for_transfer_done() {
  local send_log="$1"
  local recv_log="$2"
  local timeout_s="$3"
  local deadline=$((SECONDS + timeout_s))

  while ((SECONDS < deadline)); do
    if grep -q "STAGE transfer done" "$send_log" 2>/dev/null && grep -q "STAGE transfer done" "$recv_log" 2>/dev/null; then
      return 0
    fi
    if grep -Eq " STAGE (stun|rendezvous|quic|noise|transfer) error " "$send_log" "$recv_log" 2>/dev/null; then
      return 1
    fi
    sleep 0.2
  done
  return 124
}

stop_pid() {
  local pid="$1"
  if kill -0 "$pid" >/dev/null 2>&1; then
    kill "$pid" >/dev/null 2>&1 || true
    sleep 0.2
    kill -9 "$pid" >/dev/null 2>&1 || true
  fi
  wait "$pid" >/dev/null 2>&1 || true
}

extract_code() {
  local out_file="$1"
  local line
  line="$(grep -m1 -E "rendezvous assigned code " "$out_file" 2>/dev/null || true)"
  if [[ -z "$line" ]]; then
    return 1
  fi
  echo "${line##*rendezvous assigned code }"
}

wait_for_code() {
  local out_file="$1"
  local timeout_s="$2"
  local deadline=$((SECONDS + timeout_s))
  while ((SECONDS < deadline)); do
    local code
    if code="$(extract_code "$out_file")"; then
      if [[ -n "$code" ]]; then
        echo "$code"
        return 0
      fi
    fi
    sleep 0.1
  done
  return 1
}

detect_path() {
  local send_out="$1"
  local recv_out="$2"
  local send_log="$3"
  local recv_log="$4"

  # Headless output is not guaranteed to print the final "Path:" line yet,
  # so classify from detailed transport logs as the primary signal.
  if grep -Eiq "direct race outcome=won details=(ice-p2p|local|reflexive|loopback)@" "$send_log" "$recv_log"; then
    echo "p2p"
    return 0
  fi
  if grep -Eiq "STAGE quic done relay fallback|direct race outcome=.*details=.*(relay|ice-relay)@" "$send_log" "$recv_log"; then
    echo "relay"
    return 0
  fi
  # Fallback to stdout parsing if available.
  if grep -Eiq "Path:[[:space:]]*P2P" "$send_out" "$recv_out"; then
    echo "p2p"
    return 0
  fi
  if grep -Eiq "Path:[[:space:]]*RELAY" "$send_out" "$recv_out"; then
    echo "relay"
    return 0
  fi
  echo "unknown"
}

pct() {
  local n="$1"
  local d="$2"
  if [[ "$d" -eq 0 ]]; then
    printf "0.00"
    return
  fi
  awk -v n="$n" -v d="$d" 'BEGIN { printf "%.2f", (n * 100.0) / d }'
}

echo "[p2p-rate] workdir: $WORKDIR"
echo "[p2p-rate] trials: $TRIALS"
echo "[p2p-rate] relay: $RELAY"
if [[ -n "$SEND_NS" ]]; then
  echo "[p2p-rate] sender namespace: $SEND_NS"
  echo "[p2p-rate] receiver namespace: $RECV_NS"
fi

# Keep payload deterministic size but random contents.
dd if=/dev/urandom of="$PAYLOAD_FILE" bs=1024 count="$PAYLOAD_KIB" status=none

printf "trial,result,path,code,send_exit,recv_exit,trial_dir\n" > "$RESULTS_CSV"

p2p_count=0
relay_count=0
fail_count=0

for ((i = 1; i <= TRIALS; i++)); do
  trial_id="$(printf "%03d" "$i")"
  trial_dir="$WORKDIR/trial-$trial_id"
  mkdir -p "$trial_dir/recv"

  send_out="$trial_dir/send.out"
  recv_out="$trial_dir/recv.out"
  send_log="$trial_dir/send.log"
  recv_log="$trial_dir/recv.log"

  send_pid=""
  recv_pid=""

  send_cmd=("$WORMZY_BIN" send "$PAYLOAD_FILE" -relay "$RELAY" -log-file "$send_log")
  recv_cmd_base=("$WORMZY_BIN" recv -relay "$RELAY" -download-dir "$trial_dir/recv" -log-file "$recv_log")
  if [[ -n "$TURN_URLS" ]]; then
    send_cmd+=( -turn "$TURN_URLS" )
    recv_cmd_base+=( -turn "$TURN_URLS" )
  fi

  run_in_ns "$SEND_NS" "${send_cmd[@]}" >"$send_out" 2>&1 &
  send_pid=$!

  code=""
  if ! code="$(wait_for_code "$send_out" "$CODE_TIMEOUT_S")"; then
    echo "[trial $trial_id] FAIL: sender did not emit pairing code within ${CODE_TIMEOUT_S}s"
    kill "$send_pid" >/dev/null 2>&1 || true
    wait "$send_pid" >/dev/null 2>&1 || true
    fail_count=$((fail_count + 1))
    printf "%s,fail,unknown,,124,124,%s\n" "$trial_id" "$trial_dir" >> "$RESULTS_CSV"
    continue
  fi

  recv_cmd=("${recv_cmd_base[@]}" "$code")
  run_in_ns "$RECV_NS" "${recv_cmd[@]}" >"$recv_out" 2>&1 &
  recv_pid=$!

  completion_exit=0
  if ! wait_for_transfer_done "$send_log" "$recv_log" "$TRIAL_TIMEOUT_S"; then
    completion_exit=$?
  fi
  stop_pid "$send_pid"
  stop_pid "$recv_pid"
  # Processes are force-stopped after we captured the needed telemetry.
  send_exit="$completion_exit"
  recv_exit="$completion_exit"

  path="$(detect_path "$send_out" "$recv_out" "$send_log" "$recv_log")"

  result="pass"
  if [[ "$completion_exit" -ne 0 ]]; then
    result="fail"
  fi

  case "$path" in
    p2p)
      if [[ "$result" == "pass" ]]; then
        p2p_count=$((p2p_count + 1))
        echo "[trial $trial_id] PASS path=P2P code=$code"
      else
        fail_count=$((fail_count + 1))
        echo "[trial $trial_id] FAIL path=P2P code=$code send_exit=$send_exit recv_exit=$recv_exit"
      fi
      ;;
    relay)
      if [[ "$result" == "pass" ]]; then
        relay_count=$((relay_count + 1))
        echo "[trial $trial_id] PASS path=RELAY code=$code"
      else
        fail_count=$((fail_count + 1))
        echo "[trial $trial_id] FAIL path=RELAY code=$code send_exit=$send_exit recv_exit=$recv_exit"
      fi
      ;;
    *)
      fail_count=$((fail_count + 1))
      echo "[trial $trial_id] FAIL path=UNKNOWN code=$code send_exit=$send_exit recv_exit=$recv_exit"
      ;;
  esac

  printf "%s,%s,%s,%s,%d,%d,%s\n" "$trial_id" "$result" "$path" "$code" "$send_exit" "$recv_exit" "$trial_dir" >> "$RESULTS_CSV"
done

success_count=$((p2p_count + relay_count))
p2p_all_rate="$(pct "$p2p_count" "$TRIALS")"
p2p_success_rate="$(pct "$p2p_count" "$success_count")"

summary_file="$WORKDIR/summary.txt"
{
  echo "Wormzy NAT traversal trial summary"
  echo "Timestamp: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "Trials: $TRIALS"
  echo "Successful: $success_count"
  echo "Failed: $fail_count"
  echo "P2P successes: $p2p_count"
  echo "Relay successes: $relay_count"
  echo "P2P rate (all trials): ${p2p_all_rate}%"
  echo "P2P rate (successful trials): ${p2p_success_rate}%"
  echo
  echo "Results CSV: $RESULTS_CSV"
} | tee "$summary_file"

# Keep data if any trial failed so logs are available.
if [[ "$fail_count" -gt 0 ]]; then
  KEEP_WORKDIR=1
  echo "[p2p-rate] failures detected; preserving logs at: $WORKDIR"
fi

# Preserve if explicitly requested.
if [[ "$KEEP_WORKDIR" -eq 1 ]]; then
  echo "[p2p-rate] artifacts kept at: $WORKDIR"
fi

# Non-zero exit on failures makes CI usage straightforward.
if [[ "$fail_count" -gt 0 ]]; then
  exit 1
fi
