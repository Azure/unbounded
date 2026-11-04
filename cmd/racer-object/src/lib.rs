// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! racer-object: an S3 and Azure Blob compatible, read-only sidecar that
//! serves objects from a Racer cache over its local client socket.

pub mod cli;
pub mod object;
pub mod racer;
