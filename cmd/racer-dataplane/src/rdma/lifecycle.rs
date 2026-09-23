// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! The sole DMA owner. Unproven quiescence retains the complete Core.
use super::*;
impl Owner {
    pub(super) fn core(&mut self) -> io::Result<&mut Core> {
        self.core
            .as_deref_mut()
            .ok_or_else(|| error(io::ErrorKind::NotConnected, "RDMA source closed"))
    }
    pub(super) fn shutdown(&mut self) -> io::Result<()> {
        if let Some(core) = self.core.as_mut() {
            core.shutdown()?;
        }
        self.core.take();
        Ok(())
    }
}
impl Drop for Owner {
    fn drop(&mut self) {
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| self.shutdown()));
        if let Some(core) = self.core.take() {
            std::mem::forget(core);
        }
        if let Err(panic) = result {
            std::mem::forget(panic);
        }
    }
}
