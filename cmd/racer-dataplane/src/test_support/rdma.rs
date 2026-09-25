//! Transfer-scoped fault events; real kernel/NIC fences need gated hardware tests.
use super::clock::{Clock, Schedule};
use crate::{
    error::{Error, Result},
    model::identity::TransferId,
};
use std::{
    cell::RefCell,
    collections::{HashMap, HashSet},
    time::Duration,
};

#[derive(Clone, Copy, Debug)]
pub enum RdmaEvent {
    LateWrite(TransferId),
    LocalComplete(TransferId),
    RemoteFence(TransferId),
    Revoke(TransferId),
}

struct Grant<L> {
    resource: L,
    region: u64,
    expires_at: Duration,
    revoked: bool,
    local_complete: bool,
    remote_fenced: bool,
    late_writes: usize,
}

/// Models quarantine and completion order without fabricating rkeys, addresses,
/// native handles, or authenticated ciphertext. `L` retains a real lease or probe.
/// Expiry/revocation stops new writes but queued writes can arrive until the remote
/// fence. Local completion alone never releases the destination for reuse.
pub struct FaultRdma<L = ()> {
    clock: Clock,
    grants: RefCell<HashMap<TransferId, Grant<L>>>,
    seen: RefCell<HashSet<TransferId>>,
    events: RefCell<Schedule<RdmaEvent>>,
}

impl<L> Default for FaultRdma<L> {
    fn default() -> Self {
        Self::new(Clock::default())
    }
}

impl<L> FaultRdma<L> {
    pub fn new(clock: Clock) -> Self {
        Self {
            clock: clock.clone(),
            grants: RefCell::new(HashMap::new()),
            seen: RefCell::new(HashSet::new()),
            events: RefCell::new(Schedule::new(clock)),
        }
    }

    /// A region cannot back a new attempt until the old one is fully fenced/taken.
    /// Failed admission returns the supplied resource to its caller.
    pub fn grant(
        &self,
        transfer: TransferId,
        region: u64,
        expires_at: Duration,
        resource: L,
    ) -> std::result::Result<(), (Error, L)> {
        if expires_at <= self.clock.elapsed() {
            return Err((Error::DeadlineExceeded, resource));
        }
        let mut grants = self.grants.borrow_mut();
        if self.seen.borrow().contains(&transfer) {
            return Err((Error::Replay, resource));
        }
        if grants.values().any(|grant| grant.region == region) {
            return Err((Error::Overloaded, resource));
        }
        self.seen.borrow_mut().insert(transfer);
        grants.insert(
            transfer,
            Grant {
                resource,
                region,
                expires_at,
                revoked: false,
                local_complete: false,
                remote_fenced: false,
                late_writes: 0,
            },
        );
        Ok(())
    }

    pub fn authorize_write(&self, transfer: TransferId) -> Result<()> {
        let grants = self.grants.borrow();
        let grant = grants.get(&transfer).ok_or(Error::InvalidRequest)?;
        if grant.revoked || grant.remote_fenced {
            return Err(Error::Unauthorized);
        }
        if self.clock.elapsed() >= grant.expires_at {
            return Err(Error::DeadlineExceeded);
        }
        Ok(())
    }

    /// Inject an already-issued write, even after expiry or a revoke request.
    pub fn late_write(&self, transfer: TransferId) -> Result<()> {
        let mut grants = self.grants.borrow_mut();
        let grant = grants.get_mut(&transfer).ok_or(Error::InvalidRequest)?;
        if grant.remote_fenced {
            return Err(Error::Unauthorized);
        }
        grant.late_writes = grant.late_writes.checked_add(1).ok_or(Error::Overloaded)?;
        Ok(())
    }

    pub fn late_writes(&self, transfer: TransferId) -> Result<usize> {
        Ok(self
            .grants
            .borrow()
            .get(&transfer)
            .ok_or(Error::InvalidRequest)?
            .late_writes)
    }

    pub fn revoke(&self, transfer: TransferId) -> Result<()> {
        self.grants
            .borrow_mut()
            .get_mut(&transfer)
            .ok_or(Error::InvalidRequest)?
            .revoked = true;
        Ok(())
    }

    pub fn local_complete(&self, transfer: TransferId) -> Result<()> {
        let mut grants = self.grants.borrow_mut();
        let grant = grants.get_mut(&transfer).ok_or(Error::InvalidRequest)?;
        if grant.local_complete {
            return Err(Error::InvalidRequest);
        }
        grant.local_complete = true;
        Ok(())
    }

    pub fn remote_fence(&self, transfer: TransferId) -> Result<()> {
        let mut grants = self.grants.borrow_mut();
        let grant = grants.get_mut(&transfer).ok_or(Error::InvalidRequest)?;
        if grant.remote_fenced || (!grant.revoked && self.clock.elapsed() < grant.expires_at) {
            return Err(Error::InvalidRequest);
        }
        grant.remote_fenced = true;
        Ok(())
    }

