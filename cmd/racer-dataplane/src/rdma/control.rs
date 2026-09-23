// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Authenticated control ingress and confirmation admission.
use super::*;
impl Core {
    pub(super) fn confirmation_send(&mut self, conn: usize, kind: u8) -> io::Result<()> {
        let i = self.allocate(conn, Phase::ConfirmSend)?;
        self.slots[i].frame = Frame {
            kind,
            session: self.connections[conn].local,
            ..Frame::default()
        };
        // Share the handshake deadline, including queue pressure. Never create
        // an unbounded or application-owned confirmation ticket.
        self.slots[i].deadline = self.connections[conn].deadline;
        if let Err(e) = self.encode(i, &[]).and_then(|_| self.send_control(i)) {
            self.release(i);
            self.fail(conn, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        Ok(())
    }
    pub(super) fn accept_reply(&mut self, i: usize, frame: Frame, ready: Phase) {
        self.slots[i].accept_reply(frame, ready);
    }
    pub(super) fn receive_bytes(&mut self, conn: usize, bytes: &[u8]) -> io::Result<()> {
        self.ready(conn, self.connections[conn].serial)?;
        if bytes.len() > CONTROL {
            return Err(protocol());
        }
        let frame = Frame::decode(bytes)?;
        self.received(conn, frame, &bytes[HEADER..])?;
        self.connections[conn].authenticated_received = true;
        Ok(())
    }
    pub(super) fn find_slot(
        &self,
        conn: usize,
        request: u64,
        phases: &[Phase],
    ) -> io::Result<usize> {
        self.slots
            .iter()
            .position(|s| s.conn == conn && s.frame.request == request && phases.contains(&s.phase))
            .ok_or_else(protocol)
    }
}
