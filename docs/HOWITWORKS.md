# How `Wormzy` Works 

This document is the explanation that is sort of *"just beyond TLDR"*

**`Wormzy`** is a peer-to-peer file sender built around three (maybe four..) subsystems: the CLI/UI (`cmd/wormzy` and `internal/ui`), the rendezvous/transport layer (`internal/transport`), and an optional HTTP mailbox proxy (`cmd/mailbox`, `internal/transport/mailbox_*`). User sends file with `wormzy` command —`wormzy send <file>` and `wormzy recv`; while infrastructure hosts the mailbox proxy that fronts a managed Redis mailbox.

## Session Flow

1. **CLI + TUI**  

   `cmd/wormzy` parses flags, prompts for a pairing code (receiver), and initializes the UI. The UI watches stage updates so users see STUN, rendezvous, Noise+QUIC, and Transfer progress. Detailed logs are hidden by default; use `--logs` to show them in the UI or `--log-file path` to save them. When a run completes, it stays on screen with the file path, size, and BLAKE3-256 hash until the user presses `q`.

2. **Pairing, Discovery & PAKE**

   `internal/transport.Run` creates or validates the pairing secret locally and displays it immediately. The secret never becomes the mailbox lookup key: Wormzy derives an opaque, domain-separated session identifier and creates a separate random capability for each role. The HTTP mailbox stores only capability verifiers and requires the raw capability over HTTPS for later operations. It then binds the legacy UDP socket, probes a shuffled STUN server list sequentially on that socket, and publishes its initial local and reflexive candidates. CPace runs over the mailbox to derive the shared secret before either peer trusts refreshed candidate metadata. The Redis-backed mailbox lives in managed Redis; the HTTP proxy (`cmd/mailbox`, `internal/transport/mailbox_http_server.go`) exposes the versioned mailbox API so production clients never talk to Redis directly.

3. **Progressive NAT Traversal & QUIC**

   Wormzy tries Pion ICE first. Pion owns the sockets used for its host, server-reflexive, and explicitly configured TURN candidates; these are separate from Wormzy's legacy UDP socket. Production same-LAN connectivity is handled by authenticated Pion ICE checks; a peer's private host candidate is accepted only when it belongs to one of the local machine's directly connected private IPv4 subnets. Immediately before Pion begins `Dial`/`Accept` connectivity checks, Wormzy arms a 1.5-second timer. If ICE is still unresolved when it expires, Wormzy starts a cancellable UPnP mapping for the legacy socket. An ICE win cancels the attempt and removes any completed mapping. If ICE fails, both peers publish their updated candidate snapshots, authenticate readiness with the CPace-derived key, verify the refreshed snapshots, and then run the legacy direct race, which prefers a validated UPnP candidate over a reflexive candidate. An explicitly configured TURN candidate can win inside the initial ICE attempt; Wormzy's custom relay is the final fallback. The relay v2 registration uses separate PAKE-derived session and token values and a dedicated ALPN, so incompatible relay versions fail closed. Use `--dev-loopback` to simulate the legacy path on localhost.

4. **Noise Handshake & Encrypted File Stream**

   On top of QUIC, both sides first complete a bounded challenge-response proving possession of the CPace key. This prevents an unrelated QUIC racer from being selected and then stalling the Noise or file stream. Wormzy then runs Noise NN and derives the XChaCha20-Poly1305 file key from the PAKE key and Noise transcript. Stream setup uses caller cancellation and deadlines. The sender streams the file via a QUIC uni-stream, appends a metadata trailer (size, chunk size, BLAKE3 digest), and the receiver verifies bytes before writing to disk. Current peers then exchange a bounded, file-key-authenticated completion receipt bound to the pairing code, file size, and digest before either side reports success. Only the protocol role and MAC travel on this control stream, so a custom relay cannot read the raw digest. Wormzy fails closed with upgrade guidance when a peer lacks this protocol; it never treats an unauthenticated QUIC close as proof of receipt.

## Configuration Points

- `--relay` (or `WORMZY_RELAY[_URL]`) selects an HTTPS mailbox. Direct Redis is
  accepted only over loopback or a local Unix socket for development.
- `--timeout`, `--show-network`, `--logs`, `--log-file`, `--dev-loopback`, and `--no-upnp` customize behavior.
- `WORMZY_UPNP=0` disables automatic UPnP mapping in environments where router discovery is not wanted.
- `cmd/mailbox` runs as `wormzy-mailbox` on your infrastructure; point it at your managed Redis string.

### Mailbox and relay trust

The official service is the default. A different mailbox or relay cannot read
the Noise-encrypted file or forge PAKE-authenticated signaling when both peers
use a strong Wormzy-generated code. It can still see connection metadata,
withhold or reorder traffic, and deny service. A mailbox also sees the candidate
metadata needed for traversal. Treat endpoint overrides as a metadata and
availability trust decision, keep Redis private, and deploy protocol-v2 servers
before protocol-v2 clients; Wormzy does not silently downgrade to the older
registration formats.

### NAT Traversal

NAT Traversal is key to creating a secure P2P connection that data is sent over. NAT traversal is not guaranteed. Some configurations, like symmetric NAT, make *NAT Punching* difficult or impossible. When this happens `wormzy` is forced to fall back to a relay. The general flow is as follows:

1. Mailbox: claims and displays the pairing code
2. STUN and local discovery: publishes the initial candidate snapshot
3. CPace: authenticates the peers and derives their shared secret
4. Pion ICE: runs connectivity checks on Pion-owned sockets (including TURN only when explicitly configured)
5. Delayed UPnP: starts 1.5 seconds after ICE checks begin and maps the separate legacy UDP socket only while ICE remains unresolved
6. Authenticated refresh and legacy punch race: used only after ICE fails
7. Custom relay fallback: used if no direct path works
8. Noise/QUIC: encrypts the file transfer over the selected path and authenticates completion

UPnP's HTTP description and SOAP requests are pinned to the private-subnet
device that answered SSDP; redirects, proxies, DNS names, and cross-host URLs
are rejected. SSDP source addresses can still be spoofed by a hostile local
network, so disable UPnP with `--no-upnp` when the LAN itself is untrusted.

### TLDR

Pairing Code -> CPace PAKE -> Shared Secret -> P2P -> Noise NN Handshake -> Session Keys -> XChaCha20-Poly1305 Stream -> Blake3 Verification -> Your file where you want it...

### Detailed Subsystem Docs

- [NAT-PUNCH](/docs/Nat-Punch.md)
- [P2P-README](/docs/P2P-README.md)


#### Relevant RFCs

- [ICE-PAC](https://datatracker.ietf.org/doc/html/rfc8863)
- [STUN](https://datatracker.ietf.org/doc/html/rfc5389)
- [ICE/NAT](https://datatracker.ietf.org/doc/html/rfc8445)
