//! Partial operations, delayed CQEs, disconnects, and resource-accounting fixtures.
use crate::{
    error::{Result, pending},
    runtime::reactor::IoId,
};
pub struct FaultIo;
impl FaultIo {
    pub fn complete(&self, _id: IoId, _bytes: usize) -> Result<()> {
        pending("test.io.complete")
    }
}
