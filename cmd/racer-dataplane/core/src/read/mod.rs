//! Sans-IO read-path decisions: acquisition budgets, candidate ordering and
//! response classification, fill/serve policy, and the candidate walk.
//!
//! The dataplane re-exports these from their historical paths under
//! `racer_dataplane::read`, so there is one implementation of each rule.

pub mod budget;
pub mod candidates;
pub mod error;
pub mod policy;
pub mod walk;
pub mod worker;

pub use budget::AcquisitionBudget;
pub use error::{Error, Result};
