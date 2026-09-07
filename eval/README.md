# Compaction evaluation harness (development checkpoint)

This opt-in Python 3.10+ harness compares newly created synthetic native Codex and
Observational Memory tasks. It does not control an existing Desktop task. The
public `om` CLI remains Go-only.

**WIP checkpoint:** deterministic unit tests currently pass, but the live driver
has not completed integration review or the project gates. Do not use this
checkpoint as release evidence. Resume from the preserved Arc T8 pause report.

Run unpaid checks:

```sh
python3 -m unittest discover -s tests -p 'test_evaluate_compaction.py'
python3 scripts/evaluate_compaction.py --mode dry-run --output /absolute/fresh/eval-preview
```

Dry-run and replay do not launch subprocesses or require authentication. Both
write `results.json`, `report.md`, and a frozen fixture manifest. Dry-run prepares
equivalent workspaces and a preview; it does not generate recovery scores.

Fixture cases and gold rubric are separate from the model-visible workload.
Pilot selects only pilot cases; release selects held-out release cases. Preserve
the split and runner hashes before tuning. Do not retune using held-out results
and then report those same cases as fresh release evidence.

Live pilot/release modes require explicit model/reasoning, separate unused Codex
homes signed in through normal Codex authentication, candidate binary/plugin,
fresh absolute output, positive maximum wall seconds and reported input tokens,
the frozen split hash, and `--allow-paid-inference`. No values are selected on the
user's behalf. Homes must be outside results and differ from active/global homes.
The runner never copies credentials. Candidate plugin staging substitutes its
runtime version only in a temporary copy, installs the supplied binary, and
records both original and staged hashes.

Release fixes both variants, 50 distinct actual completed compactions per variant,
200000/total native compaction and a separate recovery probe after every cycle.
Forced compaction is diagnostic pilot-only. Scores use exact facts, source lines
and action evidence; a functioning runner does not imply quality passed.

Input and wall limits cover the whole run, including both variants and observed
maintenance. Cumulative usage is differenced; cached input and reasoning output
are not added twice. Missing telemetry stops live work. Usage may overshoot
between notifications, so this is not a hard per-request billing cap. Unavailable
measurements are null. Ties do not establish superiority.

Protocol reference: [official Codex App Server documentation](https://learn.chatgpt.com/docs/app-server).
The live runner freshly generates installed schemas and reads effective settings;
cached schema evidence is not release evidence.
