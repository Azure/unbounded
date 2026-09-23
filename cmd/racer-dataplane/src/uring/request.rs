// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Typed request observation, cancellation, and terminal resource collection.
use super::*;
impl Ring {
    pub fn cancel<O: Operation>(&mut self, ticket: &Ticket<O>) -> io::Result<Ticket<Cancel>> {
        if self.request(ticket)?.slab_pending.is_some() {
            // A NOP supplies the ordinary cancellation acknowledgment. The target
            // never reached the kernel and retains resources until collection.
            let ack = self
                .enqueue(abi::Sqe::default(), Resource::None, None)
                .map_err(|(e, _)| e)?;
            self.core.as_mut().unwrap().slots[ticket.id as u32 as usize]
                .request
                .as_mut()
                .unwrap()
                .cancel_queued();
            self.slab_queue.retain(|id| *id != ticket.id);
            return Ok(ack);
        }
        self.enqueue(
            abi::Sqe {
                opcode: abi::CANCEL,
                addr: ticket.id,
                ..Default::default()
            },
            Resource::None,
            None,
        )
        .map_err(|(e, _)| e)
    }
    pub(super) fn request<O: Operation>(&self, ticket: &Ticket<O>) -> io::Result<&Request> {
        if !Rc::ptr_eq(&ticket.book, &self.book) || ticket.collected {
            return Err(invalid("foreign or collected ticket"));
        }
        let slot = self
            .core
            .as_ref()
            .and_then(|c| c.slots.get(ticket.id as u32 as usize))
            .ok_or_else(|| invalid("stale ticket"))?;
        if slot.generation != (ticket.id >> 32) as u32 {
            return Err(invalid("stale ticket generation"));
        }
        slot.request
            .as_ref()
            .ok_or_else(|| invalid("released ticket"))
    }
    /// Early result observation never transfers buffer ownership.
    pub fn send_result(&self, ticket: &Ticket<SendZc>) -> io::Result<Option<io::Result<usize>>> {
        Ok(match self.request(ticket)?.state {
            State::InFlight => None,
            State::Notification(r) | State::Complete(r) => Some(result(r)),
        })
    }
    pub(super) fn take<O: Operation>(
        &mut self,
        ticket: &mut Ticket<O>,
    ) -> io::Result<Option<(Resource, i32)>> {
        let State::Complete(res) = self.request(ticket)?.state else {
            return Ok(None);
        };
        ticket.collected = true;
        let request = self
            .core
            .as_mut()
            .unwrap()
            .release(ticket.id as u32 as usize);
        Ok(Some((request.resource, res)))
    }
}
