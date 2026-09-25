//! Readiness follows usable resources and credentials, never changes placement.
use crate::error::{Result, pending};
pub struct Health;
#[derive(Clone, Copy, Debug)]
pub enum State {
    Starting,
    Ready,
    Degraded,
    Draining,
    Stopped,
}
impl Health {
    pub fn state(&self) -> Result<State> {
        pending("health.state")
    }
    pub fn transition(&self, _state: State) -> Result<()> {
        pending("health.transition")
    }
}
#[cfg(test)]
mod tests { /* Cover startup failure, expired credentials, overload, and draining. */
}
