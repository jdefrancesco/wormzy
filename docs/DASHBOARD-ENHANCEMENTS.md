# Dashboard Enhancements Summary

## Overview

The dashboard has been significantly enhanced to provide more verbose and actionable information for P2P optimization.

## New Features

### 1. **Visual P2P Success Rate Indicator**

The dashboard now prominently displays P2P success rate with color-coded indicators:

- 🟢 **80%+ (EXCELLENT)** - P2P working great!
- 🟢 **70-80% (GOOD)** - Healthy P2P rate
- 🟡 **50-70% (FAIR)** - Room for improvement
- 🟠 **30-50% (POOR)** - P2P struggling - check logs
- 🔴 **<30% (CRITICAL)** - P2P mostly failing - investigate

This gives you an at-a-glance understanding of your P2P health.

### 2. **Verbose Mode Toggle**

Press `v` to toggle between verbose and compact modes:

**Verbose mode shows:**
- Candidate type for each session (reflexive, local, relay)
- Detailed waiting states (waiting recv/send)
- Avg duration and throughput
- Expanded direct outcomes with percentages
- Full candidate usage breakdown
- More failure details (8 instead of 4)
- Longer error messages (80 chars vs 56)

**Compact mode shows:**
- Essential metrics only
- Aggregated summaries
- Perfect for smaller terminals or quick glances

### 3. **Built-in Help Screen**

Press `h` or `?` to view an interactive help screen that explains:
- All keyboard shortcuts
- What each metric means
- P2P success rate thresholds
- Direct outcome definitions
- Candidate type explanations
- Optimization tips with actionable advice
- Link to detailed documentation

### 4. **Percentage-Based Metrics**

- P2P vs Relay now shows percentages: `P2P: 15 (75.0%)`
- Direct outcomes show distribution percentages in verbose mode
- Makes it easier to spot trends at a glance

### 5. **Enhanced Debug Panel**

Now titled "🔍 Debug Information" with:
- Direct outcomes listed individually with percentages (verbose mode)
- Candidates used listed individually with counts (verbose mode)
- More detailed failure information
- Better visual hierarchy

### 6. **Improved Session Tables**

Verbose mode adds a "Candidate" column showing which connection type was used:
```
Code         State        Size       Duration   Candidate    TTL left
test-123     P2P          5.2 MiB    3s         reflexive    8m32s
```

### 7. **Clearer Footer**

Footer now shows:
- Current mode (verbose/compact)
- Available actions
- Example: `Mode: verbose • Press h for help, q to exit`

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `r` | Refresh metrics now (manual refresh) |
| `v` | Toggle verbose/compact mode |
| `h` or `?` | Toggle help screen |
| `q` or `Ctrl+C` | Quit dashboard |

## Usage Examples

### Starting the Dashboard

```bash
# Default (verbose mode enabled by default)
go run ./cmd/dashboard -redis rediss://your-redis-url

# With custom refresh interval
go run ./cmd/dashboard -redis rediss://your-redis-url -refresh 3s

# With custom key prefix
go run ./cmd/dashboard -redis rediss://your-redis-url -prefix wormzy-prod
```

### Workflow

1. **Start dashboard** - Launches in verbose mode
2. **Watch P2P indicator** - Green = good, Red = needs attention
3. **Press `v`** to compact if terminal is small
4. **Press `h`** to view help and understand metrics
5. **Press `r`** to force refresh if needed
6. Monitor "Direct outcomes" and "Candidates used" to diagnose issues

## Reading the Dashboard

### Example Output (Verbose Mode)

