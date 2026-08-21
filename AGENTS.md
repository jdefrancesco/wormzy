# Repository Guidelines

## Project Overview

`Wormzy` is a Go-native take on `magic-wormhole`: exchange a code for hassle free p2p file transfers that are secure and built on modern primitives.  punching. Read docs/ files and understand the current landscape and implementation.

## Project Structure & Module Organization

- Binaries live in `cmd/`: `rendezvous` (server), `wormzy` (CLI), and `stuncheck` (debug helper).
- Core logic is split across `internal/crypto`, `internal/transfer`, `internal/transport`, `internal/stun`, `internal/rendezvous`, and `internal/ui`. Extend these packages instead of spawning new siblings.
- Tests live beside their packages (for example `internal/stun/stun_test.go`).

## Build, Test, and Development Commands

- `make build` → runs `gosec -exclude=G104,G307` (skipping `mvp/`) and builds the `wormzy` binary at repo root.
- `make test` → executes `go test -v $(PACKAGES)`; prefer this wrapper so the curated package list is reused everywhere.

## Coding Style & Naming Conventions

- Always run `gofmt`, group imports, and keep exported APIs minimal while using descriptive filenames (`transport_conn.go`) and lowerCamelCase internals. Config uses the `WORMZY_` prefix (for example `WORMZY_RENDEZVOUS`). Follow significant edits with `gosec` and `go vet ./...`, especially around crypto, networking, or IO.

- Comment all functions using Golang convention. Also comment when it makes sense. Don't be too verbose unless it is necessary.

- Make sure significant changes are added to docs/ relevant documentation file.

- Confirm the implementation aligns with our documentation every so often. Manual testing and packet tracing may be done by myself. Making sure no discrepancies are present and things work at the security level it should

## Testing Guidelines

- Prefer table-driven tests named `Test<Area>_<Scenario>` so selective runs like `go test -run Punch ./internal/stun` stay meaningful.
- Run `make test` before every commit and add `go test -race ./internal/...` for concurrency/QUIC/crypto edits.
- For CLI updates, execute a manual send/receive loop against a local rendezvous and compare prompts with the Python magic-wormhole UX.
- scripts/ folder contains test scripts mainly meant to be executed on wormzy server.

## Commit & Pull Request Guidelines

Do NOT commit without first prompting me. I need to inspect the code for quality and possibly modify or make changes if I see a better way. This helps keep technical debt to a minimum. If I do ask you to commit: keep commit messages short, present-tense imperatives (`Tighten STUN retries`, `Refine rendezvous logging`) scoped to one change. PRs need a problem summary, commands/tests run, and links to issues or docs, plus logs or screenshots when UX output changes. Highlight crypto/TLS/NAT edits so reviewers can prioritize risk.

## Security & Configuration Tips

Default to TLS when running `cmd/rendezvous`, keep `server.crt` and `server.key` out of Git, and rotate them for shared environments. Document changes to STUN lists, PAKE parameters, or `WORMZY_*` behaviors inside the relevant `internal/*` package, and sanity-check relay or UX tweaks against the Python magic-wormhole tool to preserve familiarity. Security is a big part of this project along with efficiency. Fast and secure transfers built on modern primitives is what the project is all about.
