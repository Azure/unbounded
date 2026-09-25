//! Transfer-scoped grants, expiry/revocation, and local/remote completion fences.
//!
//! Never expose the reusable pool-wide rkey. Unsupported scoped permissions fall
//! back to HTTP. A cancelled buffer stays quarantined until remote writes stop.
use super::{registered::RegisteredLease, session::SessionLease};
use crate::{
    error::{Operation, Result, deferred, pending},
    model::identity::TransferId,
    runtime::deadline::Deadline,
};
pub struct Permissions;
pub struct Grant {
    transfer: TransferId,
    buffer: RegisteredLease,
}
pub struct RemoteDescriptor {
    pub transfer: TransferId,
    pub address: u64,
    pub length: u64,
    pub scoped_key: u32,
}
impl Permissions {
    pub fn grant(
        &self,
        _session: &SessionLease,
        _buffer: RegisteredLease,
        _transfer: TransferId,
        _deadline: Deadline,
    ) -> Result<Grant> {
        pending("rdma.grant")
    }
    pub fn revoke_and_fence(&self, _grant: Grant) -> Operation<'_, RegisteredLease> {
        deferred("rdma.revoke_and_fence")
    }
}
#[cfg(test)]
mod tests { /* Late remote writes, expiry, key invalidation, no premature buffer reuse. */
}
