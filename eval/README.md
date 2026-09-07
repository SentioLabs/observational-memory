# Native compaction / Observational Memory evaluation

The opt-in Python 3.10+ harness evaluates newly created synthetic Codex tasks. It
uses only the standard library, the installed Codex App Server and the supplied
v2 `om` candidate. The public `om` CLI remains Go-only. It never controls an
existing Desktop task or copies authentication into result artifacts.

`dry-run` and `replay` start **no subprocesses or models** and need no credentials.
`pilot` and `release` are explicitly paid modes; running tests or a preview does
not authorize either. A functioning runner or successful pilot is not release
quality evidence.

## Unpaid preview and tests

```sh
python3 -m unittest discover -s tests -p 'test_evaluate_compaction.py'
# Equivalent Taskfile target: task eval:test
python3 scripts/evaluate_compaction.py --mode dry-run --split pilot --cycles 1 \
  --output /absolute/fresh/pilot-preview
python3 scripts/evaluate_compaction.py --mode dry-run --split release \
  --output /absolute/fresh/release-preview
```

Each output directory must be new. Previews write paired synthetic workspaces,
`preview.json`, `frozen-manifest.json`, `results.json` and `report.md`. Candidate
hashes are null unless `--om-binary` and `--plugin-root` are supplied. Supply those
paths during the operator preview to record their real identities without
launching either binary. Preview results are `incomplete`, never release PASS.

The default unittest gate builds a fresh local Go candidate and runs real metered
hooks/ledger commands, including quoted paths and session names. An explicit
`OM_EVAL_TEST_BINARY` may supply a candidate instead; absence never skips these
integration checks. Set the ordinary Go cache environment when needed. No model
or authentication calls are involved.

## Local activation proof

```sh
python3 scripts/evaluate_compaction.py --mode activation-check \
  --output /absolute/fresh/activation-proof --om-binary /absolute/candidate/om \
  --plugin-root /absolute/observational-memory --codex codex --max-seconds 120
```

This public entry point runs the production plugin staging and meter against a
loopback Responses stub in new unauthenticated homes. It uses normal
`workspace-write` sandboxing and never calls an inference provider or reads an
account. The default and maximum wall ceiling are 120 seconds; a smaller explicit
`--max-seconds` tightens it. A failed or timed-out check returns nonzero. Dedicated
live homes and paid/model flags are refused. On hosts that prohibit nested sandbox
creation, run the parent command outside that enclosing sandbox while retaining
the App Server's normal sandbox policy.

`activation.json` records candidate, installed plugin and meter hashes; actual
SessionStart, UserPromptSubmit, PostToolUse, Stop and Interrupt evidence; the
successful native command executing the hook-delivered `prime`; effective config
equivalence; and negative-control provenance. Native-zero, untrusted, failed-hook
and failed-prime controls execute actual host turns. Missing/modified definitions
and disabled hooks are checked with actual `hooks/list` after isolated mutations.
Raw native notifications, metered calls and stub requests live under `activation/`.
Synthetic provider token metadata is separate and never contributes quality,
release scores or live input consumption. Manual compaction is not exercised by
this check; the separate native compaction canary and live campaign cover it.

Both live modes run this check before preparing signed-in homes, within the
existing whole-run wall ceiling. Before trusting anything, the runner validates
all five exact staged hook definitions and referenced plugin/candidate/meter
bytes. It writes only their reviewed hashes to the dedicated OM home, verifies
them through a fresh server, and checks identities again before every live turn.
Native hooks and unexpected definitions are refused. Only reviewed trust entries
and their exact serialized empty defaults are normalized in config comparison;
all residual settings remain comparable.

The first original OM fixture turn establishes actual live hooks and prime before
native workload execution or either variant's generated batches. Its original
prompt and all reported input usage remain part of the run. Each following OM
turn must retain actual native and metered lifecycle evidence. Missing or failed
PostToolUse after a normally completed command invalidates that turn, including
after startup; delayed evidence from an earlier turn cannot satisfy it. A turn
without a completed command does not require PostToolUse, and interrupted commands
retain their distinct Interrupt obligation. Missing or failed
activation makes either paid mode invalid and cannot produce a quality tie or
release PASS. This changes the runner's frozen hash; preserve old pilot artifacts
and generate a new preview before any separately authorized live attempt.

The fixture separates model-visible files/events from gold cases. It includes
repeated corrections, rejected proposals, a completed migration, a changed
objective, an interrupted turn, Unicode and a 48KB log with a middle error.
After setup, fresh seeded incident batches provide real per-region request,
failure and latency analysis tasks. Complete new request observations are included
in each model-visible prompt as well as its workspace file, so arithmetic tools
cannot reduce the exposure to only tiny aggregate outputs. Batches differ in content and IDs; repeating
the fixture or merely counting turns never counts as a compaction.

