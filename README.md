# Wormzy

Wormzy aims to be simple and secure way to share large files with another party.

Wormzy allows users to send files directly to another party with ease. Its primary features
include:

* Send file of any size peer-to-peer. No need to change NAT rules.
* Communication is secure/encrypted
* Utilizes QUIC for fast transfers.


## Quick Start

Install the `wormzy` CLI:

```bash
go install github.com/jdefrancesco/cmd/wormzy@latest
```

On the sender:

```bash
wormzy send ./big.bin
# => displays a pairing code such as f7p9-x2
```

On the receiver (on another terminal/machine):

```bash
wormzy recv
# prompted for the pairing code, then the file arrives
```

By default the receiver saves into the current working directory. Override this with
`wormzy recv -download-dir ~/Downloads`—Wormzy will create the directory if needed and
will refuse the transfer up front if the filesystem cannot hold the advertised file size.

## Systemd units

ators who want the relay proxy to restart automatically can install the provided
`deploy/systemd/wormzy-mailbox.service`:

1. Copy the unit into `/etc/systemd/system/`.
2. Create `/etc/wormzy/mailbox.env` with at least:
   ```bash
   WORMZY_MAILBOX_LISTEN=:9000
   WORMZY_MAILBOX_REDIS=rediss://default:password@example.com:25061
   ```
3. `sudo systemctl daemon-reload && sudo systemctl enable --now wormzy-mailbox`.

The unit runs the `/usr/local/bin/mailbox` binary, restarts on failure, and keeps Redis
credentials outside the unit file so you can rotate them without editing service config.

## Screenshots

Add screenshots (or a short screencast thumbnail) under `docs/screenshots/` and link them here.

<!-- Example:
![Wormzy send](docs/screenshots/wormzy-session.png)
-->

## Security Policy

TBD

## Reporting a Vulnerability

Please email jdefr89@gmail.com.
