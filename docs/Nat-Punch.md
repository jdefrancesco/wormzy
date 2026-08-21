# NAT Traversal and NAT Table Evolution

This document explains UDP NAT traversal using STUN plus a separate rendezvous channel, including:

- how peers learn each other’s public endpoint
- how UDP hole punching works
- NAT table evolution step-by-step
- a side-by-side comparison of a successful case vs. a symmetric NAT failure case


## Classic UDP Punching Model

Each client:

1. Creates a UDP socket for the traversal path
2. Sends a binding request to a **STUN server** from that socket
3. The STUN server returns the **public source IP:port** it observes
4. A separate rendezvous channel exchanges those observed public endpoints between the two peers
5. Both peers start sending UDP punch packets to the public endpoint they were given
6. If both NATs are friendly enough, the packets cross and direct connectivity is established

Within this classic path, the most important rule is:

> Use the same UDP socket for STUN, punching, and later peer traffic on that path.

If you create a different socket later, the NAT may assign a different mapping and the peer will be sending to the wrong place.

## Wormzy's Two Socket Paths

Wormzy does not use one UDP socket for every traversal strategy. Its pairing code and candidate metadata travel through the HTTPS mailbox, not through a UDP rendezvous packet. A direct Redis mailbox is available only over loopback or a local Unix socket for development:

- Pion ICE owns its own sockets and first gathers host, server-reflexive, and explicitly configured TURN candidates. Pion then performs the first connectivity checks; a configured TURN candidate may win during this initial attempt.
- Wormzy separately binds a legacy UDP socket. `DiscoverOnConn` probes a shuffled STUN server list sequentially on that socket so its reflexive address remains valid for the later legacy punch and QUIC path.
- Immediately before Pion calls `Dial` or `Accept`, Wormzy arms a 1.5-second timer. If ICE is still unresolved when it expires, Wormzy may map the legacy socket with UPnP. The mapping is not injected into Pion ICE.
- If ICE succeeds, the UPnP attempt is canceled and any mapping is removed. If ICE fails, peers authenticate readiness and refreshed candidate snapshots with the CPace-derived key before the legacy direct race uses a validated UPnP candidate.
- The custom Wormzy relay is tried after the direct paths fail. It is distinct from an explicitly configured TURN server inside Pion ICE.


## What the STUN Server Does...

When each client sends to a STUN server, the server sees the packet after NAT translation.

For example:

- Alice local socket: `10.0.0.10:40000`
- NAT-A public IP: `198.51.100.10`

Alice sends to STUN:

```text
src = 10.0.0.10:40000
dst = 198.51.100.200:3478
```

NAT-A may rewrite that to:

```text
src = 198.51.100.10:62000
dst = 198.51.100.200:3478
```

So the STUN server returns:

```text
Alice = 198.51.100.10:62000
```

That public endpoint is what gets shared with Bob.

## Successful UDP Punch 

Assume a public STUN server plus Wormzy's mailbox rendezvous.

### Actors

#### Alice side

- Alice host private IP: `10.0.0.10`
- Alice UDP socket bound to: `10.0.0.10:40000`

#### Bob side

- Bob host private IP: `192.168.1.20`
- Bob UDP socket bound to: `192.168.1.20:50000`

#### NATs

- NAT-A public IP: `198.51.100.10`
- NAT-B public IP: `203.0.113.20`

<br />

##  NAT Punch In Detail - *if you care*

Neither NAT has a mapping yet obviously...

### NAT-A table

```text
(empty)
```

### NAT-B table

```text
(empty)
```

> As you'd expect. No outside host can send directly to Alice or Bob yet.

## Step 1: Alice talks to STUN

Alice sends a UDP packet from this traversal socket:

```text
src = 10.0.0.10:40000
dst = 198.51.100.200:3478
payload = "STUN binding request"
```

NAT-A creates a public mapping. Suppose NAT-A chooses public port `62000`.

So NAT-A rewrites the packet to:

```text
src = 198.51.100.10:62000
dst = 198.51.100.200:3478
```

### NAT-A table now

```text
Inside                    Outside/Public            Allowed remote
10.0.0.10:40000    <->    198.51.100.10:62000      (at least the STUN server)
```

### STUN server observes

```text
Alice appears as 198.51.100.10:62000
```

## Step 2: Bob talks to STUN

Bob sends from his traversal socket:

```text
src = 192.168.1.20:50000
dst = 198.51.100.200:3478
payload = "STUN binding request"
```

NAT-B creates a mapping. Suppose it chooses public port `61000`.

Packet becomes:

```text
src = 203.0.113.20:61000
dst = 198.51.100.200:3478
```

### NAT-B table now

```text
Inside                     Outside/Public           Allowed remote
192.168.1.20:50000   <->   203.0.113.20:61000      (at least the STUN server)
```

### STUN server observes

```text
Bob appears as 203.0.113.20:61000
```