The two variants receive the same ordered workload generator and initial files;
native may use ordinary notes/tools, and OM additionally receives its plugin.
Each advances that sequence until its target number of native compactions. OM
maintenance can change how many batches fit between compactions. Results include
batch hashes/counts/bytes and usage to reach the cycle target; they do not pretend
that unequal workload lengths measure identical work throughput.

## Frozen cases and live commands

A preview's `frozen-manifest.json` contains:

- `split_sha256`: selected case IDs, split, contents, workload/generator identity
  **and the runner source bytes**. A runner change invalidates the campaign hash.
- `rubric_sha256`: selected cases/workload/generator independent of runner bytes.
- `fixture_sha256`, `runner_sha256` and supplied candidate/plugin content hashes.

Pilot scores only pilot cases; release scores held-out release cases. Global
fixture validation checks split/marker separation, but unselected cases are not
passed to scoring or copied to model workspaces. Gold responses never generate
recovery output. Do not tune on release scores and then claim the same cases are
fresh held-out evidence. The two homes also retain release-exposure hashes and
refuse the same rubric after a live release attempt. This local record cannot
establish what was exposed outside this runner.

Before live execution, the operator must explicitly choose the model, reasoning,
wall seconds, input-token ceiling and paths. Sign in normally in two dedicated
Codex homes; do not copy another home's auth files. New homes may contain only
normal authentication/installation files (`auth.json`, `.credentials.json`,
`installation_id`) and the `log/` and `tmp/` directories created by normal login.
Those existing login artifacts are preserved, not adopted for deletion. Existing
sessions, configuration, plugins, caches and unrelated top-level state are refused;
symlinks or unsupported file types anywhere in accepted home state are refused.
The runner validates actual account
sign-in and the model/reasoning advertised by that host.

After those choices, use the following commands. The variables name the actual
operator-approved values, not defaults. Both maximums cover **the entire run and
both variants**, including their model maintenance/compaction work.

```sh
# Set PILOT_PREVIEW and RELEASE_PREVIEW to the fresh previews above.
PILOT_SPLIT_HASH=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["split_sha256"])' "$PILOT_PREVIEW/frozen-manifest.json")
python3 scripts/evaluate_compaction.py --mode pilot --cycles 1 \
  --output "$PILOT_OUTPUT" --model "$MODEL" --reasoning "$REASONING" \
  --native-home "$NATIVE_HOME" --om-home "$OM_HOME" \
  --om-binary "$OM_BINARY" --plugin-root "$PLUGIN_ROOT" \
  --max-seconds "$PILOT_MAX_SECONDS" --max-input-tokens "$PILOT_MAX_INPUT_TOKENS" \
  --frozen-split-hash "$PILOT_SPLIT_HASH" --allow-paid-inference

RELEASE_SPLIT_HASH=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["split_sha256"])' "$RELEASE_PREVIEW/frozen-manifest.json")
python3 scripts/evaluate_compaction.py --mode release \
  --output "$RELEASE_OUTPUT" --model "$MODEL" --reasoning "$REASONING" \
  --native-home "$NATIVE_HOME" --om-home "$OM_HOME" \
  --om-binary "$OM_BINARY" --plugin-root "$PLUGIN_ROOT" \
  --max-seconds "$RELEASE_MAX_SECONDS" --max-input-tokens "$RELEASE_MAX_INPUT_TOKENS" \
  --frozen-split-hash "$RELEASE_SPLIT_HASH" --allow-paid-inference
```

Recreate the release preview after any pilot-driven runner/fixture change. Live
pilot may additionally use `--force-compaction` for diagnosis; it never qualifies
as release evidence. Release fixes both variants, 50 actual cycles, threshold
200000 and scope `total`; conflicting flags fail before process startup.

The same two dedicated homes can be reused for pilot then release. Before setup
or cleanup starts, `.om-eval-ownership.json` is atomically marked `in_progress`.
After verified host shutdown, finalization records hashes of recognized state
created by the runner/Codex, including its `cache/`, and marks ownership `complete`.
Before reuse, the runner verifies that complete record and removes only its recorded
synthetic state, including prior sessions, plugin configuration and stores. It
preserves original login artifacts and credential refreshes. Modified or unknown
state causes refusal; it is never silently deleted. New threads and stores are
created for the next run. Missing, incomplete or older unrecognized ownership
requires operator inspection or a fresh dedicated home; never remove the marker
to bypass this check.

Finalization attempts both homes and checks global configuration even when an
earlier step fails. Results retain per-home status, global-check status and every
finalization error, alongside the original run error and measured usage. A cleanup
failure alone also fails the run. An unverified host shutdown or failed home
finalizer leaves ownership incomplete and blocks automatic reuse. Interruptions
during cleanup are deferred until the remaining attempts finish, then re-raised
unless an earlier run error is already being preserved. A recorded run path must
match at finalization; a different run cannot claim its ownership record.

