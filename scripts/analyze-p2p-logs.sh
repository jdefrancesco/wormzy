#!/usr/bin/env bash
# P2P Metrics Collection Helper Script
#
# This script helps automate collection of P2P vs relay metrics for analysis.
# Run this alongside manual testing to extract key metrics from logs.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Options:
    -l, --log FILE          Analyze log file (default: stdin)
    -o, --output FILE       Write summary to file (default: stdout)
    -v, --verbose           Show detailed breakdown
    -h, --help              Show this help message

Examples:
    # Analyze logs from file
    $0 -l transfer.log

    # Analyze logs from running transfer with live output
    go run ./cmd/wormzy -mode recv 2>&1 | tee transfer.log
    # (in another terminal after transfer completes)
    $0 -l transfer.log

    # Verbose analysis
    $0 -l transfer.log -v
EOF
}

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*" >&2
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*" >&2
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

analyze_logs() {
    local log_file="$1"
    local verbose="${2:-false}"

    if [[ ! -f "$log_file" ]]; then
        log_error "Log file not found: $log_file"
        return 1
    fi

    log_info "Analyzing log file: $log_file"
    echo ""

    # Extract key metrics
    echo "=== P2P Connection Analysis ==="
    echo ""

    # Punch statistics
    echo "## NAT Punch Statistics"
    if grep -q "punch/stop" "$log_file"; then
        local punch_lines=$(grep "punch/stop" "$log_file")
        echo "$punch_lines" | while IFS= read -r line; do
            if [[ "$line" =~ rounds=([0-9]+) ]]; then
                rounds="${BASH_REMATCH[1]}"
            fi
            if [[ "$line" =~ packets=([0-9]+) ]]; then
                packets="${BASH_REMATCH[1]}"
            fi
            if [[ "$line" =~ elapsed=([0-9.]+[a-z]+) ]]; then
                elapsed="${BASH_REMATCH[1]}"
            fi
            if [[ "$line" =~ reason=([a-z-]+) ]]; then
                reason="${BASH_REMATCH[1]}"
            fi
            printf "  Rounds: %-3s  Packets: %-4s  Elapsed: %-8s  Reason: %s\n" \
                "${rounds:-?}" "${packets:-?}" "${elapsed:-?}" "${reason:-?}"
        done
    else
        echo "  (no punch statistics found)"
    fi
    echo ""

    # Direct race outcome
    echo "## Direct Race Results"
    if grep -q "direct race" "$log_file"; then
        grep "direct race" "$log_file" | head -20
    else
        echo "  (no direct race info found)"
    fi
    echo ""

    # Transport used
    echo "## Connection Outcome"
    if grep -Eq "direct race won|direct race outcome=won" "$log_file"; then
        echo -e "  ${GREEN}✓ P2P connection established${NC}"
        grep -E "direct race won|direct race outcome=won" "$log_file" | head -1
    elif grep -q "falling back to relay" "$log_file"; then
        echo -e "  ${YELLOW}→ Fell back to relay${NC}"
        grep "falling back to relay" "$log_file" | head -1
    else
        echo "  (connection outcome unclear)"
    fi
    echo ""

    # STUN results
    echo "## STUN Discovery"
    if grep -qi "stun" "$log_file"; then
        stun_success=$(grep -Eci "STAGE stun done|STUN.*discovered" "$log_file" || true)
        stun_fail=$(grep -Eci "STAGE stun error|STUN.*error|STUN.*failed" "$log_file" || true)
        echo "  Successful STUN queries: $stun_success"
        echo "  Failed STUN queries: $stun_fail"
    else
        echo "  (no STUN info found)"
    fi
    echo ""

    # Timing breakdown
    echo "## Timing Breakdown"
    if grep -q "direct/dial-start" "$log_file"; then
        echo "  Dial attempts:"
        grep "direct/dial-start" "$log_file" | while IFS= read -r line; do
            if [[ "$line" =~ attempt=([0-9]+) ]]; then
                attempt="${BASH_REMATCH[1]}"
            fi
            if [[ "$line" =~ target=([^ ]+) ]]; then
                target="${BASH_REMATCH[1]}"
            fi
            if [[ "$line" =~ type=([^ ]+) ]]; then
                type="${BASH_REMATCH[1]}"
            fi
            echo "    Attempt $attempt: $type -> $target"
        done
    fi
    echo ""

    # Errors
    echo "## Errors/Warnings"
    if grep -qi "error\|failed\|timeout" "$log_file"; then
        grep -i "error\|failed\|timeout" "$log_file" | grep -v "InsecureSkipVerify" | head -10
    else
        echo "  (no errors found)"
    fi
    echo ""

    # Transfer stats
    echo "## Transfer Statistics"
    if grep -Eq "transfer complete|STAGE transfer done" "$log_file"; then
        echo -e "  ${GREEN}✓ Transfer completed successfully${NC}"
        if grep -q "throughput" "$log_file"; then
            grep "throughput" "$log_file" | tail -1
        fi
    else
        echo "  (transfer not completed or stats not found)"
    fi
    echo ""

    log_success "Analysis complete"
}

# Parse arguments
LOG_FILE=""
OUTPUT_FILE=""
VERBOSE="false"

while [[ $# -gt 0 ]]; do
    case $1 in
        -l|--log)
            LOG_FILE="$2"
            shift 2
            ;;
        -o|--output)
            OUTPUT_FILE="$2"
            shift 2
            ;;
        -v|--verbose)
            VERBOSE="true"
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            usage
            exit 1
            ;;
    esac
done

# Main execution
if [[ -z "$LOG_FILE" ]]; then
    log_error "No log file specified"
    usage
    exit 1
fi

if [[ -n "$OUTPUT_FILE" ]]; then
    analyze_logs "$LOG_FILE" "$VERBOSE" > "$OUTPUT_FILE"
    log_success "Results written to: $OUTPUT_FILE"
else
    analyze_logs "$LOG_FILE" "$VERBOSE"
fi
