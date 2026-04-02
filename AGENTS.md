# Repository Guidelines

## Project Overview

Wormzy is a Go-native take on `magic-wormhole`: exchange a code, derive keys with SPAKE2/Noise, and send files over QUIC with NAT punching. Read `mvp/mvp.go` and `mvp/relay.go` for the rendezvous → PAKE → QUIC flow.

## Project Structure & Module Organization

- Binaries live in `cmd/`: `rendezvous` (server), `wormzy` (CLI), and `stuncheck` (debug helper).
- Core logic is split across `internal/crypto`, `internal/transfer`, `internal/transport`, `internal/stun`, `internal/rendezvous`, and `internal/ui`. Extend these packages instead of spawning new siblings.
- Tests live beside their packages (for example `internal/stun/stun_test.go`).

## Build, Test, and Development Commands

- `make build` → runs `gosec -exclude=G104,G307` (skipping `mvp/`) and builds the `wormzy` binary at repo root.
- `make test` → executes `go test -v $(PACKAGES)`; prefer this wrapper so the curated package list is reused everywhere.
- `go run ./cmd/rendezvous -addr :9999 -tlscert server.crt -tlskey server.key` → TLS rendezvous for local development; omit TLS only for quick debugging.
- `go run ./cmd/wormzy -mode send -file ./big.bin -code f7p9-x2 -relay 127.0.0.1:9999` plus the matching `-mode recv` → fastest smoke test of CLI UX; run `make debug` first if you need an unoptimized binary.

## Coding Style & Naming Conventions

Always run `gofmt`, group imports, and keep exported APIs minimal while using descriptive filenames (`transport_conn.go`) and lowerCamelCase internals. Config uses the `WORMZY_` prefix (for example `WORMZY_RENDEZVOUS`). Follow significant edits with `gosec` and `go vet ./...`, especially around crypto, networking, or IO.

## Testing Guidelines

- Prefer table-driven tests named `Test<Area>_<Scenario>` so selective runs like `go test -run Punch ./internal/stun` stay meaningful.
- Run `make test` before every commit and add `go test -race ./internal/...` for concurrency/QUIC/crypto edits.
- For CLI updates, execute a manual send/receive loop against a local rendezvous and compare prompts with the Python magic-wormhole UX.

## Commit & Pull Request Guidelines

Keep commit messages short, present-tense imperatives (`Tighten STUN retries`, `Refine rendezvous logging`) scoped to one change. PRs need a problem summary, commands/tests run, and links to issues or docs, plus logs or screenshots when UX output changes. Highlight crypto/TLS/NAT edits so reviewers can prioritize risk.

## Security & Configuration Tips

Default to TLS when running `cmd/rendezvous`, keep `server.crt` and `server.key` out of Git, and rotate them for shared environments. Document changes to STUN lists, PAKE parameters, or `WORMZY_*` behaviors inside the relevant `internal/*` package, and sanity-check relay or UX tweaks against the Python magic-wormhole tool to preserve familiarity.
