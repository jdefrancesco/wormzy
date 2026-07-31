#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'USAGE'
Usage:
  scripts/upnp-ab.sh plan [options]
  scripts/upnp-ab.sh run [options]
  scripts/upnp-ab.sh summarize [options]

Create one plan, copy it to two machines behind different NATs, and run one
machine as sender and the other as receiver. Both runs use the same trial codes
and alternate UPnP on/off in a balanced order.

Plan options:
  --trials-per-arm N   Trials with UPnP on and off (default: 10)
  --output FILE        Plan CSV to create (required)
  --force              Replace an existing plan

Run options:
  --role ROLE          send or recv (required)
  --plan FILE          Shared plan CSV (required)
  --wormzy PATH        Wormzy binary (default: ./bin/wormzy)
  --relay URL          Mailbox endpoint (default: https://relay.wormzy.io)
  --turn URLS          Optional authenticated TURN list passed to wormzy --turn
  --payload FILE       Sender payload (default: generated 64 KiB file)
  --payload-kib N      Generated sender payload size (default: 64)
  --trial-timeout SEC  Handshake timeout for each trial (default: 90)
  --workdir DIR        Artifact directory (default: mktemp)

Summarize options:
  --send-results FILE  Sender results.csv (required)
  --recv-results FILE  Receiver results.csv (required)

Example:
  scripts/upnp-ab.sh plan --trials-per-arm 20 --output upnp-plan.csv

  # NAT A
  scripts/upnp-ab.sh run --role send --plan upnp-plan.csv --workdir ab-send

  # NAT B
  scripts/upnp-ab.sh run --role recv --plan upnp-plan.csv --workdir ab-recv

  scripts/upnp-ab.sh summarize \
    --send-results ab-send/results.csv \
    --recv-results ab-recv/results.csv
USAGE
}

die() {
  echo "error: $*" >&2
  exit 2
}

require_value() {
  if [[ $# -lt 2 || -z "$2" ]]; then
    die "$1 requires a value"
  fi
}

generate_code() {
  local raw
  raw="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  if [[ ${#raw} -ne 16 ]]; then
    die "could not generate a trial code"
  fi
  printf "upnp-ab-%s" "$raw"
}

plan_command() {
  local trials_per_arm=10
  local output=""
  local force=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --trials-per-arm)
        require_value "$@"
        trials_per_arm="$2"
        shift 2
        ;;
      --output)
        require_value "$@"
        output="$2"
        shift 2
        ;;
      --force)
        force=1
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown plan option $1"
        ;;
    esac
  done

  [[ "$trials_per_arm" =~ ^[0-9]+$ ]] && [[ "$trials_per_arm" -gt 0 ]] ||
    die "--trials-per-arm must be a positive integer"
  [[ -n "$output" ]] || die "--output is required"
  if [[ -e "$output" && "$force" -ne 1 ]]; then
    die "plan already exists: $output (pass --force to replace it)"
  fi

  printf "trial,arm,code\n" > "$output"
  local pair trial arm code
  trial=1
  for ((pair = 1; pair <= trials_per_arm; pair++)); do
    if ((pair % 2 == 1)); then
      for arm in on off; do
        code="$(generate_code)"
        printf "%03d,%s,%s\n" "$trial" "$arm" "$code" >> "$output"
        trial=$((trial + 1))
      done
    else
      for arm in off on; do
        code="$(generate_code)"
        printf "%03d,%s,%s\n" "$trial" "$arm" "$code" >> "$output"
        trial=$((trial + 1))
      done
    fi
  done

  echo "[upnp-ab] plan: $output"
  echo "[upnp-ab] trials: $((trials_per_arm * 2)) ($trials_per_arm per arm)"
}

detect_path() {
  local output="$1"
  local log="$2"
  local line
  line="$(grep -E "^Path: (P2P|RELAY)" "$output" 2>/dev/null | tail -1 || true)"
  case "$line" in
    "Path: P2P "*)
      echo "p2p"
      return
      ;;
    "Path: RELAY "*)
      echo "relay"
      return
      ;;
  esac
  if grep -Eq "STAGE quic done relay fallback|direct race outcome=.*(relay|ice-relay)@.*=won" "$log" 2>/dev/null; then
    echo "relay"
    return
  fi
  if grep -Eq "direct race outcome=won details=.*(ice-p2p|local|reflexive|loopback|upnp)@.*=won" "$log" 2>/dev/null; then
    echo "p2p"
    return
  fi
  echo "unknown"
}

detect_candidate() {
  local output="$1"
  local log="$2"
  local line details token
  line="$(grep -E "^Path: (P2P|RELAY) \([^)]*\)" "$output" 2>/dev/null | tail -1 || true)"
  if [[ -n "$line" ]]; then
    line="${line#*(}"
    echo "${line%)}"
    return
  fi
  line="$(grep -E "direct race outcome=won details=.*=won" "$log" 2>/dev/null | tail -1 || true)"
  if [[ -n "$line" ]]; then
    details="${line##*details=}"
    while IFS= read -r token; do
      if [[ "$token" == *=won ]]; then
        token="${token%%@*}"
        echo "${token##* }"
        return
      fi
    done < <(printf "%s" "$details" | tr ',' '\n')
  fi
  if grep -Eq "STAGE quic done relay fallback" "$log" 2>/dev/null; then
    echo "relay"
    return
  fi
  echo "unknown"
}

