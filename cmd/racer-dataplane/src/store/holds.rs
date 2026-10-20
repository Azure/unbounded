//! Worker-local hard holds over the extent volume's storage tier. Each reserved
//! page charges one data slot of the hold quota. Published held pages are
//! storage-tier entries whose `retain_until` is the hold deadline, so the
//! volume keeps their slots until the deadline passes and they survive a
//! crash once committed. Live expiry requires both clocks to pass the
//! deadline. Restart rebuilds holds from storage entries using wall time only.

use super::key::VersionKey;
use crate::error::{Error, Result};
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use std::cell::Cell;
use std::rc::Rc;
use std::time::{Duration, Instant, SystemTime};

pub const MAX_HOLD_TTL: Duration = Duration::from_secs(30 * 24 * 60 * 60);

pub(super) struct Hold {
    pub until: SystemTime,
    pub monotonic: Instant,
    pub pages: usize,
    pub inflight: Rc<Cell<usize>>,
    pub active: Rc<Cell<usize>>,
    /// Page numbers published as storage-tier entries under this hold.
    pub published: HashSet<u32>,
}

impl Hold {
    pub fn new(until: SystemTime, monotonic: Instant) -> Self {
        Self {
            until,
            monotonic,
            pages: 0,
            inflight: Rc::new(Cell::new(0)),
            active: Rc::new(Cell::new(0)),
            published: HashSet::default(),
        }
    }

    pub fn live(&self) -> bool {
        uring_runtime::environment::wall_now() < self.until
            || uring_runtime::environment::now() < self.monotonic
    }

    pub fn unused(&self) -> usize {
        self.pages
            .saturating_sub(self.published.len() + self.inflight.get())
    }
}

pub(super) struct Holds {
    pub entries: HashMap<VersionKey, Hold>,
    pub percent: usize,
}

impl Default for Holds {
    fn default() -> Self {
        Self {
            entries: HashMap::default(),
            percent: 50,
        }
    }
}

impl Holds {
    pub fn expire(&mut self) {
        self.entries
            .retain(|_, hold| hold.live() || hold.inflight.get() != 0 || hold.active.get() != 0);
    }

    pub fn deadline(until: SystemTime) -> Result<Instant> {
        let ttl = until
            .duration_since(uring_runtime::environment::wall_now())
            .map_err(|_| Error::InvalidConfiguration)?;
        if ttl.is_zero() || ttl > MAX_HOLD_TTL {
            return Err(Error::InvalidConfiguration);
        }
        uring_runtime::environment::now()
            .checked_add(ttl)
            .ok_or(Error::InvalidConfiguration)
    }
}

/// Keeps a hold alive while a persistence call protects it.
pub(super) struct HoldGuard(pub Rc<Cell<usize>>);

impl Drop for HoldGuard {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}

/// Charges one reserved page of a hold to an in-flight storage write, from
/// tier selection until publication or abort.
pub(super) struct HoldTicket(pub Rc<Cell<usize>>);

impl Drop for HoldTicket {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
