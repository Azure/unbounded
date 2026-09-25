//! Structured request correlation with no arbitrary fields or header logging.
use crate::{
    error::{Result, pending},
    model::identity::{AttemptId, RequestId},
};
pub struct Tracing;
pub enum Stage {
    Admission,
    Metadata,
    Lookup,
    Fill,
    Delivery,
    Drain,
}
impl Tracing {
    pub fn event(
        &self,
        _request: RequestId,
        _attempt: Option<AttemptId>,
        _stage: Stage,
    ) -> Result<()> {
        pending("tracing.event")
    }
}
#[cfg(test)]
mod tests { /* Verify credentials and opaque metadata are absent from diagnostics. */
}
