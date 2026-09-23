// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Bounded CPU-only control storage. These bytes are never registered for DMA.

use super::{CONTROL, Frame, HEADER, MAX_METADATA, invalid};
use std::io;

pub(super) struct ControlArena(Box<[[u8; CONTROL]]>);

impl ControlArena {
    pub(super) fn clear(&mut self, i: usize) {
        self.0[i].fill(0);
    }
    pub(super) fn new(count: usize) -> Self {
        Self((0..count).map(|_| [0; CONTROL]).collect())
    }

    pub(super) fn bytes(&self, i: usize) -> &[u8; CONTROL] {
        &self.0[i]
    }

    /// Encode only after the caller has retired the preceding channel send.
    pub(super) fn encode(
        &mut self,
        i: usize,
        frame: &mut Frame,
        metadata: &[u8],
    ) -> io::Result<usize> {
        if metadata.len() > MAX_METADATA {
            return Err(invalid());
        }
        frame.metadata = metadata.len() as u16;
        let len = HEADER + metadata.len();
        self.0[i].fill(0);
        let bytes = &mut self.0[i][..len];
        frame.encode(bytes);
        bytes[HEADER..].copy_from_slice(metadata);
        Ok(len)
    }
}
