use serde_json::{Value, json};
use std::{
    io::Write,
    process::{Command, Stdio},
};
use tempfile::TempDir;

fn cli(args: &[&str], input: Option<Value>) -> std::process::Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_observational-memory"))
        .args(args)
        .env_remove("OBSERVATIONAL_MEMORY_STORE")
        .env_remove("PLUGIN_DATA")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    if let Some(input) = input {
        child
            .stdin
            .take()
            .unwrap()
            .write_all(input.to_string().as_bytes())
            .unwrap();
    }
    child.wait_with_output().unwrap()
}

#[test]
fn compatibility_is_explicit_and_needs_no_store() {
    let output = cli(&["capabilities"], None);
    assert!(output.status.success());
    let capabilities: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(capabilities["protocol_version"], 1);
    assert_eq!(capabilities["clients"], json!(["codex"]));
    assert!(
        cli(
            &[
                "check-compatibility",
                "--protocol",
                "1",
                "--client",
                "codex"
            ],
            None
        )
        .status
        .success()
    );
    assert!(
        !cli(
            &[
                "check-compatibility",
                "--protocol",
                "2",
                "--client",
                "codex"
            ],
            None
        )
        .status
        .success()
    );
    assert!(
        !cli(&["hook", "--client", "claude-code"], None)
            .status
            .success()
    );
    assert!(!cli(&["hook"], None).status.success());
}

#[test]
fn core_commands_work_without_a_host_plugin() {
    let dir = TempDir::new().unwrap();
    let store = dir.path().to_str().unwrap();
    let output = cli(
        &["--store", store, "--session", "manual", "capture"],
        Some(json!({"kind":"user", "text":"Retain exact evidence.", "key":"manual-1"})),
    );
    assert!(output.status.success());
    let captured: Value = serde_json::from_slice(&output.stdout).unwrap();
    let output = cli(&["--store", store, "--session", "manual", "pending"], None);
    let pending: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(pending["sources"][0]["id"], captured["source_id"]);
    let output = cli(
        &["--store", store, "--session", "manual", "apply"],
        Some(json!({
            "through": pending["through"], "observations":[{"text":"Exact evidence is required.", "source_ids":[captured["source_id"]]}]
        })),
    );
    assert!(output.status.success());
    let receipt: Value = serde_json::from_slice(&output.stdout).unwrap();
    let output = cli(
        &[
            "--store",
            store,
            "--session",
            "manual",
            "recall",
            receipt["observations"][0].as_str().unwrap(),
        ],
        None,
    );
    let recalled: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(recalled["sources"][0]["text"], "Retain exact evidence.");
    assert!(
        !cli(&["--session", "manual", "status"], None)
            .status
            .success()
    );
}

#[test]
fn plugin_environment_does_not_silently_select_a_core_store() {
    let dir = TempDir::new().unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_observational-memory"))
        .args(["--session", "manual", "status"])
        .env_remove("OBSERVATIONAL_MEMORY_STORE")
        .env("PLUGIN_DATA", dir.path())
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 0);
    let output = Command::new(env!("CARGO_BIN_EXE_observational-memory"))
        .args(["--session", "manual", "status"])
        .env("OBSERVATIONAL_MEMORY_STORE", dir.path())
        .output()
        .unwrap();
    assert!(output.status.success());
}
