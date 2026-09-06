use observational_memory::adapters::codex::hook;
use observational_memory::{Checkpoint, Ledger, SourceKind, VIEW_LIMIT, redact};
use serde_json::{Value, json};
use std::{
    io::Write,
    path::Path,
    process::{Command, Stdio},
};
use tempfile::TempDir;

fn apply(ledger: &mut Ledger, value: Value) -> anyhow::Result<Value> {
    ledger.apply(serde_json::from_value::<Checkpoint>(value)?)
}

fn observe(ledger: &mut Ledger, text: &str) -> (i64, String, String) {
    let sid = ledger.capture(SourceKind::User, text, text).unwrap();
    let through = ledger.pending().unwrap().through;
    let response = apply(ledger,json!({"through":through,"observations":[{"text":text,"importance":"high","source_ids":[sid]}]})).unwrap();
    (
        through,
        sid,
        response["observations"][0].as_str().unwrap().into(),
    )
}

#[test]
fn observations_reflections_retirement_and_recall_survive_reopen() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "session-a").unwrap();
    let (through, sid, oid) = observe(&mut ledger, "Use SQLite so the tool works offline.");
    let receipt = apply(&mut ledger,json!({"through":through,"reflections":[{"text":"Offline operation requires SQLite.","observation_ids":[oid]}]})).unwrap();
    let rid = receipt["reflections"][0].as_str().unwrap();
    apply(&mut ledger,json!({"through":through,"retire":[{"id":oid,"reason":"Preserved in durable reflection", "replacement_ids":[rid]}]})).unwrap();
    drop(ledger);
    let ledger = Ledger::open(dir.path(), "session-a").unwrap();
    assert_eq!(ledger.entries(false).unwrap().len(), 1);
    assert_eq!(ledger.entries(true).unwrap().len(), 2);
    assert_eq!(ledger.recall(rid).unwrap()["sources"][0]["id"], sid);
    assert_eq!(ledger.recall(&oid).unwrap()["entry"]["active"], false);
    assert!(ledger.pending().unwrap().sources.is_empty());
}

#[test]
fn invalid_reference_rolls_back_entire_checkpoint_and_progress() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "rollback").unwrap();
    let sid = ledger
        .capture(SourceKind::User, "Keep the API stable.", "turn1")
        .unwrap();
    let result = apply(
        &mut ledger,
        json!({"through":1,"observations":[
        {"text":"Stable API required.","source_ids":[sid]},
        {"text":"Made up evidence.","source_ids":["s-missing"]}]}),
    );
    assert!(result.is_err());
    assert!(ledger.entries(true).unwrap().is_empty());
    assert_eq!(ledger.pending().unwrap().sources.len(), 1);
}

#[test]
fn duplicates_empty_checkpoints_and_stale_cursors() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "idempotent").unwrap();
    let (through, sid, oid) = observe(&mut ledger, "Use Rust.");
    let payload = json!({"through":through,"observations":[{"text":"Use Rust.","importance":"high","source_ids":[sid]}]});
    assert_eq!(apply(&mut ledger, payload).unwrap()["observations"][0], oid);
    assert_eq!(ledger.entries(true).unwrap().len(), 1);
    ledger
        .capture(SourceKind::User, "Routine acknowledgement.", "turn2")
        .unwrap();
    let latest = ledger.pending().unwrap().through;
    apply(&mut ledger, json!({"through":latest,"observations":[]})).unwrap();
    assert!(ledger.pending().unwrap().sources.is_empty());
    assert!(apply(&mut ledger, json!({"through":through})).is_err());
    assert!(apply(&mut ledger, json!({"through":latest+10})).is_err());
}

#[test]
fn cannot_retire_without_support_or_use_reflection_as_observation() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "support").unwrap();
    let (_, _, first) = observe(&mut ledger, "Rust is required.");
    let (through, _, second) = observe(&mut ledger, "Database must be offline.");
    let receipt = apply(&mut ledger,json!({"through":through,"reflections":[{"text":"Offline database.","observation_ids":[second]}]})).unwrap();
    let rid = &receipt["reflections"][0];
    assert!(apply(&mut ledger,json!({"through":through,"retire":[{"id":first,"reason":"Not actually covered","replacement_ids":[rid]}]})).is_err());
    assert!(apply(&mut ledger,json!({"through":through,"reflections":[{"text":"Reflection of reflection.","observation_ids":[rid]}]})).is_err());
    assert!(apply(&mut ledger,json!({"through":through,"retire":[{"id":first,"reason":"No justification","replacement_ids":[]}]})).is_err());
    assert_eq!(ledger.entries(false).unwrap().len(), 3);
}

#[test]
fn corrected_fact_replaces_old_one_without_erasing_evidence() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "correction").unwrap();
    let (_, _, old) = observe(&mut ledger, "Use Go.");
    let (through, _, new) = observe(
        &mut ledger,
        "User corrected the language to Rust, replacing Go.",
    );
    apply(&mut ledger,json!({"through":through,"retire":[{"id":old,"reason":"Explicit correction","replacement_ids":[new]}]})).unwrap();
    assert!(!ledger.view().unwrap().contains("Use Go."));
    assert!(ledger.recall(&old).unwrap().to_string().contains("Use Go."));
}

