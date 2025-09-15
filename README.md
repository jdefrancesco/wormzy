# Wormzy 

A simple, secure, and fast way to send files of any size to someone.

Similar to magic-wormhole 


 
### Architecture

* Connection flow: rendezvous → key agreement → direct QUIC attempt → relay fallback (timeouts sane?).
* UDP hole-punch sequencing: simultaneous open, STUN timeout/backoff, public/peer-reflexive addr preference.
* QUIC config: MaxStreamReceiveWindow / MaxIncomingStreams, keepalives, idle timeout, 0-RTT disabled unless rekeyed.

### Crypto 

* SPAKE2/PAKE or Noise handshake? Correct domain separation and transcript binding to the “code.”
* Key derivation: scrypt/argon2 parameters (N/r/p or memory/iters), per-session salt/psk; AEAD = XChaCha20-Poly1305 or AES-GCM with random nonce.
* Code entry UX: prevent code reuse/replay; verify both peers display the same short authentication string (SAS).

### File transfer & integrity

* Chunking & resumability: content length vs. sparse files, per-chunk MAC or global MAC+Merkle; final hash (BLAKE3/SHA-256) compared out-of-band.
* Backpressure: bounded queues; avoid unbounded goroutine fan-out.
* Zero-copy paths where possible (`io.CopyBuffer`, `sendfile` on relay path).

### NAT/relay (PUNCHING)

* STUN servers list + rotation; exponential backoff; TURN/relay only after X failed punches/timeout.
* Relay TLS pinning (or at least hostname verification) and rate limiting.

### Safety & privacy

* No plaintext metadata: filename/size encrypted or consented; consider “receive-to-tempname, rename after hash ok.”
* Ephemeral logs; redact codes/keys on error; optional `--debug` gated logs.

### DX & packaging

* Single static binary (enable CGO off if possible); cross-compile targets.
* CLI UX: progress bars, ETA, retries; `wormzy send <file>`, `wormzy recv <code>`.
* Config/env: `WORMZY_RENDEZVOUS`, `WORMZY_STUN`, `WORMZY_RELAY`.

---

#### SECURITY.md (skeleton)

```markdown
# Security Policy

## Supported Versions
MVP: latest main branch.

## Reporting a Vulnerability

Please email jdefr89@gmail.com

## Hardening Notes

- PAKE: SPAKE2 over Ed25519; KDF: scrypt (N=2^15, r=8, p=1).
- AEAD: XChaCha20-Poly1305; per-message nonce is random 24 bytes.
- Disable QUIC 0-RTT; pin rendezvous TLS hostname; rotate STUNs.
