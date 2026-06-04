# P2P Optimization Guide

## Overview

This guide helps you analyze and improve Wormzy's P2P connection success rate. Before making code changes, collect baseline metrics to understand current behavior.

## Step 1: Collect Baseline Metrics

### Run the Dashboard

```bash
# Start the dashboard (requires Redis with metrics)
go run ./cmd/dashboard -redis rediss://your-redis-url -refresh 5s
```

### Key Metrics to Monitor

1. **P2P vs Relay Ratio**
   - Target: 70%+ P2P transfers
   - Current baseline: _[record your value]_

2. **Direct Outcome Breakdown**
   - `won`: Direct connection succeeded
   - `quic-timeout`: Direct attempts timed out, fell back to relay
   - `no-response`: No direct candidates available
   - `noise-failed`: Connection established but Noise handshake failed

3. **Candidate Usage**
   - `reflexive`: Public IP from STUN
   - `local`: LAN address
   - `relay`: Fallback relay
   - `loopback`: Local testing

4. **Failure Causes**
   - Check "Recent failures" panel
   - Look for patterns in error messages

### Generate Test Traffic

```bash
# Terminal 1 (receiver)
go run ./cmd/wormzy -mode recv -code test-$(date +%s)

# Terminal 2 (sender) 
go run ./cmd/wormzy -mode send -file testfile.bin -code <code-from-recv>
```

Repeat 10-20 times across different network conditions:
- Same LAN
- Different networks (NAT-to-NAT)
- Mobile hotspot
- VPN scenarios

### Document Baseline

Create a spreadsheet or document with:

```
Date: YYYY-MM-DD
Total Sessions: ___
P2P: ___ (___%)
Relay: ___ (___%)

Direct Outcomes:
- won: ___
- quic-timeout: ___
- no-response: ___
- noise-failed: ___

Candidate Distribution:
- reflexive: ___
- local: ___
- relay: ___

Common Failures:
1. ___
2. ___
3. ___
```

## Step 2: Analyze Current Timing Configuration

### Current P2P Race Timing

From `internal/transport/transport.go`:

```go
// Constants (lines 43-54)
relayFallbackDelay  = 4 * time.Second   // How long to wait before trying relay
relayRetryDelay     = 3 * time.Second   // Delay between relay retry attempts
relayAttemptTimeout = 6 * time.Second   // Timeout for individual relay dial

// Dial schedule (lines 424-438)
baseDelay := 0ms (recv) or 200ms (send)

Attempt 1: baseDelay + 0ms
Attempt 2: baseDelay + 700ms  
Attempt 3: baseDelay + 1500ms

// Multiple targets get staggered by +120ms each
```

### Punch Packet Timing

From `internal/transport/transport.go` (line 1623):

```go
interval := 150 * time.Millisecond  // Punch packet frequency
heartbeat := 5 * time.Second        // Status logging
```

### Key Questions

1. **Is 4s relay fallback too aggressive?**
   - If direct connections succeed but after 4s, you're racing unnecessarily
   - Check dashboard: How many `quic-timeout` outcomes vs `won`?

2. **Are 3 dial attempts enough?**
   - Some NATs need more "warming up"
   - Check logs: Do attempts 1-3 all fail similarly?

3. **Is 150ms punch interval optimal?**
   - Too fast: Wastes bandwidth
   - Too slow: NAT bindings might close

## Step 3: Identify Bottlenecks

### Check DirectOutcome Distribution

If you see high `quic-timeout`:
- Direct attempts are timing out → increase attempts or delay relay fallback
- Consider: NAT type detection to adjust strategy per-peer

If you see high `no-response`:
- STUN discovery failing → check STUN server list
- No UDP candidates → peer behind restrictive firewall

If you see high `noise-failed`:
- QUIC succeeds but crypto fails → not a NAT issue
- Check key derivation or version mismatch

### Network-Specific Patterns

**Same LAN (preferLocal=true)**:
- Should always succeed via `local` candidate
- If failing: Check LAN discovery logic in `dilation.go:58`

**Different Networks (NAT-to-NAT)**:
- Most challenging scenario
- Should use `reflexive` candidates from STUN
- Success depends on NAT types (see `docs/Nat-Punch.md`)

**Symmetric NAT**:
- Hardest case - may require relay
- NAT changes port for each destination
- Check if `reflexive` attempts show consistent failures

## Step 4: Tuning Recommendations

### Conservative Improvements (Low Risk)

**1. Extend relay fallback delay**
```go
// If direct succeeds >60% but after 4s
relayFallbackDelay = 6 * time.Second  // was 4s
```

