//! The scope-aware reactor lives in [`racer_io::scoped`].
pub use racer_io::scoped::*;

#[cfg(test)]
/// Deterministic resource and completion backend for simulation runs.
pub mod simulation;