## Step 3: the mailbox exchanges endpoints

### Server now sends:

#### To Alice

```text
Bob public endpoint = 203.0.113.20:61000
```

#### To Bob

```text
Alice public endpoint = 198.51.100.10:62000
```

At this point...

#### NAT-A table

```text
10.0.0.10:40000  <->  198.51.100.10:62000   remote seen: STUN
```

#### NAT-B table

```text
192.168.1.20:50000 <-> 203.0.113.20:61000   remote seen: STUN
```

## Step 4: Alice sends first punch to Bob

Alice uses the same socket `10.0.0.10:40000` and sends:

```text
src = 10.0.0.10:40000
dst = 203.0.113.20:61000
payload = "punch"
```

For a friendly NAT, NAT-A keeps the same public mapping:

```text
10.0.0.10:40000  ->  198.51.100.10:62000
```

So packet on the Internet becomes:

```text
src = 198.51.100.10:62000
dst = 203.0.113.20:61000
payload = "punch"
```

### NAT-A table after Alice punch

```text
Inside                    Outside/Public            Remote destinations contacted
10.0.0.10:40000    <->    198.51.100.10:62000      STUN, 203.0.113.20:61000
```


## Step 5: Bob sends first punch to Alice

At nearly the same time, Bob sends from his same socket `192.168.1.20:50000`:

```text
src = 192.168.1.20:50000
dst = 198.51.100.10:62000
payload = "punch"
```

Friendly NAT-B preserves the same public mapping:

```text
192.168.1.20:50000 -> 203.0.113.20:61000
```

So on the Internet the packet becomes:

```text
src = 203.0.113.20:61000
dst = 198.51.100.10:62000
payload = "punch"
```

### NAT-B table after Bob punch

```text
Inside                     Outside/Public           Remote destinations contacted
192.168.1.20:50000   <->   203.0.113.20:61000      STUN, 198.51.100.10:62000
```


### Critical Crossing Moment

**Now** both NATs have seen outbound traffic from their inside host to the other peer’s public endpoint.
That means each NAT may now allow return traffic from that peer. This is **the hole**!

## Step 6: the first crossed packet arrives

Let’s say Bob’s punch packet reaches NAT-A first.

> Packet arriving at NAT-A from Internet:

```text
src = 203.0.113.20:61000
dst = 198.51.100.10:62000
payload = "punch"
```

NAT-A checks:

- do I have a mapping for public port `62000`? yes
- does this remote source match an allowed/expected peer for that mapping? now yes, because Alice already sent to `203.0.113.20:61000`

> So NAT-A forwards inward to Alice:

```text
src = 203.0.113.20:61000
dst = 10.0.0.10:40000
payload = "punch"
```

> Alice receives Bob’s punch.

> That is the first successful crossed punch packet.


## Step 7: Alice replies with ACK/data

> Alice now sends back using the same socket:

```text
src = 10.0.0.10:40000
dst = 203.0.113.20:61000
payload = "punch-ack"
```

> NAT-A rewrites:

```text
src = 198.51.100.10:62000
dst = 203.0.113.20:61000
```

> NAT-B sees this arrive, checks its mapping for public port `61000`, sees Bob already sent to Alice, and forwards inward:

```text
src = 198.51.100.10:62000
dst = 192.168.1.20:50000
payload = "punch-ack"
```

> Now both sides know direct connectivity works.

## Final working state

### NAT-A table

```text
Inside                    Outside/Public            Active remote peer
10.0.0.10:40000    <->    198.51.100.10:62000      203.0.113.20:61000
```

### NAT-B table

```text
Inside                     Outside/Public           Active remote peer
192.168.1.20:50000   <->   203.0.113.20:61000      198.51.100.10:62000
```

>From here on, direct UDP traffic can flow both ways as long as the mappings stay alive.


## NAT Punch Failure

The key problem:

> The NAT uses a different public mapping depending on destination.

### Failure case: symmetric NAT

>Alice’s NAT uses a different public mapping per destination.

```text
Alice local socket 10.0.0.10:40000

to STUN        -> 198.51.100.10:62000
to Bob         -> 198.51.100.10:63055
```

>That is why the STUN-observed mapping shared with Bob is no longer transferable.

## TLDR

**Again, in summary because all this NAT nonsense can fry your brain...**

### Wormzy's order:

1. Claim and display the pairing code through the mailbox
2. Exchange initial local and reflexive metadata, then derive the CPace key
3. Try Pion ICE on Pion-owned sockets
4. If ICE remains unresolved 1.5 seconds after connectivity checks begin, try UPnP on the separate legacy socket
5. If ICE fails, authenticate refreshed candidates and start the legacy punch race
6. Fall back to the custom relay for symmetric NAT, CGNAT, or blocked cases


## But... Remember

> Do whatever you can so `wormzy` can establish a fast P2P link you can use to send the latest bloated AI model with!
