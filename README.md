# Observational Memory

`om` is a Go CLI for evidence-backed memory across coding-agent sessions. Capture
sources, checkpoint observations, distill reflections, and recall the supporting evidence.
SQLite provides local persistence, atomic updates, and isolated session ledgers.

The memory core is independent of an agent client. The first integration is
**Codex**; Claude Code is planned. Client event translation lives in
`internal/adapters/`, while skills, hook registration, setup scripts, and
marketplace metadata live in [agent-marketplace](https://github.com/bfirestone/agent-marketplace/tree/bfirestone/observational-memory/codex-marketplace/plugins/observational-memory).
A Codex adapter does not imply Claude Code compatibility.

## Install

Download your host's archive and verify it against `checksums.txt` from
[releases](https://github.com/sentiolabs/observational-memory/releases).
Archives contain the `om` executable and LICENSE. Install `om` on your PATH.
Builds cover macOS and Linux on amd64 and arm64. The binaries use `modernc.org/sqlite` with CGO disabled and do not
require a system SQLite library or a compiler.

For Codex, use the marketplace plugin's setup script. It installs and verifies a
pinned release in the plugin's writable data directory. Hook execution never
builds, downloads, or updates a binary.

For development, use Go 1.26 or later:

```sh
go build -o om ./cmd/om
./om capabilities
```

### Upgrading from v0.1.1

The executable is now named `om`. For an existing standalone v0.1.1 installation,
download and verify the new release archive, then install its `om` binary on PATH.
The old updater requires an archive member named `observational-memory` and cannot
perform this rename. After this one-time installation, use `om self update`.
For a Codex plugin installation, rerun the plugin's setup script.

Continue using the same `--store` directory and session names. The SQLite schema,
`OBSERVATIONAL_MEMORY_STORE` environment variable, update preferences, repository,
and `$observational-memory` skill retain their existing names.

## CLI

Pass an explicit store and session. `OBSERVATIONAL_MEMORY_STORE` is an optional
store default. The CLI does not discover another task's session, read private
agent transcripts, or use native agent memory as a fallback.

```sh
om --store /absolute/writable/memory --session example capture <<'JSON'
{"kind":"user","text":"Keep SQLite for local persistence.","key":"decision-1"}
JSON
om --store /absolute/writable/memory --session example pending
om --store /absolute/writable/memory --session example status
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
om self update --check
om self update
om self channel rc
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

Releases follow the same Release Please and GoReleaser workflow as Arc.
Conventional commits on `main` maintain a release PR containing the changelog
and `.release-please-manifest.json` version update. Merge that PR to create the
stable tag and GitHub release. The release workflow then runs the native tests
on all four host targets before GoReleaser uploads archives, checksums, and Linux
packages (deb, rpm, and Arch). There is no manually maintained VERSION file.

Both tools run in the same workflow because tags created with `GITHUB_TOKEN`
do not trigger another push workflow. No personal access token is required.
Release Please's bot-created PR also does not automatically trigger PR checks;
close and reopen it as a maintainer to run the Tests workflow before merging.

Use `mise install` to provision the Go, Task, and GoReleaser versions from
`mise.toml`. Local release commands are:

```sh
task release:check
task release:snapshot  # builds all artifacts without publishing
task release          # publishes the checked-out tag; normally handled by CI
```

Release candidates use dotted counters such as `v0.1.2-rc.1` and `v0.1.2-rc.2`.
Push a prerelease tag to run the tests and GoReleaser with prerelease detection.
The workflow also supports beta and alpha tags. For a failed publication, rerun
the workflow on the existing tag.

The nightly workflow runs at 06:00 UTC, checks for new commits, and tags
`v<next patch>-nightly.<YYYYMMDD>` using the last stable manifest version.
It explicitly dispatches the prerelease workflow and retains seven days of
nightly releases. Repeating a run on the same day leaves the existing tag intact.
Stable and RC releases are excluded from nightly cleanup.

Marketplace plugins release independently and pin a tested runtime version,
CLI protocol, and archive checksums. Archive names remain
`observational-memory_<version>_<os>_<arch>.tar.gz` and contain only the executable
(`om`) and LICENSE, as required by both installers.

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
