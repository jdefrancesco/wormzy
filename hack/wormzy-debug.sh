#!/usr/bin/env bash
set -euo pipefail

OUTDIR="wormzy-debug-$(date +%Y%m%d-%H%M%S)"
IFACE="${1:-any}"

mkdir -p "$OUTDIR"

echo "[+] Output dir: $OUTDIR"
echo "[+] Interface:  $IFACE"
echo

cleanup() {
  echo
  echo "[+] Stopping captures..."
  for pid in "${PIDS[@]:-}"; do
    sudo kill "$pid" 2>/dev/null || true
  done
  sleep 1
}
trap cleanup EXIT

PIDS=()

echo "[+] Starting captures..."

# Full raw capture for Wireshark / tshark analysis
sudo tcpdump -i "$IFACE" -nn -s0 udp -w "$OUTDIR/full.pcap" >/dev/null 2>&1 &
PIDS+=($!)

# Human-readable STUN log
sudo tcpdump -i "$IFACE" -nn -tttt -l udp port 3478 > "$OUTDIR/stun.log" 2>/dev/null &
PIDS+=($!)

# Human-readable non-STUN UDP log (punching + QUIC + relay)
sudo tcpdump -i "$IFACE" -nn -tttt -l 'udp and not port 3478' > "$OUTDIR/udp.log" 2>/dev/null &
PIDS+=($!)

echo "[+] Capture PIDs: ${PIDS[*]}"
echo
echo "[*] Run Wormzy now on both sides."
echo "[*] Press ENTER here when the transfer attempt is done."
read -r _

cleanup
trap - EXIT

SUMMARY="$OUTDIR/summary.txt"
QUIC_TXT="$OUTDIR/quic-tshark.txt"

echo "[+] Building summary..."

{
  echo "==== Wormzy Debug Summary ===="
  echo "Created: $(date)"
  echo

  echo "---- STUN packets seen ----"
  if [[ -s "$OUTDIR/stun.log" ]]; then
    wc -l < "$OUTDIR/stun.log"
    echo
    sed -n '1,20p' "$OUTDIR/stun.log"
  else
    echo "0"
  fi
  echo

  echo "---- Unique non-STUN UDP flows ----"
  if [[ -s "$OUTDIR/udp.log" ]]; then
    awk '
      {
        src=$3; dst=$5
        sub(/:$/, "", src)
        sub(/:$/, "", dst)
        print src " -> " dst
      }
    ' "$OUTDIR/udp.log" | sort | uniq -c | sort -nr
  else
    echo "none"
  fi
  echo

  echo "---- Bidirectional pairs (likely punch success) ----"
  if [[ -s "$OUTDIR/udp.log" ]]; then
    awk '
      {
        a=$3; b=$5
        sub(/:$/, "", a)
        sub(/:$/, "", b)
        seen[a "|" b]=1
      }
      END {
        for (k in seen) {
          split(k, p, /\|/)
          rev=p[2] "|" p[1]
          if (seen[rev]) {
            if (!(done[k] || done[rev])) {
              print p[1] " <-> " p[2]
              done[k]=1
              done[rev]=1
            }
          }
        }
      }
    ' "$OUTDIR/udp.log" | sort | uniq
  else
    echo "none"
  fi
  echo

  echo "---- Top destination talkers (relay hint) ----"
  if [[ -s "$OUTDIR/udp.log" ]]; then
    awk '{print $5}' "$OUTDIR/udp.log" | sed 's/:$//' | sort | uniq -c | sort -nr | head -10
  else
    echo "none"
  fi
  echo

  echo "---- QUIC fingerprint detection ----"
  if command -v tshark >/dev/null 2>&1; then
    # Prefer protocol-aware decode if tshark can parse QUIC.
    tshark -r "$OUTDIR/full.pcap" -Y quic \
      -T fields \
      -e frame.number \
      -e frame.time \
      -e ip.src \
      -e udp.srcport \
      -e ip.dst \
      -e udp.dstport \
      -e quic.packet_type \
      2>/dev/null | tee "$QUIC_TXT" >/dev/null || true

    if [[ -s "$QUIC_TXT" ]]; then
      echo "tshark QUIC packets:"
      sed -n '1,40p' "$QUIC_TXT"
    else
      echo "No protocol-decoded QUIC packets found via tshark."
      echo "That can still happen when the traffic is encrypted or the dissector does not classify it."
    fi
  else
    echo "tshark not installed; skipping protocol-aware QUIC detection."
  fi
  echo

  echo "---- Heuristic QUIC-long-header hint ----"
  echo "Use in Wireshark/tshark:"
  echo "  udp && quic"
  echo "  udp && !stun"
  echo "  Statistics -> Conversations -> UDP"
  echo

  echo "---- Files ----"
  echo "PCAP:      $OUTDIR/full.pcap"
  echo "STUN log:  $OUTDIR/stun.log"
  echo "UDP log:   $OUTDIR/udp.log"
  [[ -f "$QUIC_TXT" ]] && echo "QUIC txt:  $QUIC_TXT"
  echo

  echo "---- Quick interpretation guide ----"
  echo "STUN seen + bidirectional peer UDP + QUIC packets   => likely successful NAT punch"
  echo "STUN seen + only one remote IP dominates            => likely relay fallback"
  echo "STUN seen + outbound-only peer probes               => likely NAT/firewall block"
} > "$SUMMARY"

cat "$SUMMARY"

echo
echo "[+] Open the capture with:"
echo "    wireshark $OUTDIR/full.pcap"
echo
echo "[+] Or inspect with tshark:"
echo "    tshark -r $OUTDIR/full.pcap -Y stun"
echo "    tshark -r $OUTDIR/full.pcap -Y quic"
echo "    tshark -r $OUTDIR/full.pcap -q -z conv,udp"
