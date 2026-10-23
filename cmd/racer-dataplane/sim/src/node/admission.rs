//! Simulator admission policy over the real `flow_control::Quotas`.
//!
//! Production `AdmissionPolicy` (`src/admission.rs`) depends on dataplane
//! configuration and telemetry, so the simulator implements
//! `flow_control::Policy` itself with the resource classes the read path
//! charges, the same limits (`src/admission.rs`, `Policy::limit`) and the
//! same requires-wipe and covers rules. Cache ids are interned to `u32`
//! (one simulated cache), so a charge never clones a string. Rejections are
//! counted per class in a counter shared by every node.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;

use flow_control::{Charge, Policy, Quotas, Rejection};
use racer_core::read::Error;
use racer_core::read::flight::FlightAdmission;
use racer_object_wire::model::PageId;

use super::config::NodeConfig;
use super::ids::{CACHE, Ids};
use crate::scenario::Fixed;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ResourceClass {
    Plaintext,
    Ciphertext,
    DirtyCiphertext,
    Flight,
    Waiter,
    Relay,
}

impl ResourceClass {
    pub const ALL: [Self; 6] = [
        Self::Plaintext,
        Self::Ciphertext,
        Self::DirtyCiphertext,
        Self::Flight,
        Self::Waiter,
        Self::Relay,
    ];

    pub fn name(self) -> &'static str {
        match self {
            Self::Plaintext => "plaintext",
            Self::Ciphertext => "ciphertext",
            Self::DirtyCiphertext => "dirty_ciphertext",
            Self::Flight => "flight",
            Self::Waiter => "waiter",
            Self::Relay => "relay",
        }
    }
}

impl flow_control::Class for ResourceClass {
    const COUNT: usize = 6;

    fn index(self) -> usize {
        self as usize
    }

    fn requires_wipe(self) -> bool {
        !matches!(self, Self::Ciphertext)
    }
}

/// Rejection counts per class, plus key-table rejections at the end.
pub type Rejections = Rc<RefCell<[u64; ResourceClass::ALL.len() + 1]>>;

pub struct SimPolicy {
    limits: [usize; 6],
    rejections: Rejections,
}

/// Production page size; `floor` rules use it like `src/admission.rs`.
pub const PAGE_BYTES: usize = 1 << 20;

impl SimPolicy {
    pub fn new(config: &NodeConfig, rejections: Rejections) -> Self {
        let flights = config.flights as usize;
        Self {
            limits: [
                config.plaintext_bytes as usize,
                config.ciphertext_bytes as usize,
                config.dirty_bytes as usize,
                flights,
                flights.saturating_mul(config.waiters_per_flight as usize),
                config.relay_transfers as usize,
            ],
            rejections,
        }
    }
}

impl Policy for SimPolicy {
    type Class = ResourceClass;
    type Key = u32;

    fn limit(&self, class: ResourceClass) -> usize {
        self.limits[class as usize]
    }

    fn floor(&self, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => PAGE_BYTES,
            ResourceClass::Ciphertext | ResourceClass::DirtyCiphertext => PAGE_BYTES + 16,
            _ => 1,
        }
    }

    fn max_keys(&self) -> usize {
        4096
    }

    fn wakes(_: ResourceClass) -> bool {
        false
    }

    fn covers(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::Ciphertext)
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
        assert_eq!(read_error(flow_control::Error::Io), Error::Unavailable);
    }
}
