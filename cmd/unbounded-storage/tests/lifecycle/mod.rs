// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Config, disk hot-swap, and shard-lifecycle DST.
//!
//! The production `ShardControlGroup` uses blocking std channels, so this
//! harness models the same apply/ack contract with deterministic, nonblocking
//! mailboxes while exercising the real config diffing, disk registry,
//! disk-channel directory, page-channel service, storage engine, and shard loop
//! primitives.

#![allow(clippy::arc_with_non_send_sync)]

pub mod tests;
pub mod workload;
