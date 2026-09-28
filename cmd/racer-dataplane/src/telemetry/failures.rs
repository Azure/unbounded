//! Bounded internal failures, exported separately from low-cardinality metrics.
//! Only typed errors, correlation IDs, and numeric progress/resource facts enter
//! this ring. Object keys, ETags, headers, credentials, and payloads never enter it.
use crate::{
    error::{Error, Result},
    model::{
        identity::{AttemptId, RequestId, WorkerId},
        limits::ResourceClass,
    },
    runtime::deadline::RequestScope,
};
use std::sync::{Arc, Mutex};

pub const CAPACITY: usize = 128;

#[derive(Clone, Copy, Debug)]
pub enum Stage {
    Admission,
    ClientRead,
    FirstSlice,
    NextSlice,
    ClientWrite,
    RangeScope,
    RangePipe,
    RangeBudget,
    PageDispatch,
    PageAcquire,
    PageAttach,
    CandidateExchange,
    CandidateResponse,
    CandidateExhausted,
    PeerRoute,
    PeerVerify,
    PeerCheckout,
    PeerHandshake,
    PeerHead,
    PeerReceiveAdmission,
    PeerReceiveBody,
    PeerDecode,
    PeerLocal,
    PeerRelay,
}

#[derive(Clone, Copy, Debug, Default)]
pub enum Detail {
    #[default]
    None,
    Page(u64),
    Delivery {
        sent: u64,
        expected: u64,
    },
    Budget {
        attempts: u32,
        links: u8,
    },
    Resource {
        class: ResourceClass,
        used: usize,
        limit: usize,
        requested: usize,
        cache_used: Option<usize>,
        cache_limit: Option<usize>,
    },
    CacheEntries {
        used: usize,
        limit: usize,
    },
}

#[derive(Clone, Copy, Debug)]
pub struct Failure {
    pub unix_millis: u64,
    pub stage: Stage,
    pub error: Error,
    pub request: Option<RequestId>,
    pub attempt: Option<AttemptId>,
    pub detail: Detail,
}
impl Failure {
    pub fn new(stage: Stage, error: Error) -> Self {
        Self {
            unix_millis: crate::runtime::environment::wall_now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis()
                .min(u64::MAX as u128) as u64,
            stage,
            error,
            request: None,
            attempt: None,
            detail: Detail::None,
        }
    }
    pub fn request(mut self, scope: &RequestScope) -> Self {
        self.request = Some(scope.request);
        self
    }
    pub fn attempt(mut self, attempt: AttemptId) -> Self {
        self.attempt = Some(attempt);
        self
    }
    pub fn detail(mut self, detail: Detail) -> Self {
        self.detail = detail;
        self
    }
}

#[derive(Clone, Default)]
pub struct Failures(Arc<Mutex<Ring>>);
struct Ring {
    entries: [Option<(u64, WorkerId, Failure)>; CAPACITY],
    total: u64,
    next: usize,
    len: usize,
}
impl Default for Ring {
    fn default() -> Self {
        Self {
            entries: [None; CAPACITY],
            total: 0,
            next: 0,
            len: 0,
        }
    }
}

/// Absent in standalone components until the production composition attaches it.
#[derive(Clone, Default)]
pub struct Observer(Option<(Failures, WorkerId)>);
impl Failures {
    pub fn observer(&self, worker: WorkerId) -> Observer {
        Observer(Some((self.clone(), worker)))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        // Copy bounded records before formatting; never hold the lock across I/O.
        let (entries, total, next, len) = {
            let ring = self.0.lock().unwrap_or_else(|e| e.into_inner());
            (ring.entries, ring.total, ring.next, ring.len)
        };
        writeln!(out, "total={total} retained={len} capacity={CAPACITY}")?;
        for offset in 0..len {
            let index = (next + CAPACITY - len + offset) % CAPACITY;
            if let Some((sequence, worker, failure)) = entries[index] {
                write!(
                    out,
                    "sequence={sequence} worker={} stage={:?} error={:?} request=",
                    worker.0, failure.stage, failure.error
                )?;
                if let Some(request) = failure.request {
                    for byte in request.0 {
                        write!(out, "{byte:02x}")?;
                    }
                } else {
                    write!(out, "none")?;
                }
                write!(out, " attempt=")?;
                if let Some(attempt) = failure.attempt {
                    for byte in attempt.0 {
                        write!(out, "{byte:02x}")?;
                    }
                } else {
                    write!(out, "none")?;
                }
                writeln!(
                    out,
                    " unix_millis={} detail={:?}",
                    failure.unix_millis, failure.detail
                )?;
            }
        }
        Ok(())
    }
}
impl Observer {
    pub fn record(&self, failure: Failure) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        let mut ring = failures.0.lock().unwrap_or_else(|e| e.into_inner());
        ring.total = ring.total.saturating_add(1);
        let next = ring.next;
        ring.entries[next] = Some((ring.total, *worker, failure));
        ring.next = (next + 1) % CAPACITY;
        ring.len = (ring.len + 1).min(CAPACITY);
    }
    pub fn result<T>(&self, stage: Stage, scope: &RequestScope, result: Result<T>) -> Result<T> {
        if let Err(error) = &result {
            self.record(Failure::new(stage, *error).request(scope));
        }
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn shared_workers_bounded_oldest_first_and_success_is_silent() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(3));
        let scope = RequestScope::new(
            RequestId([0xab; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        observer.result(Stage::ClientRead, &scope, Ok(())).unwrap();
        assert_eq!(failures.0.lock().unwrap().total, 0);
        std::thread::spawn(move || {
            for _ in 0..CAPACITY + 2 {
                observer.record(
                    Failure::new(Stage::NextSlice, Error::Io)
                        .request(&scope)
                        .detail(Detail::Delivery {
                            sent: 16777216,
                            expected: 52157952,
                        }),
                );
            }
        })
        .join()
        .unwrap();
        let mut text = String::new();
        failures.write(&mut text).unwrap();
        assert_eq!(text.lines().count(), CAPACITY + 1);
        assert!(
            text.lines()
                .nth(1)
                .unwrap()
                .starts_with("sequence=3 worker=3 stage=NextSlice error=Io request=abab")
        );
        assert!(text.contains("sent: 16777216, expected: 52157952"));
    }
}
