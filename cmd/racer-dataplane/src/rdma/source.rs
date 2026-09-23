// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! io_uring completion-source integration and CQ notification arming.
use super::*;
impl uring::CompletionSource for Source {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        for ticket in &mut self.polls {
            if let Some(t) = ticket
                && let Some(c) = ring.take_control(t)?
            {
                c.result?;
                *ticket = None;
            }
        }
        let mut owner = self.transport.owner.borrow_mut();
        if owner.core.is_none() {
            return Ok(uring::Work::default());
        }
        let core = owner.core()?;
        let channels = core.poll_channels(ring, budget)?;
        let mut work = core.progress(budget)?;
        work.runnable |= channels.runnable;
        work.deadline = work.deadline.into_iter().chain(channels.deadline).min();
        Ok(work)
    }
    fn arm(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        if owner.core.is_none() {
            return Ok(());
        }
        let core = owner.core()?;
        if core.stopped {
            return Err(error(io::ErrorKind::NotConnected, "RDMA source stopped"));
        }
        check(unsafe { ffi::racer_notify(core.device) })?;
        for i in 0..2 {
            if self.polls[i].is_none() {
                self.polls[i] = Some(ring.poll_fd(
                    self.files.as_ref().unwrap()[i].clone().into(),
                    uring::Readiness::Readable,
                )?);
            }
        }
        // uring::Driver rechecks CQ/application state after arming, before wait.
        Ok(())
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        for ticket in &mut self.polls {
            if let Some(t) = ticket.take() {
                drop(ring.cancel(&t)?);
            }
        }
        self.transport.shutdown()
    }
}
impl Drop for Source {
    fn drop(&mut self) {
        let result =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| self.transport.shutdown()));
        if let Err(panic) = result {
            std::mem::forget(panic);
        }
    }
}
