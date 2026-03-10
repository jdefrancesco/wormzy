# Wormzy

Wormzy aims to be simple and secure way to share large files with another party.

Wormzy allows users to send files directly to another party with ease. Its primary features 
include:

* Send file of any size peer-to-peer. No need to change NAT rules. 
* Communication is secure/encrypted
* Utilizes QUIC for fast transfers.


## Quick Start

Wormzy automatically spins up an embedded Redis mailbox for local development, so you can run both peers on your laptop without any extra setup:

```bash
go run ./cmd/wormzy send ./big.bin
# => displays a pairing code such as f7p9-x2
```

```bash
go run ./cmd/wormzy recv
# prompted for the pairing code, then the file arrives
```

When you deploy a managed Redis instance (recommended), set the endpoint via an env var instead of sprinkling `-relay` flags:

```bash
export WORMZY_RELAY_URL="rediss://default:<password>@redis-12345.c1.us-east-1-2.ec2.cloud.redislabs.com:25061"
go run ./cmd/wormzy send ./big.bin
go run ./cmd/wormzy recv
```

`WORMZY_RELAY_URL` takes precedence over CLI flags; fall back to `WORMZY_RELAY` or `-relay` only when you need to override the default.

### Hosting the relay on DigitalOcean Managed Redis

1. In the DigitalOcean dashboard, create a **Managed Database → Redis** cluster (the smallest plan is plenty). Choose the region closest to your users.
2. Once provisioned, grab the `rediss://` connection string from the “Connection Details” panel (it includes the username, password, host, and TLS port).
3. Export that URL on both peers before running Wormzy:
   ```bash
   export WORMZY_RELAY_URL="rediss://default:<password>@primary-do-redis.example.com:25061"
   ```
4. Run `wormzy send <file>` / `wormzy recv` as usual. The CLI will use your managed Redis mailbox automatically; no additional flags required.

You can still override the relay per invocation (`wormzy send -relay rediss://...`) and the embedded Redis fallback remains available for offline demos.
 
### Architecture

* Connection flow: rendezvous → key agreement → direct QUIC attempt → relay fallback (timeouts sane?).
* UDP hole-punch sequencing: simultaneous open, STUN timeout/backoff, public/peer-reflexive addr preference.
* QUIC config: MaxStreamReceiveWindow / MaxIncomingStreams, connection keep-alive, idle timeout, 0-RTT disabled unless rekeyed.

### Crypto 

* SPAKE2/PAKE or Noise handshake? Correct domain separation and transcript binding to the “code.”
* Key derivation: scrypt/argon2 parameters (N/r/p or memory/iters), per-session salt/psk; AEAD = XChaCha20-Poly1305 or AES-GCM with random nonce.
* Code entry UX: prevent code reuse/replay; verify both peers display the same short authentication string (SAS).

### File transfer & integrity

* Chunking & reusability: content length vs. sparse files, per-chunk MAC or global MAC+Merkle; final hash (BLAKE3/SHA-256) compared out-of-band.
* Back pressure: bounded queues; avoid unbounded goroutine fan-out.
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


## Security Policy


## Reporting a Vulnerability

Please email jdefr89@gmail.com

## Hardening Notes

- PAKE: SPAKE2 over Ed25519; KDF: scrypt (N=2^15, r=8, p=1).
- AEAD: XChaCha20-Poly1305; per-message nonce is random 24 bytes.
- Disable QUIC 0-RTT; pin rendezvous TLS hostname; rotate STUNs.
