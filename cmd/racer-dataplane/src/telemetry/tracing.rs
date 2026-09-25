//! Structured request correlation with no arbitrary fields or header logging.
use crate::{
    error::{Error, Result},
    model::identity::{AttemptId, RequestId},
};
use std::sync::{Arc, Mutex};
pub const TRACE_CAPACITY: usize = 128;
#[derive(Clone, Default)]
pub struct Tracing(Arc<Mutex<Ring>>);
struct Ring {
    entries: [Option<TraceEvent>; TRACE_CAPACITY],
    next: usize,
    len: usize,
    overwritten: u64,
}
impl Default for Ring {
    fn default() -> Self {
        Self {
            entries: [None; TRACE_CAPACITY],
            next: 0,
            len: 0,
            overwritten: 0,
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Stage {
    Admission,
    Metadata,
    Lookup,
    Fill,
    Delivery,
    Drain,
}
/// The only trace payload. There is no arbitrary field, text, header, key,
/// credential, or request-context argument and no logging/export callback.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TraceEvent {
    pub request: RequestId,
    pub attempt: Option<AttemptId>,
    pub stage: Stage,
}
impl Tracing {
    pub fn event(
        &self,
        request: RequestId,
        attempt: Option<AttemptId>,
        stage: Stage,
    ) -> Result<()> {
        let mut ring = self.0.lock().map_err(|_| Error::Unavailable)?;
        let next = ring.next;
        ring.entries[next] = Some(TraceEvent {
            request,
            attempt,
            stage,
        });
        ring.next = (next + 1) % TRACE_CAPACITY;
        if ring.len == TRACE_CAPACITY {
            ring.overwritten = ring.overwritten.saturating_add(1);
        } else {
            ring.len += 1;
        }
        Ok(())
    }
    /// Copy oldest-first into caller-bounded storage. Traces are never HTTP output.
    pub fn snapshot(&self, output: &mut [Option<TraceEvent>]) -> Result<usize> {
        let ring = self.0.lock().map_err(|_| Error::Unavailable)?;
        let count = ring.len.min(output.len());
        output.fill(None);
        let start = (ring.next + TRACE_CAPACITY - ring.len) % TRACE_CAPACITY;
        for (index, target) in output[..count].iter_mut().enumerate() {
            *target = ring.entries[(start + index) % TRACE_CAPACITY];
        }
        Ok(count)
    }
    pub fn overwritten(&self) -> Result<u64> {
        Ok(self.0.lock().map_err(|_| Error::Unavailable)?.overwritten)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn ring_is_bounded_and_contains_only_typed_correlation() {
        let tracing = Tracing::default();
        for index in 0..TRACE_CAPACITY + 3 {
            tracing
                .event(
                    RequestId([index as u8; 16]),
                    Some(AttemptId([7; 16])),
                    Stage::Lookup,
                )
                .unwrap();
        }
        let mut snapshot = [None; TRACE_CAPACITY + 1];
        assert_eq!(tracing.snapshot(&mut snapshot).unwrap(), TRACE_CAPACITY);
        assert_eq!(
            snapshot[0].unwrap(),
            TraceEvent {
                request: RequestId([3; 16]),
                attempt: Some(AttemptId([7; 16])),
                stage: Stage::Lookup
            }
        );
        assert!(snapshot[TRACE_CAPACITY].is_none());
        assert_eq!(tracing.overwritten().unwrap(), 3);
        assert_eq!(tracing.snapshot(&mut []).unwrap(), 0);
        assert!(std::mem::size_of::<TraceEvent>() <= 40);
    }
}
