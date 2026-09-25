//! Typed publication schema boundary. Canonical wire encoding remains versioned.
use super::caches::CacheDefinition;
use crate::{model::identity::MembershipVersion, topology::membership::Member};
pub struct Publication {
    pub schema_version: u32,
    pub sequence: u64,
    pub membership_version: MembershipVersion,
    pub members: Vec<Member>,
    pub caches: Vec<CacheDefinition>,
}
pub struct EnrollmentResponse {
    pub certificate_chain: Vec<Vec<u8>>,
}
#[cfg(test)]
mod tests { /* Unknown versions, excessive bounds, and out-of-order publications. */
}