detect_upnp_status() {
  local arm="$1"
  local log="$2"
  if [[ "$arm" == "off" ]]; then
    echo "disabled"
  elif grep -q "upnp/map external=" "$log" 2>/dev/null; then
    echo "mapped"
  elif grep -q "upnp/map failed:" "$log" 2>/dev/null; then
    echo "failed"
  else
    echo "unknown"
  fi
}

print_role_summary() {
  local results="$1"
  awk -F, '
    NR > 1 {
      arm = $2
      total[arm]++
      if ($4 == "pass" && $5 == "p2p") p2p[arm]++
      else if ($4 == "pass" && $5 == "relay") relay[arm]++
      else failed[arm]++
      if ($7 == "mapped") mapped[arm]++
    }
    END {
      print "Arm,Trials,P2P,Relay,Failed,Local mappings"
      printf "on,%d,%d,%d,%d,%d\n", total["on"] + 0, p2p["on"] + 0, relay["on"] + 0, failed["on"] + 0, mapped["on"] + 0
      printf "off,%d,%d,%d,%d,%d\n", total["off"] + 0, p2p["off"] + 0, relay["off"] + 0, failed["off"] + 0, mapped["off"] + 0
    }
  ' "$results"
}

run_command() {
  local role=""
  local plan=""
  local wormzy_bin="./bin/wormzy"
  local relay="https://relay.wormzy.io"
  local turn_urls=""
  local payload=""
  local payload_kib=64
  local trial_timeout=90
  local workdir=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --role)
        require_value "$@"
        role="$2"
        shift 2
        ;;
      --plan)
        require_value "$@"
        plan="$2"
        shift 2
        ;;
      --wormzy)
        require_value "$@"
        wormzy_bin="$2"
        shift 2
        ;;
      --relay)
        require_value "$@"
        relay="$2"
        shift 2
        ;;
      --turn)
        require_value "$@"
        turn_urls="$2"
        shift 2
        ;;
      --payload)
        require_value "$@"
        payload="$2"
        shift 2
        ;;
      --payload-kib)
        require_value "$@"
        payload_kib="$2"
        shift 2
        ;;
      --trial-timeout)
        require_value "$@"
        trial_timeout="$2"
        shift 2
        ;;
      --workdir)
        require_value "$@"
        workdir="$2"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown run option $1"
        ;;
    esac
  done

  [[ "$role" == "send" || "$role" == "recv" ]] || die "--role must be send or recv"
  [[ -f "$plan" ]] || die "plan not found: $plan"
  [[ -x "$wormzy_bin" ]] || die "wormzy binary not executable: $wormzy_bin"
  [[ "$payload_kib" =~ ^[0-9]+$ ]] && [[ "$payload_kib" -gt 0 ]] ||
    die "--payload-kib must be a positive integer"
  [[ "$trial_timeout" =~ ^[0-9]+$ ]] && [[ "$trial_timeout" -gt 0 ]] ||
    die "--trial-timeout must be a positive integer"
  [[ "$(head -1 "$plan")" == "trial,arm,code" ]] || die "unexpected plan header: $plan"

  if [[ -z "$workdir" ]]; then
    workdir="$(mktemp -d -t "wormzy-upnp-ab-${role}-XXXXXX")"
  else
    mkdir -p "$workdir"
  fi
  if [[ "$role" == "send" ]]; then
    if [[ -z "$payload" ]]; then
      payload="$workdir/payload.bin"
      dd if=/dev/urandom of="$payload" bs=1024 count="$payload_kib" status=none
    fi
    [[ -f "$payload" ]] || die "payload not found: $payload"
  fi

  local results="$workdir/results.csv"
  printf "trial,arm,code,result,path,candidate,upnp_status,exit_code,duration_seconds,trial_dir\n" > "$results"
  echo "[upnp-ab] role: $role"
  echo "[upnp-ab] workdir: $workdir"
  echo "[upnp-ab] plan: $plan"
  echo "[upnp-ab] start the matching role on the other NAT if it is not already running"

  local trial arm code extra trial_dir output log started finished status path candidate upnp_status result
  local overall_status=0
  while IFS=, read -r trial arm code extra; do
    if [[ "$trial" == "trial" ]]; then
      continue
    fi
    code="${code%$'\r'}"
    [[ "$trial" =~ ^[0-9]+$ ]] || die "invalid trial number in $plan: $trial"
    [[ "$arm" == "on" || "$arm" == "off" ]] || die "invalid arm in $plan: $arm"
    [[ -n "$code" && -z "$extra" ]] || die "invalid plan row for trial $trial"

    trial_dir="$workdir/trial-$trial"
    mkdir -p "$trial_dir"
    output="$trial_dir/$role.out"
    log="$trial_dir/$role.log"
    local -a cmd
    if [[ "$role" == "send" ]]; then
      cmd=("$wormzy_bin" send "$payload" --code "$code" --relay "$relay" --timeout "${trial_timeout}s" --log-file "$log" --auto-exit)
    else
      mkdir -p "$trial_dir/recv"
      cmd=("$wormzy_bin" recv --relay "$relay" --timeout "${trial_timeout}s" --download-dir "$trial_dir/recv" --log-file "$log" --auto-exit)
    fi
    if [[ -n "$turn_urls" ]]; then
      cmd+=(--turn "$turn_urls")
    fi
    if [[ "$arm" == "off" ]]; then
      cmd+=(--no-upnp)
    fi
    if [[ "$role" == "recv" ]]; then
      cmd+=("$code")
    fi

    echo "[trial $trial] role=$role upnp=$arm code=$code"
    started="$(date +%s)"
    set +e
    "${cmd[@]}" > "$output" 2>&1
    status=$?
    set -e
    finished="$(date +%s)"

    path="$(detect_path "$output" "$log")"
    candidate="$(detect_candidate "$output" "$log")"
    upnp_status="$(detect_upnp_status "$arm" "$log")"
    result="pass"
    if [[ "$status" -ne 0 || "$path" == "unknown" ]]; then
      result="fail"
      overall_status=1
    fi
    printf "%s,%s,%s,%s,%s,%s,%s,%d,%d,%s\n" \
      "$trial" "$arm" "$code" "$result" "$path" "$candidate" "$upnp_status" \
      "$status" "$((finished - started))" "$trial_dir" >> "$results"
    echo "[trial $trial] result=$result path=$path candidate=$candidate upnp_status=$upnp_status"
  done < "$plan"

  echo
  print_role_summary "$results"
  echo "[upnp-ab] results: $results"
  exit "$overall_status"
}

