// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Affine negotiation capabilities and their cancellation-on-drop contract.
use super::*;
impl Connected {
    /// Install the exact authenticated TLS channel once.
    /// Errors retire the QP. Responders activate before sending Ready; initiators
    /// verify Ready before activation.
    pub fn authenticate_channel(mut self, channel: ControlChannel) -> io::Result<Connection> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let c = core.connection(self.index, self.serial)?;
        if core.stopped
            || !c.ready
            || c.failed
            || c.qp.is_null()
            || c.cancelled.get()
            || c.channel.is_some()
            || c.next_request != 0
            || c.last_request != 0
        {
            return Err(invalid());
        }
        channel.matches_transport(c.binding.as_ref().ok_or_else(invalid)?)?;
        if !channel.healthy() || !channel.admitting() {
            return Err(error(io::ErrorKind::PermissionDenied, "stale TLS channel"));
        }
        core.connections[self.index].channel = Some(channel);
        self.armed = false;
        Ok(Connection {
            transport: self.transport.clone(),
            index: self.index,
            serial: self.serial,
            cancelled: self.cancelled.clone(),
        })
    }
    /// Cancel pending activation through a shared runtime owner. A borrow
    /// conflict records cancellation for progress and returns `WouldBlock`.
    pub fn cancel(&self) -> io::Result<()> {
        self.transport
            .disconnect(self.index, self.serial, &self.cancelled)
    }
    pub fn close(self) -> io::Result<()> {
        self.cancel()
    }
}

