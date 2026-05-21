// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Mercury DST: project-specific mocks, workload model, and invariant
//! tests for the Mercury bulk-get RPC. The framework crate under
//! `tests/framework/` is deliberately ignorant of everything in here;
//! this module is where Mercury plugs into the generic executor + PRNG.
//!
//! Production `MercuryTransport` is `Send + Sync`, but the DST mocks
//! are intentionally `!Send` (they hold `Rc`/`RefCell`) so a single
//! deterministic executor can drive them without any cross-thread
//! synchronization. The `arc_with_non_send_sync` allow below is what
//! the bufferpool plug-in uses for the same reason.

#![allow(dead_code)]
#![allow(clippy::arc_with_non_send_sync)]

mod mocks;
mod oracle;
mod recovery;
mod tests;
mod workload;
