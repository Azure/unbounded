//! Linux ublk read-only block devices backed by cached objects. See
//! `designs/racer-ublk.md`.
//!
//! - `uapi`, `control`, and `queue` mirror the kernel interface.
//! - `manager` is the node-level device table and `ublk-control` thread.
//! - `worker` attaches each worker's queues and serves them on its reactor.
//! - `service` is the per-tag request loop.
//! - `directory` reads pages through the dataplane read path.

pub(crate) mod control;
pub(crate) mod directory;
pub(crate) mod manager;
pub(crate) mod queue;
pub(crate) mod service;
pub(crate) mod uapi;
pub(crate) mod worker;

#[cfg(test)]
mod tests;
