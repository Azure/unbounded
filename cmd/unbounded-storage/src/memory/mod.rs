// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Memory and backing concerns: the pinned NUMA-local `Backing`, its
//! allocator, and the NUMA memory-binding syscalls they use.

mod backing;
mod numa;

pub use backing::{Backing, BackingError, BackingKind, BackingRequest, HUGEPAGE_2MB, allocate};
