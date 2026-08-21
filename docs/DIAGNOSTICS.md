# P2P Optimization & Dashboard Enhancement - Complete Summary

## What Was Done

I've created a **complete framework** for P2P optimization and significantly enhanced the dashboard. This 
is server side stuff for so probably not interesting.

### Quick Start

```bash
# Run enhanced dashboard
go run ./cmd/dashboard -redis rediss://your-redis-url -refresh 5s

# Try it out:
# - Press 'v' to toggle verbosity
# - Press 'h' to see help
# - Press 'r' to force refresh
```


Key sections:

- **Step 1**: Collect baseline metrics
- **Step 2**: Analyze current timing (4s fallback, 3 dial attempts, 150ms punch)
- **Step 3**: Identify bottlenecks (quic-timeout, no-response, noise-failed)
- **Step 4**: Tuning recommendations (specific code changes)
- **Step 5**: Systematic testing methodology
- **Step 6**: Advanced diagnostics (logs, Redis, packet capture)
- **Step 7**: Decision framework (when to tune vs accept)





Usage:

```bash
cp docs/P2P-BASELINE-TEMPLATE.md docs/P2P-BASELINE-$(date +%Y%m%d).md
# Fill in with your test results
```

## ️ Analysis Tools



Features:

- Extracts NAT punch statistics
- Identifies direct race results
- Summarizes STUN discovery
- Shows timing breakdown
- Highlights errors/warnings
- Reports connection outcome

Usage:

```bash
# Analyze a transfer log
./scripts/analyze-p2p-logs.sh -l transfer.log

# Save analysis to file
./scripts/analyze-p2p-logs.sh -l transfer.log -o analysis.txt

# Verbose output
./scripts/analyze-p2p-logs.sh -l transfer.log -v
```


### Immediate Action (30 minutes)

1. **Start the enhanced dashboard**
   ```bash
   go run ./cmd/dashboard -redis $WORMZY_METRICS_REDIS
   ```

2. **Run test transfers**
   ```bash
   # Terminal 1
   PAIRING_CODE="$(go run ./cmd/wormzy code)"
   printf 'Pairing code: %s\n' "$PAIRING_CODE"
   go run ./cmd/wormzy recv --code "$PAIRING_CODE" 2>&1 | tee recv.log
   
   # Terminal 2: copy the freshly generated code printed by Terminal 1.
   PAIRING_CODE='<code-from-terminal-1>'
   go run ./cmd/wormzy send test.bin --code "$PAIRING_CODE" 2>&1 | tee send.log
   ```

3. **Check the dashboard**
   - Look at the P2P indicator 
   - Press `v` for verbose mode
   - Press `h` to see help
   - Note the DirectOutcome distribution

4. **Analyze logs**
   ```bash
   ./scripts/analyze-p2p-logs.sh -l recv.log
   ./scripts/analyze-p2p-logs.sh -l send.log
   ```

### Data Collection Phase (1-2 hours)

1. **Copy the baseline template**
   ```bash
   cp docs/P2P-BASELINE-TEMPLATE.md docs/P2P-BASELINE-$(date +%Y%m%d).md
   ```

2. **Run 10 transfers in each scenario:**
   - Same LAN (should be 100% P2P)
   - Different networks (target 70%+ P2P)
   - Mobile hotspot
   - VPN (optional)

3. **Fill in the template** with dashboard metrics

4. **Identify bottleneck** using the guide

### Optimization Phase (if needed)

**Only if P2P < 70%**, make ONE targeted change:

**Example: High quic-timeout**
```go
// In internal/transport/transport.go line 52
relayFallbackDelay = 6 * time.Second  // was 4s
```

**Example: Need more attempts**
```go
// In internal/transport/transport.go after line 438
for i, target := range directTargets {
    launchDial(target, baseDelay+2400*time.Millisecond+time.Duration(i)*120*time.Millisecond, 4)
}
```

Then re-test and compare metrics!

##  Success Criteria

### Healthy System

-  P2P rate: 70-80%+ overall
-  Same-LAN: 100% P2P via `local` candidate
-  DirectOutcome: Mostly `won`
-  Minimal `no-response` errors

### Red Flags

-  Same-LAN using relay (BUG)
-  P2P rate < 50%
-  High `no-response` (STUN issue)
-  All `quic-timeout` (timing too aggressive)

##  Current Implementation Strengths

The existing code is already sophisticated:

-  3 concurrent dial attempts per candidate
-  Staggered timing (0ms, 700ms, 1500ms)
-  LAN detection (same public IP → prefer local)
-  Sequential probes of a shuffled STUN list on the shared legacy socket
-  Pion ICE first, using Pion-owned sockets and candidate gathering
-  Delayed, cancellable UPnP fallback for the legacy socket
-  4s graceful relay fallback
-  150ms NAT punch packets
-  Role-based path preference (prevents split-brain)

**Don't over-optimize without data!**


##  Key Concepts Explained

### DirectOutcome Values

- `won` - Direct P2P succeeded
- `quic-timeout` - Direct timed out → used relay
- `no-response` - No UDP candidates (STUN failed)
- `noise-failed` - QUIC worked but crypto failed

### Candidate Types

- `reflexive` - Public IP from STUN (typical NAT-to-NAT)
- `upnp` - Validated router mapping for the legacy path after ICE failure
- `local` - LAN address (same network)
- `relay` - Fallback relay server
- `loopback` - Local testing only

### Timing Constants (transport.go)

- Progressive UPnP delay: `1.5s` - Timer starts immediately before Pion connectivity checks; maps only while ICE remains unresolved
- `relayFallbackDelay = 4s` - When to try relay
- `relayRetryDelay = 3s` - Delay between relay retries
- `relayAttemptTimeout = 6s` - Individual relay timeout
- Punch interval: `150ms` - NAT hole punch frequency

##  Expected Results

### Before Optimization (Baseline)

- Collect 20-30 transfers across scenarios
- Document P2P% and DirectOutcome distribution
- Identify patterns (same-LAN? quic-timeout common?)

### After Tuning (If Needed)

- Compare P2P% before/after
- Verify no regressions (new errors?)
- Document what changed and why
- Commit with clear message

### Target Metrics

- **70-80% P2P** in typical home/mobile = Excellent
- **100% P2P** same-LAN = Required
- **Low `quic-timeout`** (<20%) = Healthy
- **Minimal `no-response`** (<10%) = Good STUN



**The framework emphasizes data collection before optimization** - measure first, tune second!