impl Connection {
    /// Reserve one bounded control slot after Ready, independent of application
    /// requests. Both controls use request zero on the authenticated channel.
    pub(crate) fn begin_confirmation(&self, initiator: bool, deadline: Instant) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        core.ready(self.index, self.serial)?;
        let c = &mut core.connections[self.index];
        if c.channel.is_none() || c.confirmation != Confirmation::None {
            return Err(protocol());
        }
        c.deadline = c.deadline.min(deadline);
        if crate::environment::now() >= c.deadline {
            return Err(error(io::ErrorKind::TimedOut, "RDMA confirmation expired"));
        }
        c.confirmation = if initiator {
            Confirmation::AwaitAck
        } else {
            Confirmation::AwaitConfirm
        };
        if initiator {
            core.confirmation_send(self.index, 5)?;
        }
        Ok(())
    }
    /// ACK receipt can precede local write retirement. Admission waits for both so
    /// the confirmation arena is retired before application capacity is used.
    pub(crate) fn is_confirmed(&self) -> bool {
        self.inspect(|core, c| {
            c.confirmation == Confirmation::Complete
                && core.ready(self.index, self.serial).is_ok()
                && !core
                    .slots
                    .iter()
                    .any(|s| s.conn == self.index && s.phase == Phase::ConfirmSend)
        })
    }
    fn inspect(&self, f: impl FnOnce(&Core, &ConnectionState) -> bool) -> bool {
        self.transport.owner.try_borrow().is_ok_and(|owner| {
            owner.core.as_ref().is_some_and(|core| {
                core.connection(self.index, self.serial)
                    .is_ok_and(|session| f(core, session))
            })
        })
    }
    /// Local maintenance is recovery-only, not peer failure evidence.
    pub(crate) fn needs_http_recovery(&self) -> bool {
        if self.key_draining() {
            return true;
        }
        self.transport
            .owner
            .borrow()
            .core
            .as_ref()
            .is_some_and(|core| {
                core.connection(self.index, self.serial)
                    .map_or(true, |c| c.local_renewal || (core.renewing && !c.failed))
            })
    }
    pub fn is_authenticated(&self) -> bool {
        self.inspect(|_, s| s.channel.as_ref().is_some_and(ControlChannel::healthy))
    }
    /// True only after a valid authenticated RDMA control was received. HTTP
    /// Ready does not count. This includes transport confirmation controls.
    pub fn authenticated_received(&self) -> bool {
        self.inspect(|_, s| s.authenticated_received)
    }
    /// Local transport health, not a peer liveness probe. False after deferred
    /// cancellation, source shutdown, or failure (also during reentrant polling).
    pub fn is_healthy(&self) -> bool {
        !self.cancelled.get()
            && self.inspect(|core, s| {
                core.ready(self.index, self.serial).is_ok()
                    && s.channel.as_ref().is_some_and(ControlChannel::healthy)
            })
    }
    pub(crate) fn key_draining(&self) -> bool {
        self.inspect(|_, s| {
            s.channel
                .as_ref()
                .is_some_and(|channel| !channel.admitting())
        })
    }
    /// A replacement may retire this channel only after all original RPCs and
    /// their terminal controls have relinquished transport ownership.
    pub(crate) fn is_drained(&self) -> bool {
        self.key_draining()
            && self.inspect(|core, _| {
                !core
                    .slots
                    .iter()
                    .any(|s| s.conn == self.index && s.phase != Phase::Free)
            })
    }
    /// Idempotently stop admission/destroy QP; errors retain DMA and cancellation.
    pub fn disconnect(&self) -> io::Result<()> {
        self.transport
            .disconnect(self.index, self.serial, &self.cancelled)
    }
}
impl Connecting {
    pub fn offer(&self) -> &Offer {
        &self.offer
    }
    /// Cancel through a shared reference, including an `Rc` runtime owner.
    /// A `WouldBlock` result still records cancellation for driver progress.
    pub fn cancel(&self) -> io::Result<()> {
        self.transport
            .disconnect(self.index, self.serial, &self.cancelled)
    }
    pub fn close(self) -> io::Result<()> {
        self.cancel()
    }
    pub fn connect(mut self, peer: AuthenticatedOffer, shard: u64) -> io::Result<Connected> {
        let (peer, binding) = peer.into_parts();
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let c = core.connection(self.index, self.serial)?;
        if core.stopped
            || c.failed
            || c.cancelled.get()
            || c.ready
            || c.qp.is_null()
            || crate::environment::now() >= c.deadline
        {
            return Err(error(
                io::ErrorKind::NotConnected,
                "RDMA handshake expired or closed",
            ));
        }
        if peer.fabric != self.offer.fabric
            || peer.challenge != self.offer.challenge
            || peer.nonce == self.offer.nonce
            || peer.endpoint.ethernet != self.offer.endpoint.ethernet
            || rails_for_shard(shard, self.offer.rails as usize, peer.rails as usize)
                != Some((self.offer.rail as usize, peer.rail as usize))
        {
            return Err(invalid());
        }
        let qp = c.qp;
        // SAFETY: QP is owned and INIT; endpoint was structurally parsed and authenticated.
        let result = core.connect_qp(
            qp,
            &peer.endpoint,
            self.offer.endpoint.psn,
            peer.reads.min(self.offer.reads),
        );
        if result != 0 {
            core.fail(self.index, io::ErrorKind::ConnectionAborted)?;
            return Err(io::Error::from_raw_os_error(result));
        }
        let c = &mut core.connections[self.index];
        c.peer = peer.nonce;
        c.binding = Some(binding);
        c.ready = true;
        self.armed = false;
        Ok(Connected {
            transport: self.transport.clone(),
            index: self.index,
            serial: self.serial,
            cancelled: self.cancelled.clone(),
            armed: true,
        })
    }
    /// Activate only the offer from this channel and install control protection
    /// before returning a connection usable by the application.
    pub fn connect_authenticated(
        self,
        offer: AuthenticatedOffer,
        channel: ControlChannel,
        shard: u64,
    ) -> io::Result<Connection> {
        self.connect(offer, shard)?.authenticate_channel(channel)
    }
}
impl Drop for Connecting {
    fn drop(&mut self) {
        if self.armed {
            self.transport
                .drop_connection(self.index, self.serial, &self.cancelled);
        }
    }
}
impl Drop for Connection {
    fn drop(&mut self) {
        self.transport
            .drop_connection(self.index, self.serial, &self.cancelled);
    }
}
impl Drop for Connected {
    fn drop(&mut self) {
        if self.armed {
            self.transport
                .drop_connection(self.index, self.serial, &self.cancelled);
        }
    }
}
