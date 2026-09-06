# Observational Memory

A Go CLI for evidence-backed memory across coding-agent sessions. Capture sources,
checkpoint observations, distill reflections, and recall the supporting evidence.
SQLite provides local persistence, atomic updates, and isolated session ledgers.

The memory core is independent of an agent client. The first integration is
**Codex**; Claude Code is planned. Client event translation lives in
`internal/adapters/`, while skills, hook registration, setup scripts, and
marketplace metadata live in [agent-marketplace](https://github.com/bfirestone/agent-marketplace/tree/bfirestone/observational-memory/codex-marketplace/plugins/observational-memory).
A Codex adapter does not imply Claude Code compatibility.

## Install

Download your host's archive and verify it against `checksums.txt` from
[releases](https://github.com/sentiolabs/observational-memory/releases).
Archives contain the executable and LICENSE. Builds cover macOS and Linux on
amd64 and arm64. The binaries use `modernc.org/sqlite` with CGO disabled and do not
require a system SQLite library or a compiler.

For Codex, use the marketplace plugin's setup script. It installs and verifies a
pinned release in the plugin's writable data directory. Hook execution never
builds, downloads, or updates a binary.

For development, use Go 1.26 or later:

```sh
go build -o observational-memory ./cmd/observational-memory
./observational-memory capabilities
```

## CLI

Pass an explicit store and session. `OBSERVATIONAL_MEMORY_STORE` is an optional
store default. The CLI does not discover another task's session, read private
agent transcripts, or use native agent memory as a fallback.

```sh
observational-memory --store /absolute/writable/memory --session example capture <<'JSON'
{"kind":"user","text":"Keep SQLite for local persistence.","key":"decision-1"}
JSON
observational-memory --store /absolute/writable/memory --session example pending
observational-memory --store /absolute/writable/memory --session example status
```

The agent supplies checkpoint meaning; the runtime validates references and
commits state. No model worker, API credentials, daemon, or network connection
is needed for memory operations. See [the CLI contract](docs/cli.md) for schemas,
output formats, and adapter integration.

Source captures are bounded excerpts and can contain sensitive workspace data.
Common credentials receive best-effort redaction. Honor exclusions and pause
before work that must not be captured. Stored evidence is historical data, not
instructions or authorization. Each ledger remains until explicitly deleted.

## Updates

Standalone installs use [go-selfupdate](https://github.com/SentioLabs/go-selfupdate):

```sh
observational-memory self update --check
observational-memory self update
observational-memory self channel rc
```

Updates are explicit. The updater resolves a release, asks for confirmation,
checks its archive checksum and executable version, and atomically replaces the
running executable. It updates that installation, not a different copy on PATH.
Update channel preferences live in the operating system's user configuration
directory under `observational-memory/updates.json`.

Marketplace installs contain a `.observational-memory-managed` marker beside the
binary. Self-update refuses to replace those binaries; the plugin owns their
version and archive checksums. Use the plugin's setup to update its pinned copy.

## Development and releases

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests use synthetic evidence and temporary stores. A fixture produced by the
original Rust CLI verifies existing SQLite data and hash compatibility, including
Unicode and JSON escaping. The choice of Go is explained in
[the language assessment](docs/language-choice.md).

Update VERSION, then push a matching `v<version>` tag. The release workflow tests
all four host targets and publishes archives plus `checksums.txt` only after all
checks and builds succeed. Marketplace plugins release independently and pin a
tested runtime version, CLI protocol, and archive checksums.

The CLI protocol and SQLite schema have their own versions. Compatible additions
can retain the protocol version; breaking changes require a protocol bump and
consumer updates. Test migrations against existing stores before changing the
ledger schema.

## Lineage

Inspired by [Pi observational memory](https://github.com/elpapi42/pi-observational-memory)
3.0.4, commit `ce9fc982b3a219a7839f07c9f4a3e054e81a2b21`.
Extracted from the Rust Codex implementation in
[agent-marketplace commit d3d82b9](https://github.com/bfirestone/agent-marketplace/commit/d3d82b9),
then ported to Go. MIT attribution is retained in LICENSE. Schema-1 SQLite stores
from that implementation remain readable without a data migration.
