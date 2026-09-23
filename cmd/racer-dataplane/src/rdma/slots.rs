// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Slot admission and retirement preserve ticket and rkey generations.
use super::*;
impl Core {
    pub(super) fn allocate(&mut self, conn: usize, phase: Phase) -> io::Result<usize> {
        if self.free.is_empty() && self.slots.iter().any(|s| s.phase == Phase::Free) {
            self.renewing = true;
            return Err(error(
                io::ErrorKind::NotConnected,
                "RDMA retired capacity requires renewal; use HTTP",
            ));
        }
        let i = self.free.pop().ok_or_else(full)?;
        let generation =
            self.slots[i].begin(conn, phase, crate::environment::now() + self.config.timeout)?;
        self.book.slots[i].set((generation, false));
        Ok(i)
    }
    pub(super) fn ticket<T>(&mut self, i: usize) -> Ticket<T> {
        self.slots[i].tracked = true;
        Ticket {
            book: self.book.clone(),
            index: i,
            generation: self.slots[i].generation,
            active: true,
            _kind: PhantomData,
        }
    }
    pub(super) fn validate<T>(
        &self,
        conn: usize,
        serial: u64,
        ticket: &Ticket<T>,
    ) -> io::Result<usize> {
        self.connection(conn, serial)?;
        if !ticket.active || !Rc::ptr_eq(&self.book, &ticket.book) {
            return Err(invalid());
        }
        let s = &self.slots[ticket.index];
        if s.conn != conn || s.generation != ticket.generation || s.phase == Phase::Free {
            return Err(invalid());
        }
        Ok(ticket.index)
    }
    pub(super) fn release(&mut self, i: usize) {
        let s = &mut self.slots[i];
        debug_assert_eq!(s.wr, 0);
        debug_assert_eq!(s.send_id, 0);
        s.phase = Phase::Free;
        s.tracked = false;
        s.early = None;
        s.send_pending = false;
        self.book.slots[i].set((s.generation, false));
        // Never wrap an rkey on a live QP. The entire rail must quiesce before
        // the provider may recycle any window index (including on another QP).
        if s.uses == 255 {
            self.renewing = true;
        }
        // After successful QP destruction, a never-bound MW has no exported
        // capability to retire. Reuse its slot for reconnect without forcing
        // unrelated confirmed sessions through rail-wide MW renewal. Bound MWs
        // still require the existing all-QP quiescence/renewal discipline.
        if s.uses < 255
            && (!self.connections[s.conn].failed
                || (s.uses == 0 && self.connections[s.conn].qp.is_null()))
        {
            self.free.push(i);
        }
        s.buffer.take();
        s.fill.take();
    }
}
