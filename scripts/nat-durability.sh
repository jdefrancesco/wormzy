#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'USAGE'
Usage:
  scripts/nat-durability.sh [options]

Options:
  --trials N            Trials per payload size (default: 20)
  --payload-kibs LIST   Comma-separated KiB sizes (default: 16,64,1024)
  --wormzy PATH         Wormzy binary path (default: ./bin/wormzy)
  --relay URL           Relay/mailbox endpoint (default: https://relay.wormzy.io)
  --turn URLS           Optional TURN list passed to wormzy --turn
  --send-ns NAME        Sender network namespace (optional)
  --recv-ns NAME        Receiver network namespace (optional)
  --trial-timeout SEC   Timeout per sender/receiver process (default: 90)
  --code-timeout SEC    Time to wait for sender pairing code (default: 20)
  --workdir DIR         Output directory (default: mktemp)
  --stop-on-fail        Stop after the first failing scenario
  -h, --help            Show this help

Examples:
  scripts/nat-durability.sh --trials 50 --payload-kibs 16,64,1024

  sudo scripts/setup-nat-lab.sh up --mode cone
  scripts/nat-durability.sh \
    --trials 40 \
    --payload-kibs 16,64,1024,8192 \
    --send-ns nsA \
    --recv-ns nsB
USAGE
}

TRIALS=20
PAYLOAD_KIBS="16,64,1024"
WORMZY_BIN="./bin/wormzy"
RELAY="https://relay.wormzy.io"
TURN_URLS=""
SEND_NS=""
RECV_NS=""
TRIAL_TIMEOUT_S=90
CODE_TIMEOUT_S=20
WORKDIR=""
STOP_ON_FAIL=0
P2P_SCRIPT="scripts/p2p-rate.sh"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --trials)
      TRIALS="$2"
      shift 2
      ;;
    --payload-kibs)
      PAYLOAD_KIBS="$2"
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
    --stop-on-fail)
      STOP_ON_FAIL=1
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
fi
if [[ ! -x "$WORMZY_BIN" ]]; then
  echo "error: wormzy binary not executable: $WORMZY_BIN" >&2
  echo "hint: run make build" >&2
  exit 1
fi
if [[ ! -x "$P2P_SCRIPT" ]]; then
  echo "error: helper script not executable: $P2P_SCRIPT" >&2
  exit 1
fi

payloads="$(printf "%s" "$PAYLOAD_KIBS" | tr ',' ' ')"
for payload_kib in $payloads; do
  if ! [[ "$payload_kib" =~ ^[0-9]+$ ]] || [[ "$payload_kib" -le 0 ]]; then
    echo "error: payload sizes must be positive integers: $PAYLOAD_KIBS" >&2
    exit 2
  fi
done

if [[ -z "$WORKDIR" ]]; then
  WORKDIR="$(mktemp -d -t wormzy-nat-durability-XXXXXX)"
else
  mkdir -p "$WORKDIR"
fi

MATRIX_CSV="$WORKDIR/matrix.csv"
SUMMARY_MD="$WORKDIR/summary.md"

pct() {
  local n="$1"
  local d="$2"
  if [[ "$d" -eq 0 ]]; then
    printf "0.00"
    return
  fi
  awk -v n="$n" -v d="$d" 'BEGIN { printf "%.2f", (n * 100.0) / d }'
}

longest_p2p_streak() {
  awk -F, '
    NR > 1 {
      if ($2 == "pass" && $3 == "p2p") {
        streak++
        if (streak > best) best = streak
      } else {
        streak = 0
      }
    }
    END { print best + 0 }
  ' "$1"
}

scenario_stats() {
  awk -F, '
    NR > 1 {
      total++
      if ($2 == "pass" && $3 == "p2p") p2p++
      else if ($2 == "pass" && $3 == "relay") relay++
      else fail++
    }
    END {
      printf "%d,%d,%d,%d", total + 0, p2p + 0, relay + 0, fail + 0
    }
  ' "$1"
}

