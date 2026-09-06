//! Codex lifecycle events. Plugin prompts and hook registration live in the marketplace.
use crate::{Ledger, SourceKind};
use anyhow::{Result, bail, ensure};
use serde_json::{Value, json};
use std::{fs, path::Path};

const OBSERVE_CHARS: u64 = 40_000;

fn quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}
fn field<'a>(event: &'a Value, key: &str) -> &'a str {
    event[key].as_str().unwrap_or("")
}
fn context(event: &str, text: String) -> Value {
    json!({"hookSpecificOutput":{"hookEventName":event,"additionalContext":text}})
}

pub fn hook(event: Value, store: &Path, executable: &Path) -> Result<Value> {
    ensure!(event.is_object(), "Hook input must be an object");
    let name = field(&event, "hook_event_name");
    if !["SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"].contains(&name)
        || !field(&event, "agent_id").is_empty()
    {
        return Ok(json!({}));
    }
    let session = field(&event, "session_id");
    let ledger = Ledger::open(store, session)?;
    if ledger.paused()? {
        return Ok(json!({}));
    }
    let command = format!(
        "{} --store {} --session {}",
        quote(&executable.to_string_lossy()),
        quote(&fs::canonicalize(store)?.to_string_lossy()),
        quote(session)
    );
    let guidance = format!(
        "Use $observational-memory to checkpoint new decisions, constraints, corrections, completions and blockers before ending substantive work. Ledger command: {command}. Read pending and apply a grounded checkpoint; an empty observation list is valid for routine content. This ledger is scoped to this session. Follow the user's current memory preferences. "
    );
    let turn = field(&event, "turn_id");
    match name {
        "SessionStart" => Ok(context(name, guidance + "\n" + &ledger.view()?)),
        "UserPromptSubmit" => {
            let prompt = field(&event, "prompt");
            // Codex can submit Stop continuations as user prompts. They are our
            // instructions, not new user evidence, and must not feed the ledger.
            if prompt == ledger.state("stop_prompt")? {
                return Ok(context(name, guidance));
            }
            if !prompt.trim().is_empty() {
                let id = ledger.capture(SourceKind::User, prompt, turn)?;
                ledger.set_state("prompt_id", &id)?;
            }
            Ok(context(name, guidance))
        }
        "PostToolUse" => {
            let input = event["tool_input"].to_string();
            if input.contains(&*executable.to_string_lossy()) || input.contains("scripts/run.sh") {
                return Ok(json!({}));
            }
            let text = json!({"tool":event["tool_name"],"input":event["tool_input"],"response":event["tool_response"]}).to_string();
            let key = if field(&event, "tool_use_id").is_empty() {
                turn
            } else {
                field(&event, "tool_use_id")
            };
            ledger.capture(SourceKind::Tool, &text, key)?;
            let cursor = ledger.checkpoint_cursor()?.to_string();
            if ledger.status()?["pending_chars"].as_u64().unwrap_or(0) >= OBSERVE_CHARS
                && ledger.state("notified_cursor")? != cursor
            {
                ledger.set_state("notified_cursor", &cursor)?;
                return Ok(context(
                    name,
                    guidance + "The observation checkpoint is due now.",
                ));
            }
            Ok(json!({}))
        }
        "Stop" => {
            let key = if turn.is_empty() {
                ledger.state("prompt_id")?
            } else {
                turn.to_owned()
            };
            if event["stop_hook_active"] == true
                || key.is_empty()
                || ledger.state("stop_turn")? == key
            {
                return Ok(json!({}));
            }
            let text = field(&event, "last_assistant_message");
            if !text.trim().is_empty() {
                ledger.capture(SourceKind::Assistant, text, &key)?;
            }
            if ledger.status()?["pending_sources"] == 0 {
                return Ok(json!({}));
            }
            let reason = guidance
                + "Perform one bounded memory pass now, then finish. If unavailable, skip it; do not retry indefinitely.";
            ledger.set_state("stop_turn", &key)?;
            ledger.set_state("stop_prompt", &reason)?;
            Ok(json!({"decision":"block","reason":reason}))
        }
        _ => bail!("Unsupported event"),
    }
}
