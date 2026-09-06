# CLI contract, version 1

`om capabilities` prints one JSON object containing `version`,
`protocol_version`, `ledger_schema`, and `clients`. Currently the only client is
`codex`. `check-compatibility --protocol 1 --client codex` exits zero with
`{"compatible":true}` when supported, and exits nonzero otherwise. These commands
need no store and do not create a ledger. Binary releases and marketplace plugin
versions are independent.

## Core commands

Pass `--store PATH --session ID` before the command. `--store` can default to
`OBSERVATIONAL_MEMORY_STORE`; there is no implicit agent environment or working
directory fallback. Different clients should use separate stores unless sharing
an exact session identity is intentional. Sessions are stored at
`session-<hash>/memory.sqlite3`. Schema version 1 preserves the original Codex
plugin's paths, IDs, sequence numbers, timestamps, support links, and cursors.

| Command | stdin | stdout |
| --- | --- | --- |
| `capture` | `{kind,text,key}` | JSON `{source_id}` |
| `pending` | None | JSON `{through,sources}` |
| `apply` | Checkpoint object below | JSON `{through,observations,reflections,retired}` |
| `status` | None | JSON session, database path, paused, through, pending counts, active counts |
| `view` | None | Human-readable active memory, at most 12K characters |
| `view --all` | None | JSON array of active and retired entries |
| `recall ID` | None | JSON entry and cited sources; reflections also include observations |
| `pause` / `resume` | None | JSON status; client adapters honor the paused state |
| `fork --to-session ID` | None | JSON status of a new independent snapshot |

`kind` is `user`, `assistant`, or `tool`; `key` identifies the capture's origin.
Source IDs begin with `s-`, observations with `o-`, and reflections with `r-`.
Identical captures with the same role, origin key, and text are idempotent.
Explicit capture remains available while automatic adapter capture is paused.
Fork preserves evidence and checkpoint progress but clears transient adapter
state. It never overwrites an existing destination session.

A checkpoint's arrays are optional:

```json
{
  "through": 3,
  "observations": [{"text":"A supported fact.","importance":"high","source_ids":["s-actual-id"]}],
  "reflections": [{"text":"A durable conclusion.","observation_ids":["o-existing-id"]}],
  "retire": [{"id":"o-older-id","reason":"Superseded.","replacement_ids":["o-newer-id"]}]
}
```

Use the cursor and IDs returned by the runtime. Apply observations first, then
use their assigned IDs in later reflection/retirement calls. Importance is
`low`, `medium` (default), `high`, or `critical`. A reflection requires active
observations. Retirement requires a newer active entry of the same kind or a
newer reflection that cites the old observation. Retirement keeps evidence
available through recall. Validation errors roll back the entire checkpoint,
including its cursor. Identical observations at the same cursor do not duplicate
or reactivate entries. Stale cursors fail; reread pending before retrying.

Sources over 24K characters retain marked head/tail excerpts. Pending chunks are
approximately 48K characters. Views prioritize reflections, importance, and
recency, then display selected entries in ledger order. `view --all` and `recall`
are unbounded inspection commands. Memory grows until its session directory is
explicitly removed. Timestamps describe capture time, not inferred event time.

CLI failures exit nonzero and write diagnostics to stderr. Input is one JSON
object limited to 4 MB. Successful machine-readable operations write JSON only
to stdout; plain `view` is the documented text exception.

## Codex adapter

```sh
om --store /absolute/plugin-data hook --client codex
```

Reads one Codex event JSON object from stdin. Handles `SessionStart`,
`UserPromptSubmit`, `PostToolUse`, and `Stop`; unsupported events and subagent
events with `agent_id` are ignored. The event supplies `session_id`. The caller
passes `--store` explicitly, typically from Codex's `PLUGIN_DATA`.

The adapter captures evidence, emits hook context, and requests at most one
bounded checkpoint continuation per turn. It excludes its own continuation and
ledger tool calls from capture. Errors produce an advisory hook JSON response
and stderr diagnostics, allowing the user's task to continue. CLI argument
errors remain nonzero. Native Codex compaction stays in control.

The Codex marketplace owns skill instructions and hook registration. A future
Claude Code adapter will translate its separately verified event contract into
the same core operations; the core has no dependency on Codex event fields.

[Official Codex hook contract](https://learn.chatgpt.com/docs/hooks), checked
2026-09-06 against advertised CLI version 0.153.4.

## Standalone updates

`self update [--check] [--force] [-y]` and `self channel [stable|rc|nightly] [-y]`
use go-selfupdate. These are explicit user-facing operations with text output;
they are outside the memory JSON contract and can use the network. No hook calls
them. Plugin-managed binaries reject replacement through self-update. The plugin
pins an exact release and owns its installation lifecycle.
