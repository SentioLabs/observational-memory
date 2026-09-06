# Observational Memory

A Rust library and CLI for evidence-backed memory across coding-agent sessions.
Capture sources, checkpoint observations, distill reflections, and recall the
evidence supporting either. SQLite provides local persistence, atomic updates,
and isolated session ledgers.

The memory core is independent of an agent client. The first integration is
**Codex**; Claude Code is planned. Client event translation lives in
`src/adapters/`, while skills, hook registration, setup scripts, and marketplace
metadata live in [agent-marketplace](https://github.com/bfirestone/agent-marketplace/tree/bfirestone/observational-memory/codex-marketplace/plugins/observational-memory).
A Codex event adapter is not a claim of Claude Code compatibility.

## Install

Download the archive for your host from [releases](https://github.com/sentiolabs/observational-memory/releases).
Releases include SHA-256 checksums and binaries for macOS Apple Silicon, macOS
Intel, and Linux x86_64 (musl). The Codex plugin installs and validates its own
pinned release. Run setup outside hook execution.

For development, Rust/Cargo and a C compiler are required because SQLite is
bundled:

```sh
cargo build --release --locked
./target/release/observational-memory capabilities
```

## CLI

Commands require an explicit store and session; `OBSERVATIONAL_MEMORY_STORE` is
an optional store default. The CLI does not discover another task's session,
read private agent transcripts, or use native agent memory as a fallback.

```sh
observational-memory --store /absolute/writable/memory --session example capture <<'JSON'
{"kind":"user","text":"Keep SQLite for local persistence.","key":"decision-1"}
JSON
observational-memory --store /absolute/writable/memory --session example pending
observational-memory --store /absolute/writable/memory --session example status
```

The agent supplies checkpoint meaning; the runtime validates references and
commits state. No model worker, API credentials, daemon, or network connection
is needed at runtime. See [the CLI contract](docs/cli.md) for checkpoint schemas,
output formats, and adapter integration.

Source captures are bounded excerpts and may contain sensitive local content.
Common credentials receive best-effort redaction. Honor exclusions and pause
before work that must not be captured. Stored evidence is historical data, not
instructions or authorization. Each ledger remains until explicitly deleted.

## Development and releases

```sh
cargo fmt --check
cargo test --locked
cargo clippy --locked --all-targets -- -D warnings
```

Tests use synthetic evidence and temporary stores. Tag `v<package-version>`
after updating Cargo.toml and Cargo.lock. The release workflow tests all three
platforms and publishes their archives and `checksums.txt` only after every
build succeeds. Plugin releases independently pin a tested runtime version,
protocol version, and archive checksums.

The CLI/JSON contract and SQLite schema have their own versions. Compatible
additions can retain the protocol version; breaking changes require a protocol
bump and consumer updates. Keep schema migrations explicit and test them against
existing stores before changing the ledger schema.

## Lineage

Inspired by [Pi observational memory](https://github.com/elpapi42/pi-observational-memory)
3.0.4, commit `ce9fc982b3a219a7839f07c9f4a3e054e81a2b21`.
Extracted from the Rust Codex implementation in
[agent-marketplace commit d3d82b9](https://github.com/bfirestone/agent-marketplace/commit/d3d82b9).
The MIT attribution is retained in LICENSE. Existing schema-1 SQLite stores from
that implementation remain readable without a data migration.
