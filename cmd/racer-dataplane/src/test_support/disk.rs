//! Sparse slab crash images, torn/reordered writes, and direct-I/O alignment faults.
use crate::error::{Result, pending};
pub struct CrashDisk;
impl CrashDisk {
    pub fn crash(&self) -> Result<()> {
        pending("test.disk.crash")
    }
}
