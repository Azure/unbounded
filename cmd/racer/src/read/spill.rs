//! Read spill settings: pages a read parks on local disk under short reader holds.

use crate::error::{Error, Result};

/// Largest per-read spill horizon.
pub const MAX_RANGE_SPILL_PAGES: usize = 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Config {
    /// Percent of segment capacity that holds may pin
    /// (`RACER_RETAINED_SEGMENT_PERCENT`).
    pub retained_percent: usize,
    /// Pages a read may fetch ahead of its in-memory window and park on local
    /// disk under its own short hold. Zero disables read spill.
    pub range_spill_pages: usize,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            retained_percent: 50,
            range_spill_pages: 0,
        }
    }
}

impl Config {
    pub fn validate(self) -> Result<()> {
        if !(1..=100).contains(&self.retained_percent)
            || self.range_spill_pages > MAX_RANGE_SPILL_PAGES
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