```
╭────────────────────────────────────────╮
│ 📊 Overall Statistics                  │
│                                        │
│ 🟢 EXCELLENT  85.7% P2P  (P2P working  │
│ great!)                                │
│                                        │
│ Total sessions    42                   │
│ Active sessions   3                    │
│  • waiting recv   1                    │
│  • waiting send   0                    │
│ Completed         35                   │
│ Failed            4                    │
│ P2P transfers     30 (85.7%)           │
│ Relay transfers   5 (14.3%)            │
│ Data transferred  2.1 GB               │
│ Avg duration      8s                   │
│ Avg throughput    32.5 MB/s            │
╰────────────────────────────────────────╯

╭────────────────────────────────────────╮
│ Recent transfers                       │
│ Code         State  Size    Duration   │
│              Candidate       Updated   │
│ test-abc     P2P    1.2 GiB 12s        │
│              reflexive       2m ago    │
│ test-xyz     Relay  5.2 MiB 3s         │
│              relay           5m ago    │
╰────────────────────────────────────────╯

╭────────────────────────────────────────╮
│ 🔍 Debug Information                   │
│                                        │
│ Direct outcomes:                       │
│   won            30 (75.0%)            │
│   quic-timeout   8 (20.0%)             │
│   no-response    2 (5.0%)              │
│                                        │
│ Candidates used:                       │
│   reflexive      25                    │
│   local          5                     │
│   relay          5                     │
│                                        │
│ Failure causes   timeout=2, noise=1    │
╰────────────────────────────────────────╯
```

## Interpreting Results

### 🟢 Healthy Dashboard (85% P2P)

```
Direct outcomes:
  won           30 (75%)
  quic-timeout   5 (12.5%)
  no-response    3 (7.5%)
  
Candidates:
  reflexive     25  ← NAT-to-NAT working well
  local         10  ← Same-LAN working perfectly
  relay          5  ← Acceptable fallback
```

**Action:** System is working well, no changes needed.

### 🟡 Needs Tuning (55% P2P)

```
Direct outcomes:
  won           22 (40%)
  quic-timeout  30 (55%)  ← HIGH: Direct timing out
  no-response    3 (5%)
  
Candidates:
  reflexive     20
  relay         30  ← Too much relay usage
```

**Action:** Increase `relayFallbackDelay` from 4s to 6s in `transport.go:52`

### 🔴 Critical Issue (20% P2P)

```
Direct outcomes:
  won            8 (15%)
  no-response   40 (75%)  ← CRITICAL: STUN failing
  quic-timeout   5 (10%)
  
Candidates:
  relay         40  ← Forced relay usage
  reflexive      8
```

**Action:** STUN discovery broken. Check:
1. Network blocks UDP?
2. STUN servers reachable?
3. Check `internal/stun/stun.go` server list

## Integration with P2P Optimization

The dashboard works hand-in-hand with the optimization guides:

1. **Collect baseline** using dashboard metrics
2. **Fill in** `P2P-BASELINE-TEMPLATE.md` with dashboard data
3. **Identify bottleneck** using DirectOutcome percentages
4. **Make changes** based on `P2P-OPTIMIZATION-GUIDE.md`
5. **Compare** dashboard before/after metrics

## Technical Details

### State Management

The dashboard uses Bubble Tea's Elm architecture:
- `dashboardModel` holds state (metrics, verbose flag, help flag)
- `Update()` handles keyboard input and metric updates
- `View()` renders based on current state

### Styling

Custom lipgloss styles for visual hierarchy:
- `headerStyle` - Main title (pink/bold)
- `titleStyle` - Section titles (light pink/bold)
- `successStyle` - Positive indicators (green)
- `warningStyle` - Caution indicators (orange)
- `errorStyle` - Critical indicators (red)
- `subtleStyle` - Secondary text (gray)

### Auto-refresh

- Configurable via `-refresh` flag (default: 5s)
- Fetches from Redis on interval
- Manual refresh with `r` key
- Shows "Refreshing…" indicator during fetch

## Future Enhancements

Potential additions:
- [ ] Historical P2P rate graph
- [ ] Export metrics to CSV
- [ ] Alert thresholds (email when P2P <50%)
- [ ] Filter by time range
- [ ] Search/filter sessions by code
- [ ] Drill-down into individual session details
- [ ] Compare two time periods

## Files Modified

- `cmd/dashboard/main.go` - Enhanced with verbose mode, help, and better metrics display

## Related Documentation

- `docs/P2P-OPTIMIZATION-GUIDE.md` - Comprehensive tuning guide
- `docs/P2P-BASELINE-TEMPLATE.md` - Metrics collection template
- `docs/P2P-README.md` - Workflow overview
- `scripts/analyze-p2p-logs.sh` - Log analysis companion tool