    pub fn take(&self, transfer: TransferId) -> Result<Option<L>> {
        let mut grants = self.grants.borrow_mut();
        let grant = grants.get(&transfer).ok_or(Error::InvalidRequest)?;
        if !grant.local_complete || !grant.remote_fenced {
            return Ok(None);
        }
        Ok(Some(grants.remove(&transfer).unwrap().resource))
    }

    pub fn retained(&self) -> usize {
        self.grants.borrow().len()
    }

    pub fn schedule(&self, at: Duration, event: RdmaEvent) -> Result<()> {
        self.events.borrow_mut().push(at, event)
    }

    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        let mut delivered = 0;
        while delivered < budget {
            let event = self.events.borrow_mut().pop_ready();
            let Some(event) = event else { break };
            match event {
                RdmaEvent::LateWrite(transfer) => self.late_write(transfer)?,
                RdmaEvent::LocalComplete(transfer) => self.local_complete(transfer)?,
                RdmaEvent::RemoteFence(transfer) => self.remote_fence(transfer)?,
                RdmaEvent::Revoke(transfer) => self.revoke(transfer)?,
            }
            delivered += 1;
        }
        Ok(delivered)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{cell::Cell, rc::Rc};

    #[derive(Debug)]
    struct Probe(Rc<Cell<usize>>);
    impl Drop for Probe {
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }

    #[test]
    fn fallback_cannot_reuse_quarantined_region_in_either_completion_order() {
        for remote_first in [false, true] {
            let rdma = FaultRdma::default();
            let drops = Rc::new(Cell::new(0));
            let old = TransferId([1; 16]);
            let new = TransferId([2; 16]);
            rdma.grant(old, 7, Duration::from_secs(10), Probe(drops.clone()))
                .unwrap();
            assert_eq!(rdma.authorize_write(old), Ok(()));
            assert_eq!(rdma.remote_fence(old), Err(Error::InvalidRequest));
            rdma.revoke(old).unwrap();
            assert_eq!(rdma.authorize_write(old), Err(Error::Unauthorized));
            rdma.late_write(old).unwrap();
            assert_eq!(rdma.late_writes(old), Ok(1));
            if remote_first {
                rdma.remote_fence(old).unwrap();
            } else {
                rdma.local_complete(old).unwrap();
            }
            assert!(rdma.take(old).unwrap().is_none());
            let (error, rejected) = rdma
                .grant(new, 7, Duration::from_secs(10), Probe(drops.clone()))
                .unwrap_err();
            assert_eq!(error, Error::Overloaded);
            assert_eq!(drops.get(), 0);
            if remote_first {
                rdma.local_complete(old).unwrap();
            } else {
                rdma.remote_fence(old).unwrap();
            }
            assert_eq!(rdma.late_write(old), Err(Error::Unauthorized));
            drop(rdma.take(old).unwrap().unwrap());
            assert_eq!(drops.get(), 1);
            rdma.grant(new, 7, Duration::from_secs(10), rejected)
                .unwrap();
            assert_eq!(rdma.late_write(old), Err(Error::InvalidRequest));
            assert_eq!(rdma.late_writes(new), Ok(0));
            assert_eq!(rdma.retained(), 1);
        }
    }

    #[test]
    fn expiry_is_inclusive_but_not_a_remote_fence_and_faults_are_ordered() {
        let clock = Clock::default();
        let rdma = FaultRdma::new(clock.clone());
        let transfer = TransferId([1; 16]);
        let expiry = Duration::from_secs(2);
        rdma.grant(transfer, 1, expiry, ()).unwrap();
        for event in [
            RdmaEvent::LateWrite(transfer),
            RdmaEvent::RemoteFence(transfer),
            RdmaEvent::LocalComplete(transfer),
        ] {
            rdma.schedule(expiry, event).unwrap();
        }
        assert_eq!(rdma.poll_budgeted(8), Ok(0));
        clock.advance(expiry).unwrap();
        assert_eq!(rdma.authorize_write(transfer), Err(Error::DeadlineExceeded));
        assert_eq!(rdma.take(transfer), Ok(None));
        assert_eq!(rdma.poll_budgeted(0), Ok(0));
        assert_eq!(rdma.poll_budgeted(1), Ok(1));
        assert_eq!(rdma.late_writes(transfer), Ok(1));
        assert_eq!(rdma.poll_budgeted(1), Ok(1));
        assert_eq!(rdma.take(transfer), Ok(None));
        assert_eq!(rdma.late_write(transfer), Err(Error::Unauthorized));
        assert_eq!(rdma.poll_budgeted(1), Ok(1));
        assert_eq!(rdma.local_complete(transfer), Err(Error::InvalidRequest));
        assert_eq!(rdma.take(transfer), Ok(Some(())));
        assert_eq!(
            rdma.grant(transfer, 1, expiry + expiry, ()),
            Err((Error::Replay, ()))
        );
        assert_eq!(
            rdma.grant(TransferId([2; 16]), 1, expiry, ()),
            Err((Error::DeadlineExceeded, ()))
        );
    }
}
