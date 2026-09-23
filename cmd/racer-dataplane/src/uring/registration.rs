// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-wide inbound admission retains outbound registration headroom.
use super::*;
// One inbound budget per worker ring, shared by every HTTP listener/generation.
use std::cell::Cell;

// Always preserve outbound registration capacity, even when the SQ reserve is
// disabled. Tiny tables reserve a quarter rounded up; a one-file table cannot
// support inbound plus cold upstream and therefore admits no inbound work.
pub(super) fn inbound_reserve(files: u32) -> usize {
    files.div_ceil(4).min(8) as usize
}
pub(crate) struct Inbound(Rc<Cell<usize>>);
impl Drop for Inbound {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
impl Ring {
    pub(crate) fn admit_inbound(&self) -> Option<Rc<Inbound>> {
        let limit = (self.config.fixed_files as usize - inbound_reserve(self.config.fixed_files))
            .min(self.config.requests as usize / 2);
        if self.stopping || self.inbound.get() >= limit {
            return None;
        }
        self.inbound.set(self.inbound.get() + 1);
        Some(Rc::new(Inbound(self.inbound.clone())))
    }
    pub(crate) fn register_inbound(
        &mut self,
        file: File,
        admission: Rc<Inbound>,
    ) -> io::Result<FixedFile> {
        self.register_file_inner(file, Some(admission))
    }
}
