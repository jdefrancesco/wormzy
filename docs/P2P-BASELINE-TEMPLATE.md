# P2P Baseline Metrics Collection

## Test Environment

**Date**: YYYY-MM-DD  
**Commit**: `git rev-parse --short HEAD`  
**Go Version**: `go version`  
**OS/Arch**: 

**Relay Server**: (URL)  
**Redis URL**: (URL)

## Network Scenarios Tested

### Scenario 1: Same LAN
- [ ] 10 transfers completed
- Network: Home WiFi / Office LAN / etc.
- Expected: 100% P2P via `local` candidate

### Scenario 2: NAT-to-NAT (Different Networks)
- [ ] 10 transfers completed  
- Network A: Home WiFi
- Network B: Different home/mobile
- Expected: 70-90% P2P via `reflexive` candidate

### Scenario 3: Mobile Hotspot
- [ ] 5 transfers completed
- Network: Mobile carrier hotspot
- Expected: 40-70% P2P (carrier NAT dependent)

### Scenario 4: VPN to Direct
- [ ] 5 transfers completed
- Network A: VPN connection
- Network B: Direct internet
- Expected: Variable (depends on VPN NAT)

## Dashboard Snapshot

### Overall Stats
```
Total Sessions:      ___
Active Sessions:     ___
Completed:           ___
Failed:              ___

P2P Transfers:       ___ (___%)
Relay Transfers:     ___ (___%)

Total Data:          ___ GB
Avg Duration:        ___
Avg Throughput:      ___ MB/s
```

### Direct Outcome Distribution
```
won:                 ___ (___%)
quic-timeout:        ___ (___%)
no-response:         ___ (___%)
noise-failed:        ___ (___%)
```

### Candidate Distribution
```
reflexive:           ___ (___%)
local:               ___ (___%)
relay:               ___ (___%)
loopback:            ___ (___%)
```

### Top 3 Error Types
```
1. _______________ : ___
2. _______________ : ___
3. _______________ : ___
```

## Detailed Breakdown by Scenario

### Same LAN Results

| Test # | Outcome | Transport | Candidate | Duration | Size  | Notes |
|--------|---------|-----------|-----------|----------|-------|-------|
| 1      |         |           |           |          |       |       |
| 2      |         |           |           |          |       |       |
| 3      |         |           |           |          |       |       |
| 4      |         |           |           |          |       |       |
| 5      |         |           |           |          |       |       |
| 6      |         |           |           |          |       |       |
| 7      |         |           |           |          |       |       |
| 8      |         |           |           |          |       |       |
| 9      |         |           |           |          |       |       |
| 10     |         |           |           |          |       |       |

**Summary**: ___% P2P

**Issues**:
- [ ] Any relay usage on same LAN? (RED FLAG)
- [ ] Consistent timing?

### NAT-to-NAT Results

| Test # | Outcome | Transport | Candidate | Duration | Size  | Notes |
|--------|---------|-----------|-----------|----------|-------|-------|
| 1      |         |           |           |          |       |       |
| 2      |         |           |           |          |       |       |
| 3      |         |           |           |          |       |       |
| 4      |         |           |           |          |       |       |
| 5      |         |           |           |          |       |       |
| 6      |         |           |           |          |       |       |
| 7      |         |           |           |          |       |       |
| 8      |         |           |           |          |       |       |
| 9      |         |           |           |          |       |       |
| 10     |         |           |           |          |       |       |

**Summary**: ___% P2P

**Issues**:
- STUN discovery failures?
- Consistent directOutcome for failures?

### Mobile Hotspot Results

| Test # | Outcome | Transport | Candidate | Duration | Size  | Notes |
|--------|---------|-----------|-----------|----------|-------|-------|
| 1      |         |           |           |          |       |       |
| 2      |         |           |           |          |       |       |
| 3      |         |           |           |          |       |       |
| 4      |         |           |           |          |       |       |
| 5      |         |           |           |          |       |       |

**Summary**: ___% P2P

**Issues**:
- Carrier NAT type?
- UDP blocking?

### VPN Results

| Test # | Outcome | Transport | Candidate | Duration | Size  | Notes |
|--------|---------|-----------|-----------|----------|-------|-------|
| 1      |         |           |           |          |       |       |
| 2      |         |           |           |          |       |       |
| 3      |         |           |           |          |       |       |
| 4      |         |           |           |          |       |       |
| 5      |         |           |           |          |       |       |

**Summary**: ___% P2P

**Issues**:
- VPN type? (WireGuard, OpenVPN, etc.)

## Log Analysis

### Sample Punch Statistics

```bash
$ grep "punch/stop" logs.txt
[Paste representative samples]
```

**Observations**:
- Typical rounds before QUIC-up: ___
- Typical packets sent: ___
- Any error patterns?

### Sample Direct Race Results

```bash
$ grep "direct race" logs.txt
[Paste representative samples]
```

**Observations**:
- Which attempt typically wins (1, 2, 3)?
- Timing from first dial to connection?

### Relay Fallback Triggers

```bash
$ grep "falling back to relay" logs.txt
[Paste representative samples]
```

**Observations**:
- How often?
- After how many direct attempts?

## Identified Issues

### High Priority
1. 
2. 
3. 

### Medium Priority
1. 
2. 

### Low Priority
1. 

## Hypotheses for Improvement

Based on the data above:

1. **If P2P < 60% overall**: 
   - [ ] Extend relay fallback delay?
   - [ ] More dial attempts?
   - [ ] Check STUN reliability?

2. **If same-LAN uses relay**:
   - [ ] BUG: Check `preferLocal` logic in `dilation.go`
   - [ ] BUG: Check candidate sorting

3. **If quic-timeout is common but connections work eventually**:
   - [ ] Increase relay fallback delay from 4s to 6s+
   - [ ] Add 4th or 5th dial attempt

4. **If no-response is common**:
   - [ ] STUN server issues
   - [ ] Network blocks UDP
   - [ ] Check STUN retry logic

5. **If noise-failed is common**:
   - [ ] Not a NAT issue - check crypto/versioning

## Proposed Next Steps

Priority order:

1. [ ] ...
2. [ ] ...
3. [ ] ...

## Notes

[Any additional observations, environmental factors, weird behaviors, etc.]
