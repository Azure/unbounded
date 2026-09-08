// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Racer's dataplane.
//!
//! The allocator, cache, consensus and anti-entropy are private: a block request reaches
//! a page only through `server`.

// Kernel-facing code (SQE construction, ublk arming, the fixed-file table) is
// unreachable under the simulator but still compiles; allowing dead code here beats
// scattering cfg attributes through the runtime.
#![cfg_attr(feature = "sim", allow(dead_code, unused_imports))]

mod alloc;
mod cache;
pub mod config;
pub mod fabric;
mod heal;
pub mod layout;
pub mod metrics;
mod paxos;
pub mod runtime;
pub mod server;

/// Deterministic simulation: a whole cluster in one thread.
#[cfg(feature = "sim")]
pub mod sim;
