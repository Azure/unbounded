//! Worker-owned singleflight keyed exactly by page identity, with bounded waiters.
//!
//! Membership and origin authority belong to the acquisition, not its identity.
//! Waiter credentials remain request-scoped and are dropped on completion/cancel.
//! Successful fills can be shared regardless of origin headers. Credential-specific
//! origin failure is not cached as a page/version miss or evidence against other
//! callers; retry with a remaining caller's context under the original budgets.
use crate::{
    error::{Result, pending},
    memory::pool::VerifiedPage,
    model::identity::PageId,
    runtime::{
        admission::{Admission, FillReservation},
        deadline::RequestScope,
    },
    topology::membership::MembershipLease,
};
use std::rc::Rc;
pub struct Flights {
    admission: Rc<Admission>,
}
pub struct FlightLeader {
    page: PageId,
    membership: MembershipLease,
    reservation: FillReservation,
    generation: u64,
}
pub struct FlightWaiter {
    id: u64,
    generation: u64,
}
pub enum JoinedFlight {
    Leader(FlightLeader),
    Waiter(FlightWaiter),
    Complete(VerifiedPage),
}
impl Flights {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self { admission }
    }
    pub fn join(
        &self,
        _page: PageId,
        _membership: MembershipLease,
        _scope: &RequestScope,
    ) -> Result<JoinedFlight> {
        pending("flight.join")
    }
    pub fn publish(&self, _leader: FlightLeader, _page: VerifiedPage) -> Result<()> {
        pending("flight.publish")
    }
    pub fn detach(&self, _waiter: FlightWaiter) -> Result<()> {
        pending("flight.detach")
    }
}
#[cfg(test)]
mod tests { /* Concurrent joins, independent cancel, stale IDs, credential-failure retry. */
}
