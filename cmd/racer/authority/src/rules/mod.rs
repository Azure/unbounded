//! Pure authority rules: durable state, election, membership, keyring,
//! publication, and the sans-IO control plane steps.
//!
//! Every rule takes explicit time inputs (monotonic and, where a protocol
//! needs it, wall time), an explicit [`crate::entropy::Entropy`] source, and
//! typed observations, and returns a decision. Nothing here reads a clock,
//! spawns a thread, or touches the filesystem or network; `tests/purity.rs`
//! enforces this. The production tasks in `seat.rs`, `primary.rs`, and
//! `server.rs` perform the I/O and interpret these rules, so the deterministic
//! simulation (`racer-dst`) drives the same rules.
//!
//! - [`shell`]: the authority shell machine: lease freshness, the state
//!   write protocol (fencing, monotonic checks, conflict retries), and seat
//!   replica acceptance, as events in and effects out.
//! - [`seat`]: seat lottery, renewal and release, following committed state.
//! - [`primary`]: primary Lease contention, one reconcile pass, discovery.
//! - [`admission`]: seat server admission (write and bootstrap slots, long
//!   poll cursors, delta versus full image bodies).
//! - [`authz`]: bearer authorization cache and API budget.
//! - [`state`], [`election`], [`members`], [`catalog`], [`pools`], [`rails`],
//!   [`keyring`], [`publication`]: durable records and their derivations.

pub mod admission;
pub mod authz;
pub mod catalog;
pub mod election;
pub mod keyring;
pub mod members;
pub mod pools;
pub mod primary;
pub mod publication;
pub mod rails;
pub mod seat;
pub mod shell;
pub mod state;
