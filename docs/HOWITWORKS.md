# How Wormzy Works

Wormzy is a peer-to-peer file sender built around three subsystems: the CLI/UI (`cmd/wormzy` and `internal/ui`), the rendezvous/transport layer (`internal/transport`), and an optional HTTP mailbox proxy (`cmd/mailbox`, `internal/transport/mailbox_*`). End users run exactly two binaries—`wormzy send <file>` and `wormzy recv`—while infrastructure hosts the mailbox proxy that fronts a managed Redis mailbox.

## Session Flow

1. **CLI + Bubble Tea UI**  
   `cmd/wormzy` parses flags, prompts for a pairing code (receiver), and initializes a Bubble Tea UI. The UI watches stage updates so users see STUN, rendezvous, Noise+QUIC, and Transfer progress. When a run completes, it stays on screen with the file path, size, and BLAKE3-256 hash until the user presses `q`. Use `-log-file path` for a detailed trace.

2. **Rendezvous & PAKE**  
   `internal/transport.Run` binds a UDP socket, probes STUN servers, and collects local + reflexive candidates. It then opens a mailbox (`internal/transport/mailbox.go` for Redis, or `mailbox_http_client.go` for HTTP) and executes the pairing exchange: claim/generate a code, publish “self” info, and run CPace over the mailbox to derive a shared secret. The redis-backed mailbox lives in managed Redis; the HTTP proxy (`cmd/mailbox`, `internal/transport/mailbox_http_server.go`) exposes `/v1/claim`, `/v1/self`, etc. so clients never talk to Redis directly.

3. **NAT Punching & QUIC**  
   Once peers learn each other’s addresses, `internal/transport/dilation.go` prioritizes LAN candidates when both share the same public IP (common home-network scenario), otherwise falls back to the reflexive address. Both sides send “punch” packets while simultaneously dialing/listening for QUIC. If the socket can’t bind, use `-dev-loopback` to simulate on localhost.

4. **Noise Handshake & Encrypted File Stream**  
   On top of QUIC, Wormzy runs a Noise NN handshake seeded with the PAKE key. A derived symmetric key drives an XChaCha20-Poly1305 writer/reader pair (`sendFileEncrypted` / `receiveFile`). The sender streams the file via a QUIC uni-stream, appends a metadata trailer (size, chunk size, BLAKE3 digest), and the receiver verifies bytes before writing to disk.

## Configuration Points

- `-relay` (or `WORMZY_RELAY[_URL]`) selects Redis vs. HTTP relay.
- `-timeout`, `-show-network`, `-log-file`, and `-dev-loopback` customize behavior.
- `cmd/mailbox` runs as `wormzy-mailbox` on your infrastructure; point it at your managed Redis string.

## TLDR

Pairing Code
    ↓
CPace PAKE
    ↓
Shared Secret
    ↓
Noise NN Handshake
    ↓
Session Keys
    ↓
XChaCha20-Poly1305 Stream
    ↓
BLAKE3 Verification