**2. Add 4th dial attempt**
```go
// After line 438 in transport.go
for i, target := range directTargets {
    launchDial(target, baseDelay+2400*time.Millisecond+time.Duration(i)*120*time.Millisecond, 4)
}
```

**3. Faster initial punch packets**
```go
// More aggressive NAT hole opening
interval := 100 * time.Millisecond  // was 150ms, for first 2 seconds
// Then back off to 200ms to save bandwidth
```

### Aggressive Improvements (Higher Risk)

**1. Role-specific timing**
```go
// Sender dials more aggressively
if cfg.Mode == "send" {
    relayFallbackDelay = 7 * time.Second
} else {
    relayFallbackDelay = 5 * time.Second  
}
```

**2. Adaptive retry based on candidate type**
```go
// More attempts for reflexive (harder NAT punch)
attempts := 3
if cand.Type == "reflexive" {
    attempts = 5
}
```

**3. Parallel STUN probes**
Already implemented well in `internal/stun/stun.go:68-77`
- Sends to all servers concurrently
- First success wins

### Do NOT Change (Without Good Reason)

- `nonPreferredGrace = 650ms` (line 474) - prevents split-brain
- QUIC KeepAlive/Idle timeouts - these are protocol stability
- Noise handshake flow - crypto must complete in order

## Step 5: Systematic Testing

After each change:

1. **Run 20+ transfers** across network scenarios
2. **Compare metrics** to baseline
3. **Check logs** for new error patterns
4. **Document changes** in commit message

### Test Matrix

```
| Scenario          | Baseline P2P% | After Change | Notes |
|-------------------|---------------|--------------|-------|
| Same LAN          |               |              |       |
| NAT-to-NAT (home) |               |              |       |
| Mobile-to-home    |               |              |       |
| VPN-to-direct     |               |              |       |
```

## Step 6: Advanced Diagnostics

### Enable Verbose Logging

The transport layer logs extensively. Check these patterns:

```bash
# Look for punch statistics
grep "punch/stop" logs.txt

# Check direct race results  
grep "direct race" logs.txt

# See STUN discovery
grep "STUN" logs.txt

# Relay fallback triggers
grep "falling back to relay" logs.txt
```

### Redis Session Inspection

```bash
# Connect to Redis
redis-cli -u rediss://your-redis-url

# List active sessions
KEYS wormzy:sessions:*

# Inspect a session
GET wormzy:sessions:<code>

# Check DirectOutcome and DirectSummary fields
```

### Packet Capture Analysis

For deep debugging:

```bash
# Capture UDP traffic on transfer port
sudo tcpdump -i any udp -w wormzy-trace.pcap

# Look for:
# - Punch packets (small, frequent)
# - QUIC Initial handshake
# - Timing between punch and handshake
```

## Step 7: Decision Framework

### When to Optimize More

✅ Do more tuning if:
- P2P rate < 60% across varied networks
- `quic-timeout` outcomes are high but attempts eventually succeed
- Logs show "direct race won" but after long delays
- Same-LAN transfers ever use relay

### When to Accept Relay Usage

✅ Relay is expected for:
- Symmetric NAT (both peers)
- Corporate firewalls blocking UDP
- Mobile carriers with strict NAT/firewall
- No STUN servers reachable

Target: 70-80% P2P in typical home/mobile scenarios is excellent.

## Current Code Layout Reference

Key files for P2P tuning:

```
internal/transport/transport.go
├── Lines 43-54:   Timing constants
├── Lines 424-438: Dial attempt schedule  
├── Lines 469-555: Relay fallback logic
├── Lines 557-707: Race waitLoop
└── Lines 1619+:   punchLoop implementation

internal/transport/dilation.go
├── Lines 17-46:  buildCandidates (priority ordering)
├── Lines 48-122: selectPeerCandidates (LAN detection)
└── Lines 124-137: candidateRaceWeight (local vs reflexive)

internal/stun/stun.go
├── Lines 14-24:  STUN server list
├── Lines 39-51:  NewStun + discovery
└── Lines 105-164: probeServer (individual STUN query)
```

## Next Steps

1. Collect baseline metrics (this step)
2. Document findings in `P2P-BASELINE.md`
3. Identify 1-2 highest-impact improvements
4. Make surgical changes with git commits per change
5. Retest and compare
6. Iterate

---

**Remember**: The current implementation is already quite sophisticated with:
- Concurrent dial attempts
- Graceful fallback
- LAN detection
- Multiple STUN servers
- Adaptive timing per role

Don't over-optimize without data showing where the real bottleneck is!
