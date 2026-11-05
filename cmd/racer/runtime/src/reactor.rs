//! The scope-aware reactor lives in [`racer_io::scoped`].
pub use racer_io::scoped::*;

#[cfg(feature = "simulation")]
/// Deterministic resource and completion backend for simulation runs.
pub mod simulation;

pub use crate::filesystem;
