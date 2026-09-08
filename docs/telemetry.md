# Local field telemetry

Telemetry is off by default. It is an opt-in, local observational study across
sessions in one explicit memory store. It neither exports data nor starts model
work, a daemon, OTel, a benchmark or an automation. Memory operation, telemetry
enrollment and independently verified host activation are separate.

After installing a matching runtime/plugin, enable the chosen store:

```sh
om --store "$OM_STORE" telemetry enable
om --store "$OM_STORE" telemetry status
om --store "$OM_STORE" telemetry report
om --store "$OM_STORE" telemetry disable
```

No session is required for enrollment/reporting. `status` and `report` produce the
same bounded JSON summary. Enrollment starts `enabled_at`; the first observed
hook is recorded separately. `awaiting_first_hook` is expected before delivery.
Piping test JSON into `om hook` can produce observations but never proves actual
Codex activation. The orchestrator should verify the real integration while
collection is OFF, then enable it for ordinary tasks. Do not import old synthetic
results into the study. No collection is enabled by installation alone.

When the **user explicitly chooses** to rate an incident, use one of:

```sh
om --store "$OM_STORE" --session "$TASK_ID" telemetry feedback recovered
om --store "$OM_STORE" --session "$TASK_ID" telemetry feedback forgot
om --store "$OM_STORE" --session "$TASK_ID" telemetry feedback repeated-work
om --store "$OM_STORE" --session "$TASK_ID" telemetry feedback correction
```

There is no free-text rating, automatic rating, model self-rating or prompt on
every turn. Each choice is linked to the pseudonymous task and, when retained,
the latest observed compaction signal. This is a temporal observation link, not
proof that a specific compaction caused the incident. Unrated recovery is unknown.

## What is stored

A separate `$OM_STORE/telemetry/events.sqlite3` stores UTC timestamps, schema and
runtime version, HMAC task/turn pseudonyms, closed command/hook categories, generic
success/failure, CLI/hook execution duration, successful prime loading, lifecycle
signals, checkpoint age and numerical coverage where an existing operation
already calculated it. Extra ledger metadata reads are constant-size; telemetry
does not add a whole-ledger scan to tool hooks. Coverage is sampled, not a promise
that the latest store state was measured. Last observed coverage totals and age
are labeled accordingly.

Task and turn identifiers use a random private 256-bit per-store key and separate
HMAC domains. Raw IDs, absolute paths, prompts, memory text, code, tool
inputs/outputs, arbitrary metadata and raw errors are not telemetry fields. Known
models have a closed allowlist; unknown/custom names are null. Host version,
provider/billed usage and model waiting time remain unknown. The local SQLite
key/database uses0600 permissions inside a0700 directory; symlinks and nonregular
or nonprivate telemetry files are refused. Memory content remains governed by
its existing separate storage/privacy rules.

Memory privacy pause also suppresses collection for that session, including the
prompt containing `[om:pause]`. Telemetry disable is independent and never pauses
or deletes memory. Operations whose privacy state cannot be established are not
recorded. Failures before valid session/privacy setup or before CLI launch may
therefore be absent. Hook failure diagnostics stay in their existing channel;
telemetry stores only `operation_failed`.

## Lifecycle and interpretation

Reference Codex0.153.4 supports `PreCompact` and `PostCompact` with task/turn/model
and auto/manual trigger fields, and `SessionStart` with source=compact but no
turn/item identity. The adapter handles compact hooks with empty `{}` output and
no capture/guidance. To receive them, the existing plugin needs two hook entries
using the same `om hook --client codex` wrapper as its other lifecycle hooks.
This runtime change does not install or trust hooks or edit global configuration.
Matched source: `codex-rs/hooks/src/schema.rs`347–385 and
`hooks/src/events/compact.rs` at OpenAI Codex commit
`3d2ee51ca2d5db578f328aa75e20aa22c0197c9a` (rust-v0.153.4).

Reports distinguish raw signal deliveries from distinct observed task/turn
compaction pairs. Duplicate Pre/Post delivery does not inflate those lower bounds;
multiple real compactions in one turn collapse to one. Missing IDs never create
an identified boundary. Exact unique compactions remain null. The first Pre/Post
UTC interval is an observation interval, not billed latency or model time.
Successful prime after a retained signal means the CLI loaded its response,
not that the model remembered, used or correctly followed it. The raw signal-chain
histogram is explicitly not a unique-compaction count.

Command/hook durations measure execution inside the CLI, excluding process startup
and telemetry persistence; they are not model waiting time, total integration
overhead or billed tokens. Summaries include counts, errors and p50/p95/total
execution duration. No provider-complete cost, causal superiority, randomized
comparison, synthetic release gate or indefinite-memory claim follows from this
OM-only study.

## Retention and failure behavior

Automatic recording has a100ms context deadline and25ms SQLite busy timeout.
Concurrent CLI processes serialize writes transactionally; inability to record
never changes normal stdout/exit status. Telemetry corruption, permissions,
contention or storage failure may lose observations. An unrecordable drop cannot
reliably record its own counter: `unrecorded_drops` stays null and limitations are
always shown. No automatic repair/deletion of corrupt telemetry occurs.

Retention is rolling, up to100000 event rows, with a64MiB main-file ceiling and
bounded SQLite rollback journal (allow up to another64MiB transiently). Older rows
are evicted when row or page capacity is approached; eviction count, cap-reached
flag and retained start/end dates are visible. This is not guaranteed14-day
retention under arbitrary traffic. Inspect collection health after seven days;
keep the bounded local database for analysis after fourteen days. Reports scan
only bounded telemetry rows, with a three-second deadline, not memory databases.
Raw typed measurements remain in SQLite for later analysis; do not share its
private pseudonym key casually.

Disable preserves existing events/key; re-enable retains the same pseudonyms and
increments enrollment epochs. First enrollment/first hook remain historical;
gaps while disabled/paused are not reconstructed. Delete only the explicitly
chosen telemetry directory while disabled and with no concurrent CLI processes
if a fresh unlinkable study is desired; this does not remove memory. Re-enabling
then creates a new key. The report never infers unseen tasks or rates unknown
recoveries as successes.

For a seven-day health check, examine enabled state, first/last hook, categories,
unknown/drop/retention limitations, coverage sampling and errors. For fourteen-day
analysis, inspect observed compaction lower bounds/chains, loading after signals,
explicit user incidents, stale/unreviewed coverage and execution latency. Keep
independent host activation evidence outside these self-reported measurements.
