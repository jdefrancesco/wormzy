# P2P Optimization Workflow

This directory contains documentation and tools for analyzing and improving Wormzy's P2P connection success rate.

## Quick Start

### 1. Set up metrics collection

Ensure your relay server has Redis configured and metrics are being published:

```bash
# Check your relay configuration
echo $WORMZY_METRICS_REDIS
echo $WORMZY_REDIS_URL
```

### 2. Run the dashboard

```bash
cd cmd/dashboard
go run . -redis rediss://your-redis-url -refresh 5s
```

### 3. Generate test traffic

Follow the testing scenarios in `P2P-BASELINE-TEMPLATE.md`:

```bash
# Same LAN test
# Terminal 1
PAIRING_CODE="$(go run ./cmd/wormzy code)"
printf 'Pairing code: %s\n' "$PAIRING_CODE"
go run ./cmd/wormzy recv --code "$PAIRING_CODE" 2>&1 | tee recv-lan.log

# Terminal 2: copy the freshly generated code printed by Terminal 1.
PAIRING_CODE='<code-from-terminal-1>'
go run ./cmd/wormzy send testfile.bin --code "$PAIRING_CODE" 2>&1 | tee send-lan.log
```

For durability testing, run a repeated matrix across payload sizes:

```bash
make build
./scripts/nat-durability.sh \
  --trials 50 \
  --payload-kibs 16,64,1024 \
  --relay https://relay.wormzy.io
```

This validates transfer churn and path selection on the current host. It is not
a strong NAT-punching test unless the peers are actually behind different NATs.

On Linux, use the namespace lab to put sender and receiver behind separate NATs:

```bash
sudo ./scripts/setup-nat-lab.sh up --mode cone
./scripts/nat-durability.sh \
  --trials 40 \
  --payload-kibs 16,64,1024,8192 \
  --send-ns nsA \
  --recv-ns nsB \
  --relay https://relay.wormzy.io
sudo ./scripts/setup-nat-lab.sh down
```

Repeat with `--mode symmetric`. Cone NAT should strongly prefer P2P; symmetric
NAT is expected to fall back to relay more often.

### 4. Analyze logs

```bash
./scripts/analyze-p2p-logs.sh -l recv-lan.log
./scripts/analyze-p2p-logs.sh -l send-lan.log
```

### 5. Record metrics

Copy `P2P-BASELINE-TEMPLATE.md` and fill in your results:

```bash
cp docs/P2P-BASELINE-TEMPLATE.md docs/P2P-BASELINE-$(date +%Y%m%d).md
```

## Files

- **P2P-OPTIMIZATION-GUIDE.md** - Comprehensive guide explaining timing, tuning, and analysis
- **P2P-BASELINE-TEMPLATE.md** - Template for recording baseline metrics
- **../scripts/analyze-p2p-logs.sh** - Helper script to parse transfer logs
- **../scripts/nat-durability.sh** - Matrix runner for repeated P2P durability trials

## Current Path Order

Wormzy claims and displays the pairing code before network discovery. It publishes initial local and reflexive candidates, derives the CPace key, and then tries Pion ICE on Pion-owned sockets. Immediately before Pion begins connectivity checks, Wormzy arms a 1.5-second timer; if ICE remains unresolved, a cancellable UPnP attempt maps the separate legacy UDP socket. An ICE win removes that mapping, while an ICE failure triggers a PAKE-authenticated candidate refresh before the legacy direct race. Explicitly configured TURN candidates participate in the initial ICE attempt and can win before UPnP; the custom Wormzy relay is last. After transfer, peers authenticate a receipt bound to the pairing code, file size, and BLAKE3 digest before reporting success. A peer without this completion protocol is rejected with upgrade guidance rather than silently downgraded.

## Workflow

```mermaid
graph TD
    A[Collect Baseline] --> B[Run Dashboard]
    B --> C[Generate Test Traffic]
    C --> D[Analyze Logs]
    D --> E{P2P Rate > 70%?}
    E -->|Yes| F[Document & Done]
    E -->|No| G[Identify Bottleneck]
    G --> H[Read Optimization Guide]
    H --> I[Make Targeted Change]
    I --> J[Test Change]
    J --> K{Improvement?}
    K -->|Yes| L[Keep Change]
    K -->|No| M[Revert]
    L --> E
    M --> G
```

## Key Metrics

Focus on these dashboard metrics:

| Metric | Target | Action if Below Target |
|--------|--------|------------------------|
| P2P % | 70%+ | Check DirectOutcome distribution |
| Same-LAN P2P | 100% | BUG: Check candidate selection |
| UPnP candidate use | Only after ICE failure | Check progressive UPnP and candidate-refresh logs |
| quic-timeout rate | <20% | Consider longer relay fallback delay |
| no-response rate | <10% | Check STUN servers |

## Common Issues

### Issue: Relay used on same LAN
**Symptom**: Dashboard shows relay transfers where both peers on local network  
**Priority**: HIGH - this should never happen  
**Fix**: Check `preferLocal` logic in `internal/transport/dilation.go:58`

### Issue: High quic-timeout but connections work eventually
**Symptom**: DirectOutcome shows many quic-timeout, but some P2P succeeds  
**Priority**: MEDIUM  
**Fix**: Increase `relayFallbackDelay` from 4s to 6s+ in `transport.go:52`

### Issue: Consistent STUN failures
**Symptom**: Logs show "STUN error" repeatedly  
**Priority**: HIGH - blocks P2P entirely  
**Fix**: Check network UDP filtering, add more STUN servers

### Issue: Noise handshake failures
**Symptom**: QUIC connects but DirectOutcome = "noise-failed"  
**Priority**: HIGH - not a NAT issue, crypto problem  
**Fix**: Check version mismatch, key derivation

## Reading the Code

Start here to understand the P2P flow:

1. `internal/transport/transport.go:Run()` - Main transfer orchestration
   - Claims the code, gathers the initial legacy candidates, tries ICE, then runs the legacy direct/relay race

2. `internal/transport/dilation.go` - Candidate selection
   - `buildCandidates()` - Advertise our addresses
   - `selectPeerCandidates()` - Choose the peer's best addresses
   - `candidateRaceWeight()` - Priority scoring

3. `internal/stun/stun.go` - STUN discovery
   - `DiscoverOnConn()` - Sequentially probes a shuffled server list on the shared legacy socket
   - `probeWithConn()` - Single STUN query on that socket

4. `internal/transport/ice_path.go` and `progressive_upnp.go`
   - Pion-owned ICE sockets and candidate exchange
   - Delayed UPnP, PAKE-authenticated readiness, and refreshed legacy candidates

## Testing Checklist

Before committing P2P changes:

- [ ] Run 10+ same-LAN transfers (should be 100% P2P)
- [ ] Run 10+ NAT-to-NAT transfers (target 70%+ P2P)
- [ ] Run 5+ mobile hotspot transfers
- [ ] Check dashboard metrics match expectations
- [ ] Verify no regressions in logs (new errors?)
- [ ] Document results in baseline file

## Getting Help

If P2P rate is unexpectedly low:

1. Check your Redis URL is correct (`WORMZY_METRICS_REDIS`)
2. Verify relay server is running and reachable
3. Test with `./scripts/analyze-p2p-logs.sh` to see detailed timing
4. Look for patterns in "Recent failures" dashboard panel
5. Capture packet trace with `tcpdump` if needed (see guide)

## References

- NAT traversal theory: `Nat-Punch.md`
- How wormzy works: `HOWITWORKS.md`
- Repository guidelines: `../AGENTS.md`