Candidate staging copies the plugin into the output directory, substitutes only
the temporary runtime version pin, installs the explicitly supplied binary with
`--from`, and verifies its content hash. It does not download a released runtime.
Results retain original/staged hashes and the version substitution. A temporary
binary wrapper measures CLI calls/bytes and successful explicit source deferrals,
including hook maintenance. The candidate bytes remain unchanged at
`bin/om-candidate`. Because Go advertises that running executable, the staged
bridge changes only the generated ledger command to
`PATH='<staged bin directory>' om --store '<canonical store>' --session '<session>'`.
The assignment affects only that invocation and routes the exact advertised
command through the meter. Native scoped bare-`om` recognition continues to
exclude memory commands from source capture. Store/session quoting is preserved,
including spaces, apostrophes and Unicode. This temporary transformation is
recorded in `meter_staging` metadata.

A Stop reason is also stored verbatim by the runtime for continuation ownership.
The bridge records the exact raw/emitted reason per session and restores the
native reason only when that same session delivers the exact emitted
`UserPromptSubmit` prompt. Similar ordinary user text and other sessions are
unchanged. The bridge never edits ledger state. Meter records distinguish emitted
bytes from candidate bytes and record command/prompt transformations. Hook
unavailability is classified from the runtime's structural `systemMessage`
advisory or failure response, not the word “unavailable” inside healthy guidance. Effective native memories use/generation are disabled
in both processes. Actual schemas, model/reasoning, skills and effective settings
are checked afresh; unsupported contracts, rerouting, mismatched tools, unknown
approval requests or missing usage stop the run. Global configuration is never
written and its before/after hashes are compared. Configuration comparison allows
only the staged `observational-memory@om-evaluation` plugin with `enabled = true`
and its `om-evaluation` local marketplace. The marketplace source must be an
absolute path resolving to this run's `candidate-market` directory; `ref`,
`last_revision`, `last_updated`, and `sparse_paths` must be absent or null, and
unknown registration options are refused. Native must have no plugin entries or
OM marketplace registration. Other marketplace entries and all remaining settings
must match. Each variant's exact writable workspace/store roots and base skill
contents are checked separately before either variant starts a thread.

## Scoring and measurements

Only distinct `item/completed` items with type `contextCompaction` and stable
thread/item IDs count. Each cycle needs its own subsequent probe turn and actual
answer. A second compaction before that recovery finishes leaves the experiment
ineligible; one answer is never copied across cycles. Checkpoints at 10/25/50
reuse the frozen rubric. The interrupted workload must produce an interrupted
turn for release eligibility.

Scoring uses exact short facts and verbatim source path/line/text. Action probes
must create a new cycle-specific artifact and preserve the original completed
migration record. Executed migration commands are also recorded across all turns,
including failures or later undo. Ordinary Python script/module selection,
interpreter flags, normalized paths and simple shell wrappers are recognized;
`python3 -m py_compile actions/migrate.py` only compiles and does not count as
running the migration. Reading/quoting a script also does not count. Dynamic code,
aliases, imports from arbitrary programs and complex shell control flow are not
generally interpreted. This is behavioral instrumentation, not a security boundary
against a malicious agent. OM's deferred-log setup requires a successful explicit
apply and complete retained source verified through bounded public recall pages.
The audit validates source identity, tool kind, contiguous UTF-8 ranges and
incompleteness flags. It accepts an exact raw tool capture or the exact decoded
string response of the native `{tool,input,response}` PostToolUse envelope. JSON
escaping does not count as truncation; an error marker or substring alone does
not count as complete recovery. Unsupported response representations remain
unverified. Deferral audit metadata remains required.
Coverage is not a claim of understanding.

Release quality requires 50 scored actual cycles per variant, every OM critical
fact/action correct, no stale correction or repeated completed action, exact-source
checks, and OM noncritical accuracy at least native's. Other host/isolation/evidence
requirements must also pass. Ties are reported as equal quality, not superiority.

Usage comes from cumulative `thread/tokenUsage/updated.tokenUsage.total` deltas;
`last` is retained separately in `usage_observations.active_context`, with its
provenance. It can contain request usage or a context-only estimate; those are not
interchangeable. Each compaction retains observation IDs and accepts post-context
evidence only from a same-turn context-only estimate between its start and the
next ordinary work item/turn boundary. The estimate may arrive before or after
the completion event. The supported Codex 0.153.4 unpaid host trace supplies this
estimate with positive `last.totalTokens` and zero `last.inputTokens` and
`last.outputTokens`; a compaction request's usage snapshot is not that estimate.
Missing, duplicate, or ambiguously associated context evidence remains null and
ineligible. Results grade `configured_policy_compaction_verified`: the effective
200000/total policy, a fresh live driver-owned thread and normal requested turn,
no manual/forced compaction or model reroute, matching compaction start/completion,
and an associated smaller context-only estimate. Replay lacks live ownership and
cannot earn this grade. This is evidence of host compaction under the configured
policy, not proof of the numerical trigger or its cause.

