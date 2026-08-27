//! Racer's dataplane.
//!
//! The developer API reference includes private items, and its build rejects hidden
//! documentation attributes. Racer's module boundaries use `pub(crate)` and `pub(super)`
//! extensively, so public-only documentation would omit interfaces that sibling modules
//! depend on. Run `make racer-docs` from the repository root to build the complete reference.

// Public modules deliberately link to crate-private APIs in the internal reference.
#![allow(rustdoc::private_intra_doc_links)]

mod alloc;
mod cache;
pub mod config;
pub mod fabric;
mod kernel;
pub use alloc::layout;
pub mod metrics;
mod paxos;
pub mod runtime;
pub mod server;
pub mod sim;
