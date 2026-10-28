//! Simulator admission over the real `flow_control::Quotas` and the
//! production admission rules.
//!
//! `limit`, `floor`, `max_keys`, `wakes`, `allows_stopped`, and `covers`
//! delegate to `racer_core::admission::Rules`, the same rules the dataplane's
//! `AdmissionPolicy` (`src/admission.rs`) delegates to, over production
//! `ResourceClass`. Limits start from production per-worker defaults
//! (`Limits::default()`) with the read-path dimensions taken from
//! `NodeConfig`. Floors use the simulator's 1 MiB page (production 16 MiB).
//! Only `rejected` differs: it counts per class in a counter shared by every
//! node instead of recording telemetry. Cache ids are interned to `u32` (one
//! simulated cache), so a charge never clones a `CacheId` string.

use std::cell::RefCell;
use std::collections::HashMap;
use std::num::NonZeroUsize;
use std::rc::Rc;

use flow_control::{Charge, Policy, Quotas, Rejection};
use racer_core::read::Error;
use racer_core::read::flight::FlightAdmission;
use racer_object_wire::model::PageId;

use super::config::NodeConfig;
use super::ids::{CACHE, Ids};
use crate::engine::Fixed;

pub use racer_core::admission::ResourceClass;
use racer_core::admission::{Floors, Limits, Rules};

/// Rejection counts per class, plus key-table rejections at the end.
pub type Rejections = Rc<RefCell<[u64; ResourceClass::ALL.len() + 1]>>;

pub struct SimPolicy {
    rules: Rules,
    rejections: Rejections,
}

/// Simulated page size; production pages are 16 MiB (`PAGE_BYTES`).
pub const PAGE_BYTES: usize = 1 << 20;

/// Production `store::MAX_HEADER_BYTES` (`src/store.rs`), the per-record
/// overhead in the ciphertext floor.
pub const MAX_HEADER_BYTES: usize = 16384 + 4096;

fn nonzero(value: u64) -> NonZeroUsize {
    NonZeroUsize::new(usize::try_from(value).unwrap_or(usize::MAX)).unwrap_or(NonZeroUsize::MIN)
}

/// Production per-worker limits with the read-path dimensions the
/// simulator configures. One simulated node is one production I/O worker.
pub fn limits(config: &NodeConfig) -> Limits {
    Limits {
        plaintext_bytes: nonzero(config.plaintext_bytes),
        ciphertext_bytes: nonzero(config.ciphertext_bytes),
        dirty_bytes: nonzero(config.dirty_bytes),
        flights: nonzero(config.flights.into()),
        waiters_per_flight: nonzero(config.waiters_per_flight.into()),
        relay_transfers: nonzero(config.relay_transfers.into()),
        cached_paths: nonzero(config.path_entries.into()),
        path_cache_bytes: nonzero(config.path_bytes),
        active_path_searches: nonzero(config.active_searches.into()),
        retained_snapshots: nonzero(config.retained_versions),
        ..Limits::default()
    }
}

impl SimPolicy {
    pub fn new(config: &NodeConfig, rejections: Rejections) -> Self {
        Self {
            rules: Rules::new(
                limits(config),
                Floors::for_page(PAGE_BYTES, MAX_HEADER_BYTES),
            ),
            rejections,
        }
    }
}

impl Policy for SimPolicy {
    type Class = ResourceClass;
    type Key = u32;

    fn limit(&self, class: ResourceClass) -> usize {
        self.rules.limit(class)
    }

    fn floor(&self, class: ResourceClass) -> usize {
        self.rules.floor(class)
    }

    fn progress(&self, class: ResourceClass) -> usize {
        self.rules.progress(class)
    }

    fn weighted(class: ResourceClass) -> bool {
        Rules::weighted(class)
    }

    fn max_keys(&self) -> usize {
        self.rules.max_keys()
    }

    fn wakes(class: ResourceClass) -> bool {
        Rules::wakes(class)
    }

    fn allows_stopped(class: ResourceClass) -> bool {
        Rules::allows_stopped(class)
    }

    fn covers(class: ResourceClass) -> bool {
        Rules::covers(class)
    }

    fn rejected(&self, rejection: Rejection<ResourceClass>) {
        let index = match rejection {
            Rejection::Resource { class, .. } => class as usize,
            Rejection::Keys { .. } => ResourceClass::ALL.len(),
        };
        self.rejections.borrow_mut()[index] += 1;
    }
}

pub type SimQuotas = Quotas<SimPolicy>;
pub type SimCharge = Charge<SimPolicy>;

