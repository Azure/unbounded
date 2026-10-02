//! Racer resource policy and compound admission operations.
use crate::{
    error::{Error, Result},
    model::{CacheId, Limits, PAGE_BYTES, ResourceClass},
    telemetry::failures::{Detail, Failure, Observer, Stage},
};
use flow_control::{Charge, Policy, Quotas, Rejection, SharedQuotas};
use std::sync::Mutex;

pub struct AdmissionPolicy {
    limits: Limits,
    observer: Mutex<Observer>,
}
impl Clone for AdmissionPolicy {
    fn clone(&self) -> Self {
        Self {
            limits: self.limits.clone(),
            observer: Mutex::new(self.observer()),
        }
    }
}
impl AdmissionPolicy {
    pub fn new(limits: Limits) -> Self {
        Self {
            limits,
            observer: Mutex::default(),
        }
    }
    fn observer(&self) -> Observer {
        self.observer
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }
}
impl Policy for AdmissionPolicy {
    type Class = ResourceClass;
    type Key = CacheId;
    fn limit(&self, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => self.limits.plaintext_bytes.get(),
            ResourceClass::Ciphertext => self.limits.ciphertext_bytes.get(),
            ResourceClass::DirtyCiphertext => self.limits.dirty_bytes.get(),
            ResourceClass::Registered => self.limits.registered_bytes.get(),
            ResourceClass::RequestContext => self.limits.request_context_bytes.get(),
            ResourceClass::Flight => self.limits.flights.get(),
            ResourceClass::Waiter => self
                .limits
                .flights
                .get()
                .saturating_mul(self.limits.waiters_per_flight.get()),
            ResourceClass::Connection => self.limits.client_connections.get(),
            // Snapshot, renewal, and keyring delivery make independent progress.
            ResourceClass::ControlConnection => (self.limits.client_connections.get() / 4).min(3),
            ResourceClass::OutboundConnection => self.limits.client_connections.get() / 4,
            ResourceClass::IngressConnection => {
                self.limits.client_connections.get().saturating_sub(
                    self.limit(ResourceClass::ControlConnection)
                        + self.limit(ResourceClass::OutboundConnection),
                )
            }
            ResourceClass::Pipe => self.limits.pipes.get(),
            ResourceClass::ControlProgress => self.limits.queue_entries.get(),
            ResourceClass::Relay => self.limits.relay_transfers.get(),
        }
    }
    fn floor(&self, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => PAGE_BYTES as usize,
            // Disk reads own padded staging and decoded ciphertext together.
            ResourceClass::Ciphertext => {
                2 * (PAGE_BYTES as usize + 16) + crate::store::format::MAX_HEADER_BYTES + 4096
            }
            ResourceClass::DirtyCiphertext | ResourceClass::Registered => PAGE_BYTES as usize + 16,
            _ => 1,
        }
    }
    fn max_keys(&self) -> usize {
        self.limits.metadata_entries.get()
    }
    fn wakes(class: ResourceClass) -> bool {
        matches!(
            class,
            ResourceClass::Connection | ResourceClass::IngressConnection
        )
    }
    fn allows_stopped(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::ControlProgress)
    }
    fn covers(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::Ciphertext)
    }
    fn rejected(&self, rejection: Rejection<ResourceClass>) {
        let detail = match rejection {
            Rejection::Keys { used, limit } => Detail::CacheEntries { used, limit },
            Rejection::Resource {
                class,
                used,
                limit,
                requested,
                key_used,
                key_limit,
            } => Detail::Resource {
                class,
                used,
                limit,
                requested,
                cache_used: key_used,
                cache_limit: key_limit,
            },
        };
        self.observer()
            .record(Failure::new(Stage::Admission, Error::Overloaded).detail(detail));
    }
}

pub struct FillReservation {
    pub plaintext: Charge<AdmissionPolicy>,
    pub ciphertext: Charge<AdmissionPolicy>,
    pub dirty: Option<Charge<AdmissionPolicy>>,
}
/// Compound socket role admission, retained through kernel completion.
pub struct ConnectionReservation {
    _total: Charge<AdmissionPolicy>,
    _role: Charge<AdmissionPolicy>,
}

/// Racer operations on the generic local authority. No quota facade or alias.
pub trait AdmissionExt {
    fn limits(&self) -> &Limits;
    fn observer(&self) -> Observer;
    fn set_observer(&self, observer: Observer);
    fn usage(&self) -> SharedQuotas<AdmissionPolicy>;
    fn connection_admission(&self) -> SharedQuotas<AdmissionPolicy>;
    fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation>;
    fn reserve_fill(&self, cache: &CacheId, persist: bool) -> Result<FillReservation>;
}
impl AdmissionExt for Quotas<AdmissionPolicy> {
    fn limits(&self) -> &Limits {
        &self.policy().limits
    }
    fn observer(&self) -> Observer {
        self.policy().observer()
    }
    fn set_observer(&self, observer: Observer) {
        *self
            .policy()
            .observer
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = observer;
    }
    fn usage(&self) -> SharedQuotas<AdmissionPolicy> {
        self.shared()
    }
    fn connection_admission(&self) -> SharedQuotas<AdmissionPolicy> {
        self.shared()
    }
    fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation> {
        if matches!(role, ResourceClass::IngressConnection) {
            return self.connection_admission().reserve_ingress();
        }
        if !matches!(
            role,
            ResourceClass::OutboundConnection | ResourceClass::ControlConnection
        ) {
            return Err(Error::InvalidConfiguration);
        }
        let role_charge = self.reserve(None, role, 1)?;
        let total = self.reserve(None, ResourceClass::Connection, 1)?;
        Ok(ConnectionReservation {
            _total: total,
            _role: role_charge,
        })
    }
    fn reserve_fill(&self, cache: &CacheId, persist: bool) -> Result<FillReservation> {
        let plaintext = self.reserve(Some(cache), ResourceClass::Plaintext, PAGE_BYTES as usize)?;
        let ciphertext = self.reserve(
            Some(cache),
            ResourceClass::Ciphertext,
            PAGE_BYTES as usize + 16,
        )?;
        let dirty = if persist {
            Some(self.reserve(
                Some(cache),
                ResourceClass::DirtyCiphertext,
                PAGE_BYTES as usize + 16,
            )?)
        } else {
            None
        };
        Ok(FillReservation {
            plaintext,
            ciphertext,
            dirty,
        })
    }
}

/// Compound ingress admission and Racer-specific usage views on the direct
/// generic shared handle. No cache authority or payload pool crosses workers.
pub trait SharedAdmissionExt {
    fn reserve_ingress(&self) -> Result<ConnectionReservation>;
    fn relay(&self) -> (usize, usize);
    fn ciphertext(&self) -> (usize, usize);
}
impl SharedAdmissionExt for SharedQuotas<AdmissionPolicy> {
    fn reserve_ingress(&self) -> Result<ConnectionReservation> {
        let role = self.reserve(ResourceClass::IngressConnection, 1)?;
        let total = self.reserve(ResourceClass::Connection, 1)?;
        Ok(ConnectionReservation {
            _total: total,
            _role: role,
        })
    }
    fn relay(&self) -> (usize, usize) {
        (
            self.used(ResourceClass::Relay),
            self.limit(ResourceClass::Relay),
        )
    }
    fn ciphertext(&self) -> (usize, usize) {
        (
            self.used(ResourceClass::Ciphertext),
            self.limit(ResourceClass::Ciphertext),
        )
    }
}

#[cfg(test)]
mod tests;
