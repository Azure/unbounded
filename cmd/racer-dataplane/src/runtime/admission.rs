//! Reserve bounded resources before acquisition, with protected completion capacity.

use crate::{
    error::{Result, pending},
    model::{
        identity::CacheId,
        limits::{Limits, ResourceClass},
    },
};

pub struct Admission {
    limits: Limits,
}

/// Non-cloneable quota ownership. Release only after all derived I/O leases end.
pub struct Reservation {
    class: ResourceClass,
    amount: usize,
}

pub struct FillReservation {
    pub plaintext: Reservation,
    pub ciphertext: Reservation,
    pub dirty: Option<Reservation>,
}

impl Admission {
    pub fn new(limits: Limits) -> Self {
        Self { limits }
    }
    pub fn reserve(
        &self,
        _cache: Option<&CacheId>,
        _class: ResourceClass,
        _amount: usize,
    ) -> Result<Reservation> {
        pending("admission.reserve")
    }
    /// Atomically reserve maximum-page progress needs in a consistent order.
    pub fn reserve_fill(&self, _cache: &CacheId, _persist: bool) -> Result<FillReservation> {
        pending("admission.reserve_fill")
    }
}

#[cfg(test)]
mod tests {
    // Cover per-cache fairness, overflow, completion reserves, and no hold-and-wait.
}