/// Map a flow-control failure onto the read-path error.
pub fn read_error(error: flow_control::Error) -> Error {
    match error {
        flow_control::Error::Overloaded => Error::Overloaded,
        flow_control::Error::InvalidInput => Error::InvalidRequest,
        flow_control::Error::Unavailable | flow_control::Error::Io => Error::Unavailable,
    }
}

/// Reserve `amount` of `class` for the simulated cache.
pub fn reserve(
    quotas: &SimQuotas,
    class: ResourceClass,
    amount: usize,
) -> Result<SimCharge, Error> {
    quotas
        .reserve(Some(&CACHE), class, amount)
        .map_err(read_error)
}

/// Flight and waiter charges held per page, as production `Flights` holds
/// them with each waiter and flight.
#[derive(Default)]
pub struct PageCharges {
    flight: Option<SimCharge>,
    waiters: Vec<SimCharge>,
}

/// `FlightAdmission` over a worker's quotas.
pub struct Admit<'a> {
    pub quotas: &'a SimQuotas,
    pub charges: &'a mut HashMap<u64, PageCharges, Fixed>,
    pub ids: &'a Ids,
}

impl Admit<'_> {
    fn entry(&mut self, page: &PageId) -> &mut PageCharges {
        self.charges.entry(self.ids.key(page)).or_default()
    }

    fn tidy(&mut self, page: &PageId) {
        let key = self.ids.key(page);
        if self
            .charges
            .get(&key)
            .is_some_and(|c| c.flight.is_none() && c.waiters.is_empty())
        {
            self.charges.remove(&key);
        }
    }
}

impl FlightAdmission for Admit<'_> {
    fn waiter(&mut self, page: &PageId) -> Result<(), Error> {
        let charge = reserve(self.quotas, ResourceClass::Waiter, 1);
        match charge {
            Ok(charge) => {
                self.entry(page).waiters.push(charge);
                Ok(())
            }
            Err(error) => {
                self.tidy(page);
                Err(error)
            }
        }
    }

    fn flight(&mut self, page: &PageId) -> Result<(), Error> {
        let charge = reserve(self.quotas, ResourceClass::Flight, 1)?;
        self.entry(page).flight = Some(charge);
        Ok(())
    }

    fn release_waiter(&mut self, page: &PageId) {
        self.entry(page).waiters.pop();
        self.tidy(page);
    }

    fn release_flight(&mut self, page: &PageId) {
        self.entry(page).flight = None;
        self.tidy(page);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn limits_rejections_and_flight_charges() {
        let rejections = Rejections::default();
        let config = NodeConfig {
            flights: 1,
            waiters_per_flight: 2,
            ..NodeConfig::default()
        };
        let quotas = SimQuotas::new(SimPolicy::new(&config, rejections.clone()));
        assert_eq!(quotas.limit(ResourceClass::Waiter), 2);
        assert_eq!(quotas.limit(ResourceClass::Relay), 16);
        let ids = Ids::new(1, 2, 4);
        let mut charges = HashMap::default();
        let mut admit = Admit {
            quotas: &quotas,
            charges: &mut charges,
            ids: &ids,
        };
        let (a, b) = (ids.page(0), ids.page(1));
        admit.waiter(&a).unwrap();
        admit.flight(&a).unwrap();
        admit.waiter(&b).unwrap();
        assert_eq!(admit.flight(&b), Err(Error::Overloaded));
        assert_eq!(admit.waiter(&b), Err(Error::Overloaded));
        assert_eq!(quotas.used(ResourceClass::Waiter), 2);
        admit.release_waiter(&a);
        admit.release_flight(&a);
        admit.release_waiter(&b);
        assert!(charges.is_empty());
        assert_eq!(quotas.used(ResourceClass::Waiter), 0);
        // Policy sees every failed attempt, including the reclamation retry.
        let counts = *rejections.borrow();
        assert!(counts[ResourceClass::Flight as usize] >= 1);
        assert!(counts[ResourceClass::Waiter as usize] >= 1);
        assert_eq!(counts[ResourceClass::Plaintext as usize], 0);
        assert!(reserve(&quotas, ResourceClass::Relay, 17).is_err());
        assert_eq!(ResourceClass::Relay.name(), "relay");
        // Production rules: unconfigured classes keep production defaults.
        assert_eq!(quotas.limit(ResourceClass::IngressConnection), 45);
        assert_eq!(quotas.policy().max_keys(), 4096);
        assert_eq!(
            quotas.policy().floor(ResourceClass::Ciphertext),
            PAGE_BYTES + (64 << 10)
        );
        assert_eq!(read_error(flow_control::Error::Io), Error::Unavailable);
    }
}
