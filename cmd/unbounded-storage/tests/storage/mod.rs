// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Storage engine DST: sim-aware [`BlockDevice`] mock, workload
//! model, oracle, and invariant tests. Mirrors the bufferpool DST
//! layout. The generic framework under `tests/framework/` knows
//! nothing about engines or block devices; this module wires the
//! `StorageEngine` to the seeded executor and PRNG.

#![allow(clippy::arc_with_non_send_sync)]

pub mod mocks;
pub mod oracle;
pub mod recovery;
pub mod snapshot;
pub mod tests;
pub mod workload;
