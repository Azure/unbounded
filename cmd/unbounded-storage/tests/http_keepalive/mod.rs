// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! HTTP keep-alive DST: drives a deterministic in-memory connection
//! model through the shared HTTP parser and connection lifetime policy.

pub mod tests;
pub mod workload;
