//! Linux ublk kernel interface for read-only block devices backed by cached
//! objects. See `designs/racer-ublk.md`.
//!
//! This module holds the UAPI mirrors, the blocking control channel, and pure
//! queue helpers. It does not use the reactor.

// Removed once the worker integration lands and uses these items.
#![allow(dead_code)]

pub(crate) mod control;
pub(crate) mod queue;
pub(crate) mod service;
pub(crate) mod uapi;

#[cfg(test)]
mod tests;
