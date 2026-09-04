// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Bufferpool DST: project-specific mocks, workload model, and
//! invariant tests. The framework crate under `tests/framework/` is
//! deliberately ignorant of everything in here; this module is where
//! the bufferpool plugs into the generic executor + PRNG.

pub mod mocks;
pub mod tests;
pub mod workload;
