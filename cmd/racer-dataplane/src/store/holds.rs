//! Worker-local hard holds. Each reserved page charges one whole segment.
//! Live expiry requires both clocks to pass the deadline. Restart uses wall time
//! only; checkpoints and payloads keep the store's existing no-fsync crash limits.

use super::catalog::RecordLocation;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageNumber};
use crate::runtime::HashMap;
use page_alloc::{SegmentId, Segments};
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
    pub locations: HashMap<PageNumber, RecordLocation>,
}

impl Hold {
    pub fn live(&self) -> bool {
        uring_runtime::environment::wall_now() < self.until
            || uring_runtime::environment::now() < self.monotonic
    }

    pub fn unused(&self) -> usize {
        self.pages
            .saturating_sub(self.locations.len() + self.inflight.get())
    }
}

pub(super) struct Holds {
    pub entries: HashMap<ObjectVersion, Hold>,
    pub segments: Option<Rc<Segments>>,
    pub free_reserve: usize,
    pub percent: usize,
}

impl Default for Holds {
    fn default() -> Self {
        Self {
            entries: HashMap::default(),
            segments: None,
            free_reserve: 1,
            percent: 50,
        }
    }
}

impl Holds {
    pub fn expire(&mut self) {
        self.entries
            .retain(|_, hold| hold.live() || hold.inflight.get() != 0 || hold.active.get() != 0);
    }

    pub fn unused(&self) -> usize {
        self.entries.values().map(Hold::unused).sum()
    }

    pub fn segment_held(&self, segment: SegmentId) -> bool {
        self.entries.values().any(|hold| {
            hold.live()
                && hold
                    .locations
                    .values()
                    .any(|location| location.segment == segment)
        })
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

pub(super) struct HoldGuard(pub Rc<Cell<usize>>);

impl Drop for HoldGuard {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
