// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Fan-out DST: cross-shard fetch-channel + owner `FetchService` pin
//! lifetime. The framework crate under `tests/framework/` stays
//! ignorant of everything here; this module plugs the fan-out path
//! into the generic executor + PRNG.

pub mod mocks;
pub mod routing;
pub mod tests;
pub mod workload;
