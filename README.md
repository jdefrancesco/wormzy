# Wormzy

`wormzy` aims to be a simple, fast, and secure method to share files of any size with another party P2P. It's similar to [Magic Wormhole](https://github.com/magic-wormhole/magic-wormhole). It's built on more modern primitives (QUIC/Noise/ChaCha) and aims to be far more secure, portable and user friendly.

**DISCLOSURE**: `wormzy` is agentic augmented. Primary core code I wrote manually to ensure security
and quality of code I wanted. Also not a huge fan of any technical debt. I manually review and test changes. See [AI/LLM Usage](#aillm-usage) 

## Primary Features

* Send file of any size peer-to-peer with zero hassle. That means no need to change NAT rules or port forwarding.
* Communication is secure/encrypted
* Utilizes QUIC for fast transfers. Blake3 is used for validating the entire, uncorrupted file was received!
* See [HOWITWORKS.md](./docs/HOWITWORKS.md) for more detailed descriptions.
* Both the server and client code is Open Source (AGPL-3.0) so users can audit code as they please.

## Why `Wormzy` ...?

- No setup for users: baked-in mailbox endpoint `https://relay.wormzy.io` works out of the box; overrides stay opt-in.
- P2P-first: prioritizes direct UDP/QUIC; relays only as a fallback.
- Human-friendly pairing codes and auto file collision handling. 
- Integrity and privacy: Noise + QUIC with SAS, disk-space preflight, and hash verification.
- Cross-platform CLI with a sleek TUI. Eventually headless mode will be added for scripts/CI *(in progress)*.

## `Wormzy` vs. `Magic Wormhole`

- Built on modern primitives: QUIC, CPace PAKE, Noise NN, XChaCha20-Poly1305, BLAKE3 
- Portable binary. Supports macOS/Linux/Windows. 
- Defaults: Baked-in HTTPS mailbox endpoint, STUN list, best-effort UPnP port mapping, and optional QUIC relay fallback; Magic Wormhole typically needs a relay URL or uses the Python community relay.
- UX: Bubble Tea TUI with headless fallback; Magic Wormhole is plain CLI.

## Quick Start

Wormzy requires Go 1.27 or newer. Install the latest released `wormzy` CLI:

```bash
go install github.com/jdefrancesco/wormzy/cmd/wormzy@latest
```

Verify the installed release:

```bash
wormzy version
```

For automation that needs to preselect a strong code, run `wormzy code` and
pass its single-line output to `wormzy send --code`. Hand-constructed codes may
have far less entropy even when they match the required format.

Go installs the binary into `GOBIN` when configured, or into
`$(go env GOPATH)/bin` otherwise. Ensure that directory is on your `PATH` if
the shell cannot find `wormzy` after installation.

On the sender:

```bash
wormzy send ./big.bin
# displays a newly generated pairing code
```

Wormzy transfers one file per session. Archive or compress a directory before
sending it; the CLI rejects directory paths before starting network setup.

On the receiver (on another terminal/machine):

```bash
wormzy recv
# prompted for the pairing code, then the file arrives
```

By default the receiver saves into the current working directory. Override this with
`wormzy recv -download-dir ~/Downloads` - `Wormzy` will create the directory if needed and
will refuse the transfer up front if the filesystem cannot hold the advertised file size.

## Testing

Run `make test` to exercise all non-mvp packages.

Focused sweeps:

* `make test-transport` — transport unit tests.
* `make test-stun` — STUN socket tests (auto-skip when UDP is blocked).

Full sweep:
- `make test-all` — runs core, transport, and STUN suites.

The STUN tests bind UDP sockets; they will automatically skip on environments that block UDP (for example, some CI or container sandboxes).

Large transfers run with per-stream idle timeouts; stalled sessions abort instead of hanging. To sanity-check on localhost, run the loopback transfer test: `go test -run TestLargeTransferLoopback ./internal/transport` (skipped automatically with `-short`).

To measure whether UPnP improves direct connections across two real NATs, use
the balanced two-host workflow in [`docs/UPNP-AB-TEST.md`](docs/UPNP-AB-TEST.md).

## Versioning

Git tags are Wormzy's version source of truth. `make build` records the nearest
tag, commit SHA, and UTC build time in every binary. A build made exactly at a
tag reports that tag (for example, `v0.2.0`); later development builds include
the commit distance and abbreviated SHA from `git describe`.

Run `wormzy version` or `wormzy --version` to inspect the client. Infrastructure
binaries expose the same information through `-version`, such as
`./bin/mailbox -version` and `./bin/relay -version`.

Release automation can override the detected values with `VERSION`, `COMMIT`,
and `BUILD_DATE` when invoking `make build`.

## Deploying updated binaries

On a server with the `systemd` units installed, run `make deploy`. It builds the
binaries, installs them to `/usr/local/bin`, reloads systemd, restarts
`wormzy-mailbox` and `wormzy-relay`, and disables the obsolete
`wormzy-rendezvous` service if it is present. Current clients use the bounded v2
HTTPS mailbox; do not expose the legacy TCP rendezvous service on port 9999.

Run the operator console anywhere that has privileged access to the production Redis instance:

```bash
WORMZY_METRICS_REDIS='rediss://user:password@redis.example:6379' ./bin/dashboard
```

It displays mailbox and relay heartbeats, current server load, relay bytes, unresolved sessions, and recent transfer outcomes. Press `d` to confirm drain/resume of new sessions, or select an unresolved session with `j`/`k` and press `x` to remove it. See [`docs/OPERATOR-CONSOLE.md`](docs/OPERATOR-CONSOLE.md) for deployment and safety details.

## Endpoint defaults

The CLI ships with the official mailbox/rendezvous endpoint (`https://relay.wormzy.io`). You don’t need to set anything for basic use. To override it, pass `--relay ...` or set `WORMZY_RELAY_URL`. A config file at `$XDG_CONFIG_HOME/wormzy/relay` or `/etc/wormzy/relay` is also honored. Wormzy also tries temporary UDP UPnP port mapping by default; pass `--no-upnp` or set `WORMZY_UPNP=0` to disable it.

Custom remote mailbox URLs must use HTTPS. Plain HTTP is accepted only for
loopback development endpoints. Direct Redis mailbox connections are likewise
restricted to loopback addresses or local Unix sockets for development; remote
clients must use the HTTPS mailbox API.

The pairing secret is generated locally; current clients send the mailbox only
an opaque session identifier and per-role capability proof. File contents stay
end-to-end encrypted even on the fallback relay. A custom mailbox/relay can
still observe connection metadata (such as IP addresses, timing, and transfer
activity), delay or suppress pairing messages, or deny service. Only configure
an endpoint whose operator you trust with that metadata, and use Wormzy-
generated codes rather than hand-constructed ones.

For a custom HTTPS mailbox, `--relay-pin` can add a certificate public-key
pin. Its value is standard padded base64 of
`SHA-256(RawSubjectPublicKeyInfo)`. Pinning supplements normal certificate-chain
and hostname verification; it does not replace either check. Wormzy rejects a
pin used with HTTP, Redis, or another non-HTTPS endpoint. Plan pin updates when
the endpoint's TLS key rotates, and obtain the pin through a trusted channel.

UPnP discovery is restricted to a responding device on the same private
subnet, with DNS, redirects, proxies, and cross-host control URLs disabled.
SSDP still relies on the local network not spoofing router responses; use
`--no-upnp` on an untrusted LAN.

Wormzy transport paths are:
- direct: UDP/QUIC NAT punching, including STUN and temporary UPnP candidates (preferred)
- relay fallback: QUIC relay on UDP/3478 (only if direct race fails)

## Screenshots

![Wormzy send](docs/screenshots/wormzy-send.png)
![Wormzy receive](docs/screenshots/wormzy-receive.png)

## Reporting a Vulnerability

Please email: jdefr89@gmail.com.

## License

Wormzy is free software licensed under the [GNU Affero General Public License version 3](LICENSE) (`AGPL-3.0-only`). Modified versions made available for users to interact with over a network must offer those users the corresponding source code, as required by the license.

## AI/LLM Usage

This project is one of my first that makes serious uses of GenAI/LLM. **However**, blind agent coding did not take place! I personally step through any generated code for quality assurance. I don't like the idea of offloading security practices to AI agents (at least not yet..). If anyone wishes to contribute, use AI sparingly and do not commit any code you haven't reviewed to some extent.