#[test]
fn sessions_and_explicit_forks_are_isolated() {
    let dir = TempDir::new().unwrap();
    let mut a = Ledger::open(dir.path(), "a").unwrap();
    let (_, _, oid) = observe(&mut a, "Keep this decision.");
    let b = Ledger::open(dir.path(), "b").unwrap();
    assert!(b.recall(&oid).is_err());
    assert!(a.fork(dir.path(), "b").is_err());
    a.fork(dir.path(), "child").unwrap();
    let mut child = Ledger::open(dir.path(), "child").unwrap();
    assert!(child.recall(&oid).is_ok());
    observe(&mut child, "Child-only correction.");
    assert_eq!(a.entries(false).unwrap().len(), 1);
    assert_eq!(child.entries(false).unwrap().len(), 2);
    assert!(a.fork(dir.path(), "child").is_err());
    let traversal = Ledger::open(dir.path(), "../../escape").unwrap();
    assert!(
        traversal
            .path
            .starts_with(dir.path().canonicalize().unwrap())
    );
}

#[test]
fn large_unicode_sources_are_bounded_and_backlog_drains_in_order() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "budget").unwrap();
    for i in 0..5 {
        ledger
            .capture(SourceKind::Tool, &"🙂".repeat(30_000), &i.to_string())
            .unwrap();
    }
    let mut seen = vec![];
    loop {
        let pending = ledger.pending().unwrap();
        if pending.sources.is_empty() {
            break;
        }
        for source in &pending.sources {
            assert!(source.truncated);
            assert!(source.text.chars().count() < 24_100);
            seen.push(source.seq);
        }
        apply(&mut ledger, json!({"through":pending.through})).unwrap();
    }
    assert_eq!(seen, vec![1, 2, 3, 4, 5]);
}

#[test]
fn bounded_view_preserves_high_priority_and_signals_omissions() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "view").unwrap();
    for i in 0..30 {
        observe(
            &mut ledger,
            &format!("Event {i}: {}", "detail ".repeat(100)),
        );
    }
    let sid = ledger
        .capture(
            SourceKind::User,
            "Never repeat the completed migration.",
            "last",
        )
        .unwrap();
    let through = ledger.pending().unwrap().through;
    apply(&mut ledger,json!({"through":through,"observations":[{"text":"Never repeat the completed migration.","importance":"critical","source_ids":[sid]}]})).unwrap();
    let text = ledger.view().unwrap();
    assert!(text.chars().count() <= VIEW_LIMIT);
    assert!(text.contains("Never repeat the completed migration."));
    assert!(!text.contains("(0 active entries omitted"));
    assert_eq!(ledger.entries(true).unwrap().len(), 31);
}

fn event(name: &str) -> Value {
    json!({"hook_event_name":name,"session_id":"hooks","turn_id":"turn-1"})
}

#[test]
fn hook_lifecycle_restores_memory_and_guards_stop_recursion() {
    let dir = TempDir::new().unwrap();
    let exe = Path::new("/example with spaces/observational-memory");
    let mut prompt = event("UserPromptSubmit");
    prompt["prompt"] = json!("Use Rust and preserve the API.");
    let output = hook(prompt, dir.path(), exe).unwrap();
    assert!(
        output["hookSpecificOutput"]["additionalContext"]
            .as_str()
            .unwrap()
            .contains("--session 'hooks'")
    );
    let mut stop = event("Stop");
    stop["last_assistant_message"] = json!("Implementation completed; tests passed.");
    assert_eq!(
        hook(stop.clone(), dir.path(), exe).unwrap()["decision"],
        "block"
    );
    assert_eq!(hook(stop.clone(), dir.path(), exe).unwrap(), json!({}));
    stop["turn_id"] = json!("turn-2");
    stop["stop_hook_active"] = json!(true);
    assert_eq!(hook(stop, dir.path(), exe).unwrap(), json!({}));
    let mut ledger = Ledger::open(dir.path(), "hooks").unwrap();
    observe(&mut ledger, "The API must stay compatible.");
    let mut start = event("SessionStart");
    start["source"] = json!("compact");
    let output = hook(start, dir.path(), exe).unwrap();
    assert_eq!(
        output["hookSpecificOutput"]["hookEventName"],
        "SessionStart"
    );
    assert!(output.to_string().contains("API must stay compatible"));
}

#[test]
fn hooks_skip_paused_subagents_and_memory_operations() {
    let dir = TempDir::new().unwrap();
    let exe = Path::new("/example/observational-memory");
    let ledger = Ledger::open(dir.path(), "hooks").unwrap();
    ledger.pause(true).unwrap();
    let mut prompt = event("UserPromptSubmit");
    prompt["prompt"] = json!("Do not retain this.");
    assert_eq!(hook(prompt.clone(), dir.path(), exe).unwrap(), json!({}));
    ledger.pause(false).unwrap();
    prompt["agent_id"] = json!("worker");
    hook(prompt, dir.path(), exe).unwrap();
    let mut tool = event("PostToolUse");
    tool["tool_input"] = json!({"cmd":"/example/observational-memory --session hooks status"});
    hook(tool, dir.path(), exe).unwrap();
    assert!(ledger.pending().unwrap().sources.is_empty());
}

