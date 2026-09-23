// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Request transitions. A cancellation acknowledgment never completes its target.

use super::*;

impl Request {
    // Returns true only when this CQE proves the kernel has released storage.
    pub(super) fn complete(&mut self, res: i32, flags: u32) -> io::Result<bool> {
        let res = match (&self.state, flags & (abi::MORE | abi::NOTIF)) {
            (State::InFlight, 0) => res,
            (State::InFlight, abi::MORE) if self.opcode == abi::SEND_ZC => {
                self.state = State::Notification(res);
                return Ok(false);
            }
            (State::Notification(res), abi::NOTIF) if self.opcode == abi::SEND_ZC => *res,
            _ => return Err(io::Error::other("unexpected CQE lifecycle flags")),
        };
        if self.opcode == abi::ACCEPT
            && res >= 0
            && !matches!(self.resource, Resource::Accepted(Some(_)))
        {
            // SAFETY: caller supplies a single-shot accept CQE with a fresh fd.
            self.resource =
                Resource::Accepted(Some(File::new(unsafe { OwnedFd::from_raw_fd(res) })));
        }
        if let Some(charge) = self.slab_charge.take() {
            charge.finish(res.max(0) as usize);
        }
        self.state = State::Complete(res);
        Ok(true)
    }

    /// Cancel only a token-queued request that has never reached the kernel.
    /// Storage remains in the request until collection or abandonment cleanup.
    pub(super) fn cancel_queued(&mut self) -> bool {
        let Some(pending) = self.slab_pending.take() else {
            return false;
        };
        if pending.waited {
            pending.io.waited(pending.since);
        }
        self.state = State::Complete(-libc::ECANCELED);
        true
    }

    /// Transfer a token-queued SQE to the kernel owner after admission succeeds.
    pub(super) fn submit_admitted(&mut self, raw: &mut KernelRing, charge: crate::slab_io::Charge) {
        let pending = self.slab_pending.as_ref().expect("queued slab request");
        if pending.waited {
            pending.io.waited(pending.since);
        }
        raw.push(pending.sqe);
        self.slab_charge = Some(charge);
        self.slab_pending = None;
    }
}
