# Language and storage decision

Use Go with SQLite for the standalone CLI. Rust has lower startup overhead in
the local probe, but Go fits the existing SentioLabs CLI ecosystem: arc already
uses modernc.org/sqlite, and arc and envctl use go-selfupdate. Reusing those
libraries and a CGO-free build is more valuable for this workload than a few
milliseconds of startup difference.

A 60-sample interleaved local probe on macOS arm64 (2026-09-06) opened a SQLite
file, created its table if absent, committed approximately 2 KB of JSON using
BEGIN IMMEDIATE, counted rows, and closed. It used each driver's default journal
and synchronous settings, optimized builds, and warm OS caches.

| Stack | Median process time | p95 | Median database work |
| --- | ---: | ---: | ---: |
| Rust 1.97.1 + rusqlite 0.40.2 | 3.4 ms | 3.8 ms | 0.66 ms |
| Go 1.26.5 + modernc.org/sqlite 1.44.2 | 6.4 ms | 7.2 ms | 0.83 ms |
| Go 1.26.5 + mattn/go-sqlite3 1.14.32 (CGO) | 5.4 ms | 5.9 ms | 0.74 ms |

This is a small storage/startup probe, not a benchmark of the complete CLI,
language throughput, or large ledgers. The compared drivers can embed different
SQLite versions. Rust wins this probe; Go is the maintenance and distribution
choice. Keep the existing ledger invariants covered by tests as the Go port
evolves, and measure actual hook latency before optimizing.

Keep SQLite rather than switching to Stoolap. The current short-lived hook
process model benefits from SQLite's coordination between processes. Stoolap's
single-process database ownership would require additional locking or a
persistent worker, and its lifecycle cost was higher in the local evaluation.

- [Modernc SQLite](https://pkg.go.dev/modernc.org/sqlite)
- [go-selfupdate](https://github.com/SentioLabs/go-selfupdate)
- [Stoolap persistence and process ownership](https://stoolap.io/docs/architecture/persistence/)
