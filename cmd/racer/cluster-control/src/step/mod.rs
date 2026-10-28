//! Sans-IO control plane steps.
//!
//! Each step is a small state machine or pure function that takes the
//! current monotonic time, wall time where a protocol needs it, an explicit
//! [`crate::entropy::Entropy`] source, and typed observations, and returns a
//! decision. The production tasks in `seat.rs`, `primary.rs`, and
//! `server.rs` perform the I/O and call these steps for every decision, so
//! the simulator (`racer-sim`) drives the same rules.
//!
//! - [`seat`]: seat lottery, renewal and release, following committed state.
//! - [`primary`]: primary Lease contention, one reconcile pass, discovery.
//! - [`admission`]: seat server admission (write and bootstrap slots, long
//!   poll cursors, delta versus full image bodies).
//! - [`authz`]: bearer authorization cache and API budget.

pub mod admission;
pub mod authz;
pub mod primary;
pub mod seat;
