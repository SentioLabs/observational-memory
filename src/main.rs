use anyhow::{Context, Result};
use clap::{Parser, Subcommand, ValueEnum};
use observational_memory::{Ledger, PROTOCOL_VERSION, SourceKind, adapters::codex};
use serde::Deserialize;
use serde_json::{Value, json};
use std::{
    io::{self, Read},
    path::PathBuf,
};

#[derive(Parser)]
#[command(
    name = "observational-memory",
    version,
    about = "Local evidence-backed memory for coding agents"
)]
struct Args {
    #[arg(long, env = "OBSERVATIONAL_MEMORY_STORE")]
    store: Option<PathBuf>,
    #[arg(long)]
    session: Option<String>,
    #[command(subcommand)]
    command: Command,
}

#[derive(Clone, Copy, ValueEnum)]
enum Client {
    Codex,
}

#[derive(Subcommand)]
enum Command {
    /// Handle a lifecycle event from an explicitly selected agent client.
    Hook {
        #[arg(long, value_enum)]
        client: Client,
    },
    /// Report the CLI contract and currently implemented client adapters.
    Capabilities,
    /// Exit unsuccessfully when a plugin requires an incompatible contract.
    CheckCompatibility {
        #[arg(long)]
        protocol: u32,
        #[arg(long, value_enum)]
        client: Client,
    },
    Pending,
    Apply,
    Status,
    Pause,
    Resume,
    View {
        #[arg(long)]
        all: bool,
    },
    Recall {
        id: String,
    },
    Capture,
    Fork {
        #[arg(long)]
        to_session: String,
    },
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Capture {
    kind: SourceKind,
    text: String,
    key: String,
}

fn input() -> Result<String> {
    let mut text = String::new();
    io::stdin().take(4_000_001).read_to_string(&mut text)?;
    anyhow::ensure!(text.len() <= 4_000_000, "Input exceeds 4MB limit");
    Ok(text)
}

fn run(args: &Args) -> Result<Value> {
    match args.command {
        Command::Capabilities => {
            return Ok(json!({
                "version": env!("CARGO_PKG_VERSION"), "protocol_version": PROTOCOL_VERSION,
                "ledger_schema": 1, "clients": ["codex"]
            }));
        }
        Command::CheckCompatibility {
            protocol,
            client: Client::Codex,
        } => {
            anyhow::ensure!(
                protocol == PROTOCOL_VERSION,
                "Unsupported CLI protocol {protocol}; this runtime supports {PROTOCOL_VERSION}"
            );
            return Ok(json!({"compatible":true}));
        }
        _ => {}
    }
    let store = args.store.as_deref().context(
        "Set --store or OBSERVATIONAL_MEMORY_STORE; no implicit global/project fallback",
    )?;
    if let Command::Hook { client } = args.command {
        return match client {
            Client::Codex => codex::hook(
                serde_json::from_str(&input()?)?,
                store,
                &std::env::current_exe()?,
            ),
        };
    }
    let mut ledger = Ledger::open(
        store,
        args.session.as_deref().context("--session is required")?,
    )?;
    Ok(match &args.command {
        Command::Pending => serde_json::to_value(ledger.pending()?)?,
        Command::Apply => ledger.apply(serde_json::from_str(&input()?)?)?,
        Command::Status => ledger.status()?,
        Command::Pause | Command::Resume => {
            ledger.pause(matches!(args.command, Command::Pause))?;
            ledger.status()?
        }
        Command::View { all: true } => serde_json::to_value(ledger.entries(true)?)?,
        Command::View { all: false } => json!(ledger.view()?),
        Command::Recall { id } => ledger.recall(id)?,
        Command::Capture => {
            let capture: Capture = serde_json::from_str(&input()?)?;
            json!({"source_id":ledger.capture(capture.kind,&capture.text,&capture.key)?})
        }
        Command::Fork { to_session } => ledger.fork(store, to_session)?,
        Command::Hook { .. } | Command::Capabilities | Command::CheckCompatibility { .. } => {
            unreachable!()
        }
    })
}

fn main() {
    let args = Args::parse();
    match run(&args) {
        Ok(Value::String(text)) => println!("{text}"),
        Ok(result) => println!("{result}"),
        Err(error) if matches!(args.command, Command::Hook { .. }) => {
            eprintln!("observational-memory: {error:#}");
            println!(
                "{}",
                json!({"systemMessage":"Observational memory unavailable; use its status command to diagnose. The task can continue."})
            );
        }
        Err(error) => {
            eprintln!("observational-memory: {error:#}");
            std::process::exit(1);
        }
    }
}
