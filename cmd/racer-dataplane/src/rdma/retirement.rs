// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Provider destruction, failed-connection cleanup, and rail-wide MW renewal.
use super::*;
impl Core {
    pub(super) fn shutdown(&mut self) -> io::Result<()> {
        self.stopped = true;
        // Nonblocking, bounded event drain also services pending QP/CQ destroy.
        // Even an event-channel error must not prevent reaping completed jobs.
        let _ = self.events(32);
        if self.connections.iter().any(|c| !c.qp.is_null()) {
            let batch = self.retiring_qps.get_or_insert_with(|| {
                self.connections
                    .iter()
                    .map(|c| UnsafeCell::new(c.qp))
                    .collect()
            });
            // SAFETY: stopped admission; fail() cannot touch batch-owned QPs.
            // C adopts any earlier single-QP job; all owners survive pending
            // and partial errors. Only successful join releases the table.
            check(unsafe {
                ffi::racer_destroy_qps(self.device, batch.as_ptr().cast_mut().cast(), batch.len())
            })?;
            for c in &mut self.connections {
                c.qp = ptr::null_mut();
                c.cleanup_after = None;
            }
            self.retiring_qps = None;
        }
        for c in 0..self.connections.len() {
            self.fail(c, io::ErrorKind::ConnectionAborted)?;
        }
        if self.connections.iter().any(|c| !c.qp.is_null()) {
            return Err(full());
        }
        self.free_windows()?;
        // With all QPs gone no new completion-channel events can be generated.
        if !self.device.is_null() {
            check(unsafe { ffi::racer_close(self.device) })?;
            self.device = ptr::null_mut();
        }
        Ok(())
    }
    pub(super) fn fail(&mut self, conn: usize, reason: io::ErrorKind) -> io::Result<()> {
        let c = &mut self.connections[conn];
        let had_qp = !c.qp.is_null();
        c.failed = true;
        c.ready = false;
        if let Some(channel) = c.channel.as_mut() {
            channel.close();
        }
        if self.retiring_qps.is_some() {
            return Ok(()); // The batch owns destruction; even fatal events only ACK.
        }
        let now = crate::environment::now();
        if had_qp && c.cleanup_after.is_some_and(|d| now < d) {
            return Ok(());
        }
        if !c.qp.is_null() {
            // ERR alone and local invalidate are NOT quiescence proofs. Successful
            // provider QP destruction stops both outgoing DMA and incoming READs.
            // The C job owns ERR + destroy. EAGAIN includes both an outstanding
            // job and bounded helper admission pressure; neither is quiescence.
            let result = unsafe { ffi::racer_destroy_qp(self.device, c.qp) };
            c.cleanup_after = Some(now + Duration::from_millis(100));
            if result == libc::EAGAIN {
                c.cleanup_after = Some(now + Duration::from_millis(10));
                return Ok(());
            }
            check(result)?;
            c.qp = ptr::null_mut();
        }
        c.cleanup_after = None;
        // The MWs remain allocated and are retired (no rkey-index recycling).
        // No future remote access can use the destroyed QP's type-2B bindings.
        for i in 0..self.slots.len() {
            let s = &mut self.slots[i];
            if s.conn != conn || s.phase == Phase::Free {
                continue;
            }
            if s.phase == Phase::Failed {
                if self.book.slots[i].get().1 {
                    self.release(i);
                }
                continue;
            }
            s.fail_after_quiescence(reason);
            if !s.tracked || self.book.slots[i].get().1 {
                self.release(i);
            } else {
                self.slots[i].buffer.take();
                self.slots[i].fill.take();
            }
        }
        if had_qp && !self.stopped && self.connections.iter().all(|c| c.qp.is_null()) {
            self.renewing = true;
        }
        Ok(())
    }
    /// Destroy ALL QPs before freeing ANY MW, permitting provider key recycling.
    /// MR/CQ stay registered; monotonic WR/ticket/session generations fence old CQEs.
    pub(super) fn renew_windows(&mut self, now: Instant) {
        if !self.renewing || self.stopped || self.renew_after.is_some_and(|d| now < d) {
            return;
        }
        self.renew_after = Some(now + Duration::from_millis(100));
        for c in &mut self.connections {
            c.local_renewal |= !c.failed;
        }
        for c in 0..self.connections.len() {
            if self.fail(c, io::ErrorKind::ConnectionAborted).is_err() {
                return; // Keep QPs, windows, control and DMA owners; no new allocation.
            }
        }
        if self.stopped || self.connections.iter().any(|c| !c.qp.is_null()) {
            return;
        }
        // All DMA is quiescent, including forgotten tracked tickets. Retire them
        // without waiting for application polling, and rebuild admission once.
        self.free.clear();
        for s in &mut self.slots {
            s.phase = Phase::Free;
            s.tracked = false;
            s.wr = 0;
            s.send_id = 0;
            s.send_pending = false;
            s.buffer.take();
            s.fill.take();
        }
        // Finish all deallocations before allocating any replacement. On a
        // partial failure, null handles record progress; retries stay bounded.
        if self.free_windows().is_err() {
            return;
        }
        for i in 0..self.slots.len() {
            if self.allocate_window(i).is_err() {
                return;
            }
        }
        self.free.extend((0..self.slots.len()).rev());
        self.renewing = false;
        self.renew_after = None;
    }
    pub(super) fn free_windows(&mut self) -> io::Result<()> {
        if self.retiring_windows.is_none() && self.slots.iter().all(|s| s.mw.is_null()) {
            return Ok(());
        }
        let batch = self
            .retiring_windows
            .get_or_insert_with(|| self.slots.iter().map(|s| UnsafeCell::new(s.mw)).collect());
        // SAFETY: all QPs are gone. The fixed table and MW owners remain alive
        // and inaccessible to Rust until the helper has successfully joined.
        check(unsafe {
            ffi::racer_free_windows(self.device, batch.as_ptr().cast_mut().cast(), batch.len())
        })?;
        for s in &mut self.slots {
            s.mw = ptr::null_mut();
        }
        self.retiring_windows = None;
        Ok(())
    }
    pub(super) fn allocate_window(&mut self, i: usize) -> io::Result<()> {
        let s = &mut self.slots[i];
        debug_assert!(s.mw.is_null());
        s.mw = unsafe { ffi::racer_window(self.device, &mut s.key) };
        if s.mw.is_null() {
            return Err(io::Error::last_os_error());
        }
        s.uses = 0;
        Ok(())
    }
}
