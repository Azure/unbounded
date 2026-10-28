//! Sans-IO control plane steps.
//!
//! Each step is a small state machine or pure function that takes the
//! current monotonic time (`now: Instant`), wall time where a protocol needs
//! it, an explicit [`Entropy`] source, and typed observations, and returns a
//! decision. The production tasks in `seat.rs`, `primary.rs`, and
//! `server.rs` perform the I/O (Lease reads and writes, Secret reads, image
//! copies, sleeps) and call these steps for every decision, so the simulator
//! (`racer-sim`) drives the same rules without sockets, TLS, or threads.
//!
//! - [`lottery`]: seat lottery pacing, candidate choice, and claim check.
//! - [`renew`]: seat renewal schedule, release reasons, conflict handling.
//! - [`follower`]: committed record admission and the need-image rule.
//! - [`contend`]: primary Lease contention.
//! - [`primary`]: one primary reconcile pass and discovery annotations.
//! - [`admission`]: seat server admission (write and bootstrap slots, long
//!   poll cursors, delta versus full image bodies).
//! - [`authz`]: bearer authorization cache and API budget.

pub mod admission;
pub mod authz;
pub mod contend;
pub mod follower;
pub mod lottery;
pub mod primary;
pub mod renew;