#[test]
fn hook_threshold_reminds_once_per_cursor() {
    let dir = TempDir::new().unwrap();
    let exe = Path::new("/example/observational-memory");
    let mut tool = event("PostToolUse");
    tool["tool_response"] = json!("a".repeat(21_000));
    tool["tool_use_id"] = json!("one");
    assert_eq!(hook(tool.clone(), dir.path(), exe).unwrap(), json!({}));
    tool["tool_use_id"] = json!("two");
    assert!(
        hook(tool.clone(), dir.path(), exe)
            .unwrap()
            .get("hookSpecificOutput")
            .is_some()
    );
    tool["tool_use_id"] = json!("three");
    assert_eq!(hook(tool, dir.path(), exe).unwrap(), json!({}));
}

#[test]
fn concurrent_capture_does_not_lose_sources() {
    let dir = TempDir::new().unwrap();
    Ledger::open(dir.path(), "parallel").unwrap();
    let threads: Vec<_> = (0..8)
        .map(|i| {
            let path = dir.path().to_owned();
            std::thread::spawn(move || {
                let ledger = Ledger::open(&path, "parallel").unwrap();
                ledger
                    .capture(
                        SourceKind::Tool,
                        &format!("tool evidence {i}"),
                        &i.to_string(),
                    )
                    .unwrap();
            })
        })
        .collect();
    for thread in threads {
        thread.join().unwrap();
    }
    assert_eq!(
        Ledger::open(dir.path(), "parallel")
            .unwrap()
            .pending()
            .unwrap()
            .sources
            .len(),
        8
    );
}

#[test]
fn common_secrets_are_redacted_before_capture() {
    let text = "Bearer abc.def-secret sk-12345678901234567890 ghp_1234567890123456789012345\n-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----";
    let sanitized = redact(text);
    assert!(!sanitized.contains("secret"));
    assert!(!sanitized.contains("1234567890"));
    let dir = TempDir::new().unwrap();
    let ledger = Ledger::open(dir.path(), "secret").unwrap();
    let id = ledger.capture(SourceKind::Tool, text, "tool1").unwrap();
    assert!(!ledger.recall(&id).unwrap().to_string().contains("secret"));
}

#[test]
fn binary_hook_failures_are_advisory_and_cli_errors_are_nonzero() {
    let dir = TempDir::new().unwrap();
    let binary = env!("CARGO_BIN_EXE_observational-memory");
    let mut child = Command::new(binary)
        .args([
            "--store",
            dir.path().to_str().unwrap(),
            "hook",
            "--client",
            "codex",
        ])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(b"invalid json")
        .unwrap();
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success());
    assert!(
        serde_json::from_slice::<Value>(&output.stdout)
            .unwrap()
            .get("systemMessage")
            .is_some()
    );
    let output = Command::new(binary)
        .args(["--store", dir.path().to_str().unwrap(), "status"])
        .output()
        .unwrap();
    assert!(!output.status.success());
}

#[test]
fn recall_orders_sources_chronologically_without_duplicate_observation() {
    let dir = TempDir::new().unwrap();
    let mut ledger = Ledger::open(dir.path(), "chronology").unwrap();
    let ids: Vec<_> = ["First decision", "Second correction", "Third confirmation"]
        .into_iter()
        .map(|text| ledger.capture(SourceKind::User, text, text).unwrap())
        .collect();
    let receipt = apply(
        &mut ledger,
        json!({"through":3,"observations":[{
            "text":"The user corrected and confirmed the decision.","source_ids":ids
        }]}),
    )
    .unwrap();
    let result = ledger
        .recall(receipt["observations"][0].as_str().unwrap())
        .unwrap();
    let order: Vec<_> = result["sources"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| s["seq"].as_i64().unwrap())
        .collect();
    assert_eq!(order, vec![1, 2, 3]);
    assert!(result.get("observations").is_none());
}

#[test]
fn synthetic_stop_prompt_is_not_saved_as_user_evidence() {
    let dir = TempDir::new().unwrap();
    let exe = Path::new("/example/observational-memory");
    let mut prompt = event("UserPromptSubmit");
    prompt["prompt"] = json!("Keep the API stable.");
    prompt["agent_id"] = Value::Null;
    hook(prompt, dir.path(), exe).unwrap();
    let result = hook(event("Stop"), dir.path(), exe).unwrap();
    let mut continuation = event("UserPromptSubmit");
    continuation["prompt"] = result["reason"].clone();
    continuation["turn_id"] = json!("continuation");
    hook(continuation, dir.path(), exe).unwrap();
    let ledger = Ledger::open(dir.path(), "hooks").unwrap();
    let sources = ledger.pending().unwrap().sources;
    assert_eq!(sources.len(), 1);
    assert_eq!(sources[0].text, "Keep the API stable.");
}
