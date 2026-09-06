//! Evidence-backed memory independent of any agent client.
pub mod adapters;
mod ledger;
pub use ledger::{
    Checkpoint, Entry, Importance, Kind, Ledger, Observation, Pending, Reflection, Retirement,
    Source, SourceKind, VIEW_LIMIT, redact,
};

/// Version of the CLI/JSON contract consumed by marketplace plugins.
pub const PROTOCOL_VERSION: u32 = 1;
