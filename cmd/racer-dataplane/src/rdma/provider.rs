// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! QP setup calls borrow provider resources already retained by Owner.
use super::*;
impl Core {
    pub(super) fn create_qp(&mut self, psn: u32, endpoint: &mut ffi::Endpoint) -> *mut c_void {
        // SAFETY: device/CQ/PD are retained by Owner; caller installs the QP
        // in that owner before any fallible setup or DMA posting.
        unsafe {
            ffi::racer_qp(
                self.device,
                (self.config.depth * 4) as u32,
                &self.rail.raw,
                psn,
                endpoint,
            )
        }
    }
    pub(super) fn init_qp(&self, qp: *mut c_void) -> i32 {
        // SAFETY: QP belongs to this core and is newly created.
        unsafe { ffi::racer_init(qp, self.rail.raw.port) }
    }
    pub(super) fn connect_qp(
        &self,
        qp: *mut c_void,
        peer: &ffi::Endpoint,
        psn: u32,
        reads: u8,
    ) -> i32 {
        // SAFETY: caller validated the INIT QP and authenticated peer endpoint.
        unsafe { ffi::racer_connect(qp, &self.rail.raw, peer, psn, reads) }
    }
}
