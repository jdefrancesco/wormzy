# Cross-NAT UPnP A/B Test

This experiment measures whether Wormzy's temporary UPnP UDP mappings improve
direct P2P establishment. It requires two machines behind different upstream
NATs. Two machines on one LAN, or two namespaces behind one physical router,
do not test the UPnP path meaningfully because Wormzy will prefer local
candidates.

Good network pairs include two separate home connections, or one home network
and one mobile hotspot. Avoid changing VPN, firewall, Wi-Fi, relay, or TURN
settings during a run.

## Prepare both machines

Build the same commit on both machines:

```bash
make wormzy
```

Create a balanced plan on either machine. Twenty trials per arm is the minimum
recommended run; ten is useful as a smoke test.

```bash
scripts/upnp-ab.sh plan --trials-per-arm 20 --output upnp-plan.csv
```

Copy `upnp-plan.csv` to the other machine. The plan contains high-entropy,
single-use pairing codes and alternates the enabled and disabled arms in an
ABBA order to reduce time drift.

## Run the two endpoints

Start the receiver behind NAT B:

```bash
scripts/upnp-ab.sh run \
  --role recv \
  --plan upnp-plan.csv \
  --workdir ab-recv
```

Start the sender behind NAT A:

```bash
scripts/upnp-ab.sh run \
  --role send \
  --plan upnp-plan.csv \
  --workdir ab-send
```

The endpoints may start in either order. Each trial uses its preselected code
and waits for the matching peer. The disabled arm passes `--no-upnp`; the
enabled arm uses Wormzy's default behavior.

If the run uses a non-default mailbox or TURN deployment, pass the same
`--relay` and authenticated `--turn` values on both machines. TURN URLs use
`turn:user:password@host:port?transport=udp` (percent-escape delimiters inside
the username or password).

## Merge the evidence

Copy one results file so both are available on the same machine, then run:

```bash
scripts/upnp-ab.sh summarize \
  --send-results ab-send/results.csv \
  --recv-results ab-recv/results.csv
```

The summary reports P2P, relay, and failure counts for both arms, as well as
whether the sender, receiver, or either NAT actually granted a mapping.

Interpretation rules:

- If neither machine reports `mapped` during the enabled arm, the result is
  inconclusive rather than evidence against UPnP.
- Compare the P2P rate for `on` against `off`. A positive delta is evidence
  that UPnP helped this particular NAT pair.
- Repeat against several NAT pairs. UPnP cannot normally help through CGNAT,
  an upstream NAT outside the user's control, or routers with UPnP disabled.
- Keep the per-trial logs. A P2P win using the `upnp` candidate is stronger
  evidence than an aggregate rate change alone.
