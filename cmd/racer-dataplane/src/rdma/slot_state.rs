// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Ticket generations and ordered request/reply transitions.
use super::*;
impl Slot {
    pub(super) fn new(now: Instant) -> Self {
        Self {
            send_pending: false,
            send_id: 0,
            control_tag: 0,
            descriptor: [0; 32],
            negative: None,
            checksum: None,
            wire_len: 0,
            phase: Phase::Free,
            conn: 0,
            generation: 0,
            wr: 0,
            opcode: 0,
            deadline: now,
            frame: Frame::default(),
            fill: None,
            buffer: None,
            mw: ptr::null_mut(),
            key: 0,
            uses: 0,
            failure: io::ErrorKind::ConnectionAborted,
            early: None,
            tracked: false,
        }
    }
    /// Start a new affine ticket generation without recycling its memory window.
    pub(super) fn begin(
        &mut self,
        conn: usize,
        phase: Phase,
        deadline: Instant,
    ) -> io::Result<u64> {
        let generation = self.generation.checked_add(1).ok_or_else(full)?;
        self.generation = generation;
        self.conn = conn;
        self.phase = phase;
        self.wr = 0;
        self.opcode = 0;
        self.frame = Frame::default();
        self.checksum = None;
        self.negative = None;
        self.descriptor = [0; 32];
        self.wire_len = 0;
        self.send_pending = false;
        self.send_id = 0;
        self.control_tag = 0;
        self.early = None;
        self.tracked = false;
        self.deadline = deadline;
        Ok(generation)
    }
    pub(super) fn accept_reply(&mut self, frame: Frame, ready: Phase) {
        if self.phase == Phase::RequestSend {
            self.early = Some(frame);
        } else {
            self.frame = frame;
            self.phase = ready;
        }
    }
    /// A reply cannot replace the request frame while TLS still owns its send.
    pub(super) fn request_sent(&mut self) {
        if let Some(frame) = self.early.take() {
            self.frame = frame;
            self.phase = if matches!(frame.kind, 4 | 7) {
                Phase::FailureReady
            } else {
                Phase::GrantReady
            };
        } else {
            self.phase = Phase::AwaitGrant;
        }
    }
    /// Only the connection teardown owner may call this after QP destruction.
    /// An error CQE or a pending provider helper is insufficient evidence.
    pub(super) fn fail_after_quiescence(&mut self, reason: io::ErrorKind) {
        self.wr = 0;
        self.send_id = 0;
        self.send_pending = false;
        self.failure = reason;
        self.phase = Phase::Failed;
    }
}
