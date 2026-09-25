//! Controlled monotonic/wall clocks, entropy, and scheduling for future model tests.
use crate::error::{Result, pending};
pub struct Clock;
impl Clock {
    pub fn advance(&self, _duration: std::time::Duration) -> Result<()> {
        pending("test.clock.advance")
    }
    pub fn jump_wall(&self, _time: std::time::SystemTime) -> Result<()> {
        pending("test.clock.jump_wall")
    }
}
