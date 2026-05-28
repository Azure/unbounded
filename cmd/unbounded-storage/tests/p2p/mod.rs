// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! P2p DST: synthetic cluster and recursive-routing workload. The
//! framework crate under `tests/framework/` stays project-agnostic;
//! this module wires the p2p public surface into the seeded
//! executor.

#![allow(clippy::arc_with_non_send_sync)]

pub mod mocks;
pub mod tests;
pub mod workload;