summarize_command() {
  local send_results=""
  local recv_results=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --send-results)
        require_value "$@"
        send_results="$2"
        shift 2
        ;;
      --recv-results)
        require_value "$@"
        recv_results="$2"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown summarize option $1"
        ;;
    esac
  done

  [[ -f "$send_results" ]] || die "sender results not found: $send_results"
  [[ -f "$recv_results" ]] || die "receiver results not found: $recv_results"

  awk -F, '
    function pct(n, d) { return d ? (100.0 * n / d) : 0 }
    FILENAME == ARGV[1] {
      if (FNR == 1) next
      arm[$1] = $2
      code[$1] = $3
      send_result[$1] = $4
      send_path[$1] = $5
      send_map[$1] = $7
      next
    }
    FNR == 1 { next }
    {
      trial = $1
      if (!(trial in arm) || arm[trial] != $2 || code[trial] != $3) {
        mismatch++
        next
      }
      seen[trial] = 1
      bucket = arm[trial]
      total[bucket]++
      if (send_map[trial] == "mapped") send_mapped[bucket]++
      if ($7 == "mapped") recv_mapped[bucket]++
      if (send_map[trial] == "mapped" || $7 == "mapped") any_mapped[bucket]++
      if (send_result[trial] != "pass" || $4 != "pass" || send_path[trial] != $5) {
        failed[bucket]++
      } else if ($5 == "p2p") {
        p2p[bucket]++
      } else if ($5 == "relay") {
        relay[bucket]++
      } else {
        failed[bucket]++
      }
    }
    END {
      for (trial in arm) if (!(trial in seen)) mismatch++
      print "Wormzy cross-NAT UPnP A/B summary"
      print "Arm,Trials,P2P,Relay,Failed,P2P all %,Send mapped,Recv mapped,Either mapped"
      printf "on,%d,%d,%d,%d,%.2f,%d,%d,%d\n", total["on"] + 0, p2p["on"] + 0, relay["on"] + 0, failed["on"] + 0, pct(p2p["on"], total["on"]), send_mapped["on"] + 0, recv_mapped["on"] + 0, any_mapped["on"] + 0
      printf "off,%d,%d,%d,%d,%.2f,%d,%d,%d\n", total["off"] + 0, p2p["off"] + 0, relay["off"] + 0, failed["off"] + 0, pct(p2p["off"], total["off"]), send_mapped["off"] + 0, recv_mapped["off"] + 0, any_mapped["off"] + 0
      print ""
      if (mismatch > 0) {
        printf "Verdict: invalid comparison (%d missing or mismatched trial rows).\n", mismatch
        exit 1
      }
      if (any_mapped["on"] == 0) {
        print "Verdict: inconclusive; neither NAT granted a UPnP mapping in the enabled arm."
        exit 0
      }
      delta = pct(p2p["on"], total["on"]) - pct(p2p["off"], total["off"])
      printf "Observed P2P-rate delta: %+.2f percentage points (UPnP on minus off).\n", delta
      print "Treat the delta as directional until each arm has at least 20 trials."
    }
  ' "$send_results" "$recv_results"
}

if [[ $# -lt 1 ]]; then
  usage >&2
  exit 2
fi

command="$1"
shift
case "$command" in
  plan)
    plan_command "$@"
    ;;
  run)
    run_command "$@"
    ;;
  summarize)
    summarize_command "$@"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    die "unknown command $command"
    ;;
esac
