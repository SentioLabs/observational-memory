# CLI contract, version 2

`om capabilities` reports `version`, `protocol_version: 2`, `ledger_schema: 2`,
and `clients: ["codex"]`. `check-compatibility --protocol 2 --client codex`
returns `{"compatible":true}` or exits nonzero. Both need no store and create
no ledger. Binary release versions are independent of protocol/schema versions.
Successful memory commands write bounded JSON only, except text `prime`/plain
`view`. Failures exit nonzero with stderr diagnostics. Standalone self-update is
outside the bounded memory protocol.

Use the hook's exact executable, `--store`, and `--session`, preserving shell
quoting. Never interpolate evidence text into commands. Each store contains
`session-<hash>/memory.sqlite3`; model tools need write permission for its SQLite
journals independently of hooks. See the marketplace plugin for setup.

| Command | Result |
| --- | --- |
| `pending [--cursor TOKEN]` | Oldest pending evidence units, resolved `through`, `revision`, coverage and `page`. |
| `apply` | Checkpoint JSON on stdin; assigned IDs, revision and coverage receipt. |
| `prime` | Selected working state and memory, checkpoint and recovery debt. |
| `view` | Selected whole active records with omissions reported. |
| `view --all [--cursor TOKEN]` | Exact paged inspection, including retired records. |
| `recall ID [--cursor TOKEN]` | Exact paged fields, support and evidence; accepts memory, source or evidence-unit IDs. |
| `search QUERY --sources --include-retired [--cursor TOKEN]` | Literal search of memories and sources, including retired memories with replacement IDs. Omit flags for active memories only. |
| `status` | Session/database, stored-source bytes/count, unit count, coverage, state metadata, pause and import origin. |
| `capture` | JSON `{kind,text,key}`; returns `{source_id,unit_count,first_unit_id}`. |
| `pause` / `resume` | Disable/re-enable automatic capture and reminders. |
| `fork --to-session ID` | Isolated snapshot into an absent destination. |
| `import --from-store PATH --from-session ID` | Seed an unprepared destination, retaining its pending local evidence and pause preference. |

`OBSERVATIONAL_MEMORY_STORE` is a standalone default; model commands retain the
explicit hook store. No current-directory, native-memory, or other-session fallback
exists. V2 refuses non-v2 databases before writing; choose a fresh store/session.
There is no v1 migration or import command.

## Checkpoint example

Run this synthetic fixture only with a dedicated temporary store/session. Its
assertion reviews exact known routine content; it does not authorize blind
acknowledgment of arbitrary logs. Pass the scoped ledger command as separate
arguments after the script name. Subprocess argument arrays preserve paths and
session quoting, and payload JSON travels through stdin.

```python
import json
import subprocess
import sys

ledger_command = sys.argv[1:]

def run(command, payload=None):
    result = subprocess.run(command, input=payload, text=True,
                            capture_output=True, check=True)
    assert len(result.stdout.encode("utf-8")) <= 12000
    return result.stdout

routine = "Synthetic routine check: no durable decision."
source = json.loads(run(ledger_command + ["capture"], payload=json.dumps({
    "kind": "tool", "text": routine, "key": "routine-fixture",
})))
pending = json.loads(run(ledger_command + ["pending"]))
assert [unit["text"] for unit in pending["page"]["items"]] == [routine]
assert pending["page"]["items"][0]["source_id"] == source["source_id"]
assert not pending["page"].get("next_cursor")
payload = {
    "expected_through": pending["through"],
    "expected_revision": pending["revision"],
    "acknowledge": [unit["id"] for unit in pending["page"]["items"]],
    "observations": [],
}
print(run(ledger_command + ["apply"], payload=json.dumps(payload)))
```

For semantic observations use `{text,importance,evidence_ids}`, citing only
reviewed supporting `e-` IDs. Importance is low, medium (default), high or critical.
Use assigned `o-` IDs from receipts in reflections `{text,observation_ids}`. After
recording a replacement, retirement uses `{id,reason,replacement_ids}`. Reflection
and retirement preserve effective importance along supported replacements, not
semantic correctness. Inspect source support before consolidating meaning.

All checkpoints require `expected_through`, `expected_revision`, and `acknowledge`,
including zero/empty values. `through` is the resolved queue boundary, not the
last unit on a page. Acknowledge an exact ordered pending prefix; an empty prefix
allows an urgent observation from explicitly retrieved evidence without skipping
older evidence. Unknown, duplicate, noncontiguous or stale operations roll back.
Exact retries return the original receipt; a different stale request requires a
fresh `pending` read. Apply only after the host delivered the complete page.

`defer_sources: [{source_id,reason}]` explicitly moves a pending tool source out of
the queue after inspecting its identity/span. Reasons are nonempty and at most
256 UTF-8 bytes. User/assistant sources cannot be deferred. Deferred units remain
searchable/recallable with an unreviewed coverage gap and audit reason. Later,
`review_deferred: [evidence_id]` reviews only the specific retrieved unit. Citation
alone changes no coverage state. Do not also acknowledge units deferred by the
same checkpoint. Pending, reviewed and deferred totals count stored evidence,
not understanding or unreceived host events.