The prior request's `last.totalTokens` is preserved as `before_last_total_tokens`,
with `before_last_kind`; it is not labeled the active-context size. The separate
`observed_request_at_or_above_threshold` comparison can be false while the
configured-policy grade passes. `trigger_context_tokens` and `trigger_reason`
remain null, including when that request comparison is true. Codex's internal
trigger calculation adds locally estimated history after the last model output
(and sometimes prior reasoning) to the last request total. Public lifecycle items
do not expose those estimates or distinguish context-limit, model-requested new
window, comp_hash-change, and model-window-change causes. No tolerance or private
transcript reconstruction is used; lifetime/cached cumulative tokens never become
trigger measurements. `context_reduction_verified` independently reports whether
the observed post-context estimate is smaller than the preceding last sample.

These semantics are grounded in the official Codex 0.153.4 source, commit
[`3d2ee51`](https://github.com/openai/codex/tree/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a):
[active-context calculation](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/context_manager/history.rs#L525),
[scope/limit evaluation](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/session/context_window.rs#L57),
and [post-compaction estimate](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/session/mod.rs#L4412).
The [configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
and [App Server contract](https://learn.chatgpt.com/docs/app-server) supply the
public policy and lifecycle. This correction does not rescore or promote prior
failed pilot artifacts. The full 50-cycle recovery and release gates remain.

Optional absent fields remain null. Cached input is already part of input; reasoning output is reported
separately without adding it to output twice. Counter resets require an explicit
new segment in replay; an unexplained live reset stops execution. Stale start-of-turn
snapshots cannot stand in for completed work's usage. Before another turn starts,
the completed turn must have fresh same-turn cumulative input telemetry beyond
the snapshot at its final agent item. A duplicate intermediate snapshot or another
turn's update cannot satisfy this obligation. The supported native host emits
final response usage after that item; other ambiguous orderings stop the run.

The monotonic wall deadline and observed input ceiling are shared across both
variants. Notifications are processed even while waiting for RPC replies, and
shutdown retains final usage/overshoot. Shutdown interrupts only the owned task
and terminates only its child process if needed. Nonblocking writes share the
run deadline and drain output while waiting. Continuous stderr cannot extend a
receive deadline. Cleanup uses independent short bounds (0.25 seconds for an
interrupt write, 0.5 seconds for telemetry draining, 0.25 seconds for graceful
exit, then one second each for terminate and kill waits). A partially written
request makes the transport unusable; cleanup still reaches terminate/kill.
Cleanup may finish after the wall deadline. Telemetry can overshoot between observations: this is **not** a hard
per-request cap, a subscription allowance measurement or a dollar conversion.

## Replay format

Replay is diagnostic and never sets release PASS, even with 50 perfect synthetic
cycles. Its JSON envelope contains `schema: 1`, the matching `split_hash`,
`compact_limit`, `scope` and ordered `events`. Records are scoped to `variant`
(`native` or `om`) and have one of these forms:

```json
{"variant":"native","kind":"thread","thread_id":"synthetic-thread"}
{"variant":"native","kind":"event","event":{"method":"item/completed","params":{"threadId":"synthetic-thread","turnId":"t1","item":{"type":"contextCompaction","id":"c1"}}}}
{"variant":"native","kind":"probe_start","cycle":1,"turn_id":"probe-1"}
{"variant":"native","kind":"probe_result","cycle":1,"turn_id":"probe-1","response":{"answers":{}},"effects":{}}
```

`event` records carry ordinary host usage/item/turn notifications. A counter reset
uses `kind: "segment"`, a unique `id`, explicit nonnegative `baseline` counters and
`reason`; it does not reset the shared allowance. `effects` represents recorded
local action evidence for replay, never gold answers. Run it with:

```sh
python3 scripts/evaluate_compaction.py --mode replay \
  --replay-events /absolute/synthetic-events.json --output /absolute/fresh/replay
```

Exit 2 means invalid arguments/prerequisites before output creation; exit 1 means
an experiment failure or a nonpassing live release; exit 0 means diagnostic
completion or a passing live release. Always inspect `status`, `release_pass` and
`eligibility_reasons`. The T8 deterministic checks establish runner behavior only;
T9 owns the explicitly authorized live pilot, full experiment and release.

References: [official Codex App Server documentation](https://learn.chatgpt.com/docs/app-server)
and [configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).
The installed host, rather than an old example, supplies enum spellings such as
`workspace-write`. Native memory flags are verified in the effective config even
when they are absent from the generated typed `Config` properties.
