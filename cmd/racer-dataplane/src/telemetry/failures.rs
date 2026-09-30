//! Bounded internal failures, exported separately from low-cardinality metrics.
//! Only typed errors, correlation IDs, and numeric progress/resource facts enter
//! this ring. Object keys, ETags, headers, credentials, and payloads never enter it.
use crate::{
    error::{Error, Result},
    model::{AttemptId, RequestId, ResourceClass, WorkerId},
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
    Body(BodyProgress),
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

/// Fixed-size facts only. Times are absolute Unix milliseconds through the same
/// stable monotonic mapping as signed deadlines, not fresh relative allowances.
/// Missing original/share means this process did not create the candidate.
#[derive(Clone, Copy, Debug)]
pub struct BodyProgress {
    pub received: u32,
    pub expected: u32,
    pub reads: u32,
    pub first: u64,
    pub last: u64,
    pub now: u64,
    pub original: u64,
    pub share: u64,
    pub signed: u64,
    pub remote: [u8; 36],
    pub tuple: Option<(std::net::SocketAddr, std::net::SocketAddr)>,
}

pub(crate) fn timestamp(at: std::time::Instant) -> u64 {
    crate::security::protocol::encode_deadline(crate::runtime::deadline::Deadline(at))
        .unwrap_or_default()
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
                let body = matches!(failure.detail, Detail::Body(_));
                if body {
                    write!(
                        out,
                        "seq={sequence:x} w={} stage={:?} error={:?} request=",
                        worker.0, failure.stage, failure.error
                    )?;
                } else {
                    write!(
                        out,
                        "sequence={sequence} worker={} stage={:?} error={:?} request=",
                        worker.0, failure.stage, failure.error
                    )?;
                }
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
                if let Detail::Body(b) = failure.detail {
                    // Compact formatting keeps all 128 worst-case records within
                    // the existing 64-KiB diagnostic response budget.
                    write!(
                        out,
                        " detail=Body rx={}/{} n={} ms=hex f={:x} l={:x} now={:x} orig={:x} share={:x} sig={:x} remote={}",
                        b.received,
                        b.expected,
                        b.reads,
                        b.first,
                        b.last,
                        b.now,
                        b.original,
                        b.share,
                        b.signed,
                        std::str::from_utf8(&b.remote).unwrap_or("unknown")
                    )?;
                    if let Some((local, remote)) = b.tuple {
                        write!(out, " tcp={local}>{remote}")?;
                    } else {
                        write!(out, " tcp=none")?;
                    }
                    writeln!(out)?;
                } else {
                    writeln!(
                        out,
                        " unix_millis={} detail={:?}",
                        failure.unix_millis, failure.detail
                    )?;
                }
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
    fn body_ring_worst_case_fits_existing_response_budget() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(u16::MAX));
        let address = "[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff%4294967295]:65535"
            .parse()
            .unwrap();
        let body = BodyProgress {
            received: u32::MAX,
            expected: u32::MAX,
            reads: u32::MAX,
            first: u64::MAX,
            last: u64::MAX,
            now: u64::MAX,
            original: u64::MAX,
            share: u64::MAX,
            signed: u64::MAX,
            remote: [b'f'; 36],
            tuple: Some((address, address)),
        };
        failures.0.lock().unwrap().total = u64::MAX - 128;
        for _ in 0..CAPACITY {
            observer.record(Failure {
                unix_millis: u64::MAX,
                stage: Stage::PeerReceiveBody,
                error: Error::InvalidConfiguration,
                request: Some(RequestId([255; 16])),
                attempt: Some(AttemptId([255; 16])),
                detail: Detail::Body(body),
            });
        }
        let mut text = String::new();
        failures.write(&mut text).unwrap();
        assert_eq!(text.lines().count(), CAPACITY + 1);
        assert!(
            text.len() <= crate::telemetry::server::MAX_RESPONSE_BYTES - 256,
            "{}",
            text.len()
        );
    }
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