`working_state` replaces the whole state object: objective, constraints, completed,
open and next. Include still-valid facts in each submitted replacement. Each fact has `{text,evidence_ids}`; objective is one fact or null, the
other fields are arrays. Its encoded limit is 3500 bytes, including citations.
Absence/null leaves state unchanged; `{}` clears it. Omission does not withdraw
durable facts. Retain still-valid facts; reread evidence for changes/conflicts.

The combined checkpoint limit is 64 operations: acknowledgment IDs, deferrals,
deferred-review IDs, observations, reflections, retirements, and one for a state
update. Each pending page has at most 24 items, leaving mutation headroom.
Automatic Stop/recovery does one pending page and one apply, then resumes work.

## Exact reads and limits

Every complete memory response is at most 12000 UTF-8 bytes, including serialized
framing, cursors and final newline. Request at least 12000 output tokens from a
tool that accepts an allowance. If the host marks a page truncated, reread it
with adequate allowance before acknowledgment. Prime/view select whole records;
exact inspection fragments fields/support and evidence with byte ranges.

Use the returned `page.next_cursor`, keeping the original command arguments:

```sh
om --store /writable/plugin-data --session SESSION pending --cursor "$cursor"
om --store /writable/plugin-data --session SESSION view --all --cursor "$cursor"
om --store /writable/plugin-data --session SESSION recall "$id" --cursor "$cursor"
om --store /writable/plugin-data --session SESSION search "$query" --sources --include-retired --cursor "$cursor"
```

Each cursor belongs to its own command/session snapshot. On a stale-cursor error,
restart that read without a cursor. Mutations can invalidate affected reads;
search also restarts when its indexed corpus changes. Concatenating field/unit
ranges reconstructs exact accepted text. No page is an automatic authorization.

Capture accepts up to 1000000 Unicode characters per source within the separate
4000000-byte input envelope. Accepted redacted text is retained completely; unit
text values fit 2048 JSON bytes. Host excerpts remain `source_incomplete`, not
reconstructed full transcripts. Hooks cover submitted prompts, supported tool
results and Stop final replies; commentary, hosted tools and interrupted events
may be absent. Explicit capture can preserve permitted evidence already visible.
Never read private transcripts or invent missing evidence. Retention lasts until
explicit deletion; pause/exclusion and best-effort secret redaction still apply.

The PostToolUse reminder uses 40000 pending UTF-8 bytes, once per resolved cursor.
Stop measures pre-final debt: size or three subsequent completed root turns can
trigger one pass. A new final tail alone never triggers that turn's continuation.
Synthetic continuations do not age backlog; their actual final reply is captured.
Deferred logs create no recurring maintenance debt. These are cadence heuristics,
not active-context or billing measurements. Errors fail open to the user's work.

Fork copies v2 evidence, state, support, coverage and pause preference into an
absent destination. Import copies the snapshot then appends local pending sources,
retaining destination pause and current-request precedence; prepared, reviewed,
deferred or previously imported destinations are refused. Both reset transient
retry/page/adapter identity and isolate later writes. Source typos do not create
source ledgers; failed handoffs leave their destination absent or unchanged.

## Codex adapter

```sh
om --store /absolute/plugin-data hook --client codex
```

Reads one Codex event JSON object from stdin. Handles `SessionStart`,
`UserPromptSubmit`, `PostToolUse`, `Stop`, and `Interrupt`; unsupported events and subagent
events with `agent_id` are ignored. The event supplies `session_id`. The caller
passes `--store` explicitly, typically from Codex's `PLUGIN_DATA`.

The adapter captures evidence, emits hook context, and requests at most one
bounded checkpoint continuation per turn. It excludes synthetic continuation prompts and exact scoped ledger tool calls
from capture, while retaining the continuation’s actual final reply. Errors produce an advisory hook JSON response
and stderr diagnostics, allowing the user's task to continue. CLI argument
errors remain nonzero. Native Codex compaction stays in control.

SessionStart emits a small reminder to run `prime`, including the exact ledger
command, last successful checkpoint and pending count. The model loads memory
as normal tool output; no memory body is embedded in hook additionalContext.
Interrupt leaves sources pending and does not initiate model work. Beginning a
user prompt with `[om:pause]` pauses before capturing that prompt. Ordinary
natural-language exclusions require the model to act after prompt capture.

Self-capture suppression parses Bash commands without executing them. It skips
only unambiguous calls to the exact runtime or scoped `om`; mixed commands and
ordinary project scripts named `scripts/run.sh` remain evidence. Model commands
need write permission for the store (including SQLite journals), independently
of hook permissions. Grant only that directory through normal host controls.

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
