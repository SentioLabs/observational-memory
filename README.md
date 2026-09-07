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
Builds cover Linux on amd64/arm64 and macOS on arm64. The binaries use `modernc.org/sqlite` with CGO disabled and do not
require a system SQLite library or a compiler.

For Codex, use the marketplace plugin's setup script. It installs and verifies a
pinned release in the plugin's writable data directory. Hook execution never
builds, downloads, or updates a binary.

For development, use Go 1.26 or later:

```sh
go build -o om ./cmd/om
./om capabilities
```

## CLI

Pass an explicit store and session. `OBSERVATIONAL_MEMORY_STORE` is an optional
store default. The CLI does not discover another task's session, read private
agent transcripts, or use native agent memory as a fallback.

```sh
om --store /absolute/writable/memory --session example capture <<'JSON'
{"kind":"user","text":"Keep SQLite for local persistence.","key":"decision-1"}
JSON
om --store /absolute/writable/memory --session example pending
om --store /absolute/writable/memory --session example prime
om --store /absolute/writable/memory --session example status
```

The agent supplies checkpoint meaning; the runtime validates references and
commits state. No model worker, API credentials, daemon, or network connection
is needed for memory operations. See [the CLI contract](docs/cli.md) for schemas,
output formats, and adapter integration. `prime` loads a bounded memory view with
its last checkpoint and pending backlog. For handoff into an already-started
session, use explicit `import` before its first checkpoint; the destination's
pending prompt is retained. `fork` remains available for an unused identity.

Complete accepted redacted sources are retained as paged evidence units, up to
1000000 Unicode characters per source within the separate 4000000-byte input
envelope. Every complete memory response is at most 12000 UTF-8 bytes, including
framing and newline. Exact paged recall/search recover omitted facts; prime
selects working state and reports pending/reviewed/deferred coverage separately.
Retained sources can contain sensitive workspace data.
Common credentials receive best-effort redaction. Honor exclusions and pause
before work that must not be captured. In Codex, begin a prompt with `[om:pause]`
to exclude that prompt before capture. Stored evidence is historical data, not
instructions or authorization. Each ledger remains until explicitly deleted.

## Codex compaction

Optional user-owned native settings for the evaluation starting point:

```toml
model_auto_compact_token_limit = 200000
model_auto_compact_token_limit_scope = "total"
```

Lifetime ledger history is distinct from active request context. 200K is evaluation
headroom, not a hard billing ceiling or optimal setting. No universal 300K/2x
Codex quota claim is supported. Setup does not alter global configuration. The
[official configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
defines the threshold/scope; verify effective settings on the actual host.
Native compaction stays in control. Graphs, Claude, background model workers and
precise per-request enforcement are outside this release.

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

Tests use synthetic evidence and temporary v2 stores, including Unicode, exact
source reconstruction, transactional checkpoints and native lifecycle canaries. The choice of Go is explained in
[the language assessment](docs/language-choice.md).

Releases follow the same Release Please and GoReleaser workflow as Arc.
Conventional commits on `main` maintain a release PR containing the changelog
and `.release-please-manifest.json` version update. Merge that PR to create the
stable tag and GitHub release. The release workflow then runs the native tests
on all three host targets before GoReleaser uploads archives, checksums, and Linux
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
consumer updates. This unused runtime moves directly to schema/protocol 2.
Non-v2 databases are refused before writes; select a fresh store/session. No v1
migration or importer is provided. T9 owns the evaluated artifact release and
matching marketplace version/checksums; intermediate commits are development only.

## Lineage

Inspired by [Pi observational memory](https://github.com/elpapi42/pi-observational-memory)
3.0.4, commit `ce9fc982b3a219a7839f07c9f4a3e054e81a2b21`.
Extracted from the Rust Codex implementation in
[agent-marketplace commit d3d82b9](https://github.com/bfirestone/agent-marketplace/commit/d3d82b9),
then ported to Go. MIT attribution is retained in LICENSE. The current v2 ledger
does not read or migrate schema-1 stores.