append_summary_header() {
  {
    echo "# Wormzy NAT Durability"
    echo
    echo "- Timestamp: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "- Relay: $RELAY"
    echo "- Trials per payload: $TRIALS"
    echo "- Payload KiB list: $PAYLOAD_KIBS"
    if [[ -n "$SEND_NS" ]]; then
      echo "- Sender namespace: $SEND_NS"
      echo "- Receiver namespace: $RECV_NS"
    else
      echo "- Sender/receiver namespaces: none"
    fi
    if [[ -n "$TURN_URLS" ]]; then
      echo "- TURN URLs: configured"
    else
      echo "- TURN URLs: default"
    fi
    echo
    echo "| Payload KiB | Trials | P2P | Relay | Fail | P2P All % | P2P Success % | Longest P2P Streak | Status |"
    echo "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |"
  } > "$SUMMARY_MD"
}

printf "payload_kib,trials,p2p,relay,fail,p2p_all_rate,p2p_success_rate,longest_p2p_streak,status,scenario_dir\n" > "$MATRIX_CSV"
append_summary_header

echo "[nat-durability] workdir: $WORKDIR"
echo "[nat-durability] payloads: $PAYLOAD_KIBS"
echo "[nat-durability] trials per payload: $TRIALS"
echo "[nat-durability] relay: $RELAY"

overall_status=0

for payload_kib in $payloads; do
  scenario_name="payload-${payload_kib}kib"
  scenario_dir="$WORKDIR/$scenario_name"
  scenario_log="$WORKDIR/$scenario_name.log"

  cmd=(
    "$P2P_SCRIPT"
    --trials "$TRIALS"
    --payload-kib "$payload_kib"
    --wormzy "$WORMZY_BIN"
    --relay "$RELAY"
    --trial-timeout "$TRIAL_TIMEOUT_S"
    --code-timeout "$CODE_TIMEOUT_S"
    --workdir "$scenario_dir"
    --keep
  )
  if [[ -n "$TURN_URLS" ]]; then
    cmd+=( --turn "$TURN_URLS" )
  fi
  if [[ -n "$SEND_NS" ]]; then
    cmd+=( --send-ns "$SEND_NS" --recv-ns "$RECV_NS" )
  fi

  echo "[nat-durability] scenario $scenario_name"
  set +e
  "${cmd[@]}" 2>&1 | tee "$scenario_log"
  scenario_status="${PIPESTATUS[0]}"
  set -e

  if [[ -f "$scenario_dir/results.csv" ]]; then
    stats="$(scenario_stats "$scenario_dir/results.csv")"
    IFS=, read -r total p2p relay fail <<EOF
$stats
EOF
    streak="$(longest_p2p_streak "$scenario_dir/results.csv")"
  else
    total="$TRIALS"
    p2p=0
    relay=0
    fail="$TRIALS"
    streak=0
  fi

  success=$((p2p + relay))
  p2p_all_rate="$(pct "$p2p" "$total")"
  p2p_success_rate="$(pct "$p2p" "$success")"
  status="pass"
  if [[ "$scenario_status" -ne 0 || "$fail" -ne 0 ]]; then
    status="fail"
    overall_status=1
  fi

  printf "%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n" \
    "$payload_kib" "$total" "$p2p" "$relay" "$fail" \
    "$p2p_all_rate" "$p2p_success_rate" "$streak" "$status" "$scenario_dir" >> "$MATRIX_CSV"

  printf "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n" \
    "$payload_kib" "$total" "$p2p" "$relay" "$fail" \
    "$p2p_all_rate" "$p2p_success_rate" "$streak" "$status" >> "$SUMMARY_MD"

  if [[ "$status" == "fail" && "$STOP_ON_FAIL" -eq 1 ]]; then
    echo "[nat-durability] stopping after failing scenario: $scenario_name"
    break
  fi
done

{
  echo
  echo "Artifacts:"
  echo "- Matrix CSV: $MATRIX_CSV"
  echo "- Summary: $SUMMARY_MD"
  echo "- Scenario logs: $WORKDIR/payload-*.log"
} >> "$SUMMARY_MD"

echo
echo "[nat-durability] matrix:"
cat "$MATRIX_CSV"
echo
echo "[nat-durability] summary: $SUMMARY_MD"

exit "$overall_status"
