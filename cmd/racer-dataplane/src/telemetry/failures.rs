//! Bounded internal failures, exported separately from low-cardinality metrics.
//! Only typed errors, correlation IDs, and numeric progress/resource facts enter
//! this ring. Object keys, ETags, headers, credentials, and payloads never enter it.
//! A separate 64-entry AEAD ring survives transient admission floods. Its CRC is
//! a receiver-side fingerprint, not proof of equality with sender bytes. Page/AAD
//! hashes are pseudonyms (known inputs can be guessed), never authentication.
//! Supplier is the original response signer; remote is the last reverse signer,
//! not a TCP address. Acquisition IDs can differ from the decrypt request after
//! retention or cross-worker handoff. Disk reconstruction has no peer provenance.
use crate::{
    error::{Error, Result},
    model::{AttemptId, RequestId, ResourceClass, WorkerId},
    runtime::deadline::RequestScope,
};
use ::telemetry::Ring;
use std::sync::{Arc, Mutex};

pub const CAPACITY: usize = 128;
pub const AEAD_CAPACITY: usize = 64;

fn hex(out: &mut impl std::fmt::Write, bytes: &[u8]) -> std::fmt::Result {
    for byte in bytes {
        write!(out, "{byte:02x}")?;
    }
    Ok(())
}

/// Authenticated acquisition identity, not a TCP tuple or proof of body integrity.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct PeerProvenance {
    pub request: RequestId,
    pub attempt: AttemptId,
    pub supplier: [u8; 36],
    pub remote: [u8; 36],
}

/// Fixed-size rejection facts. Page hashes are pseudonyms, not secret-key hashes.
#[derive(Clone, Copy)]
pub(crate) struct AeadFailure {
    pub unix_millis: u64,
    pub request: RequestId,
    pub peer: Option<PeerProvenance>,
    pub page: [u8; 32],
    pub number: u64,
    pub key: [u8; 16],
    pub nonce: [u8; 24],
    pub plaintext: u32,
    pub ciphertext: u32,
    pub aad: [u8; 32],
    pub crc: Option<u64>,
}
impl AeadFailure {
    pub(crate) fn write_fields(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        write!(out, " ms={} request=", self.unix_millis)?;
        hex(out, &self.request.0)?;
        if let Some(peer) = self.peer {
            write!(out, " acquisition=")?;
            hex(out, &peer.request.0)?;
            write!(out, " attempt=")?;
            hex(out, &peer.attempt.0)?;
            write!(
                out,
                " supplier={}",
                std::str::from_utf8(&peer.supplier).unwrap_or("unknown")
            )?;
        }
        self.write_fingerprint(out)
    }
    fn write_fingerprint(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        write!(out, " page=")?;
        hex(out, &self.page)?;
        write!(out, " number={} key=", self.number)?;
        hex(out, &self.key)?;
        write!(out, " nonce=")?;
        hex(out, &self.nonce)?;
        write!(out, " lengths={}/{} aad=", self.plaintext, self.ciphertext)?;
        hex(out, &self.aad)?;
        match self.crc {
            Some(crc) => write!(out, " crc={crc:016x}"),
            None => write!(out, " crc=none"),
        }
    }
}

type AeadRing = Ring<(crate::runtime::crypto::CryptoId, AeadFailure), AEAD_CAPACITY>;

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
            unix_millis: uring_runtime::environment::wall_now()
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
pub struct Failures(
    Arc<Mutex<Ring<(WorkerId, Failure), CAPACITY>>>,
    Arc<Mutex<AeadRing>>,
);

/// Absent in standalone components until the production composition attaches it.
#[derive(Clone, Default)]
pub struct Observer(Option<(Failures, WorkerId)>);
impl Failures {
    pub fn write_aead(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let ring = self.1.lock().unwrap_or_else(|e| e.into_inner()).clone();
        let (total, len) = (ring.total(), ring.len());
        writeln!(
            out,
            "total={total} retained={len} overwritten={} capacity={AEAD_CAPACITY}",
            total.saturating_sub(len as u64)
        )?;
        for (sequence, (id, f)) in ring.iter() {
            write!(
                out,
                "seq={sequence} w={} crypto={}:{} ms={} request=",
                id.worker.0, id.generation, id.sequence, f.unix_millis
            )?;
            hex(out, &f.request.0)?;
            if let Some(p) = f.peer {
                write!(out, " acquisition=")?;
                hex(out, &p.request.0)?;
                write!(out, " attempt=")?;
                hex(out, &p.attempt.0)?;
                write!(
                    out,
                    " supplier={} remote={}",
                    std::str::from_utf8(&p.supplier).unwrap_or("unknown"),
                    std::str::from_utf8(&p.remote).unwrap_or("unknown")
                )?;
            } else {
                write!(
                    out,
                    " acquisition=none attempt=none supplier=none remote=none"
                )?;
            }
            f.write_fingerprint(out)?;
            writeln!(out)?;
        }
        Ok(())
    }
    pub fn observer(&self, worker: WorkerId) -> Observer {
        Observer(Some((self.clone(), worker)))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        // Copy bounded records before formatting; never hold the lock across I/O.
        let ring = self.0.lock().unwrap_or_else(|e| e.into_inner()).clone();
        let (total, len) = (ring.total(), ring.len());
        writeln!(out, "total={total} retained={len} capacity={CAPACITY}")?;
        for (sequence, (worker, failure)) in ring.iter() {
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
                hex(out, &request.0)?;
            } else {
                write!(out, "none")?;
            }
            write!(out, " attempt=")?;
            if let Some(attempt) = failure.attempt {
                hex(out, &attempt.0)?;
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
        Ok(())
    }
}
impl Observer {
    pub(crate) fn record_aead(&self, id: crate::runtime::crypto::CryptoId, failure: AeadFailure) {
        let Some((failures, _)) = &self.0 else {
            return;
        };
        let mut ring = failures.1.lock().unwrap_or_else(|e| e.into_inner());
        ring.push((id, failure));
    }
    pub fn record(&self, failure: Failure) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        let mut ring = failures.0.lock().unwrap_or_else(|e| e.into_inner());
        ring.push((*worker, failure));
    }
    pub fn result<T>(&self, stage: Stage, scope: &RequestScope, result: Result<T>) -> Result<T> {
        if let Err(error) = &result {
            self.record(Failure::new(stage, *error).request(scope));
        }
        result
    }
}

#[cfg(test)]
pub(crate) fn test_aead_failure() -> AeadFailure {
    AeadFailure {
        unix_millis: u64::MAX,
        request: RequestId([255; 16]),
        peer: Some(PeerProvenance {
            request: RequestId([255; 16]),
            attempt: AttemptId([255; 16]),
            supplier: [b'f'; 36],
            remote: [b'f'; 36],
        }),
        page: [255; 32],
        number: u64::MAX,
        key: [255; 16],
        nonce: [255; 24],
        plaintext: u32::MAX,
        ciphertext: u32::MAX,
        aad: [255; 32],
        crc: Some(u64::MAX),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exact_output_and_formatting_outside_both_locks() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(3));
        observer.record(Failure {
            unix_millis: 42,
            stage: Stage::Admission,
            error: Error::Overloaded,
            request: None,
            attempt: None,
            detail: Detail::None,
        });
        let id = crate::runtime::crypto::CryptoId {
            worker: WorkerId(3),
            generation: 4,
            sequence: 5,
        };
        let mut aead = test_aead_failure();
        aead.peer = None;
        aead.crc = None;
        observer.record_aead(id, aead);
        struct Unlocked<'a> {
            failures: &'a Failures,
            text: String,
        }
        impl std::fmt::Write for Unlocked<'_> {
            fn write_str(&mut self, value: &str) -> std::fmt::Result {
                assert!(self.failures.0.try_lock().is_ok());
                assert!(self.failures.1.try_lock().is_ok());
                self.text.push_str(value);
                Ok(())
            }
        }
        let mut out = Unlocked {
            failures: &failures,
            text: String::new(),
        };
        failures.write(&mut out).unwrap();
        assert_eq!(
            out.text,
            "total=1 retained=1 capacity=128\nsequence=1 worker=3 stage=Admission error=Overloaded request=none attempt=none unix_millis=42 detail=None\n"
        );
        out.text.clear();
        failures.write_aead(&mut out).unwrap();
        assert_eq!(
            out.text,
            format!(
                "total=1 retained=1 overwritten=0 capacity=64\nseq=1 w=3 crypto=4:5 ms=18446744073709551615 request={} acquisition=none attempt=none supplier=none remote=none page={} number=18446744073709551615 key={} nonce={} lengths=4294967295/4294967295 aad={} crc=none\n",
                "ff".repeat(16),
                "ff".repeat(32),
                "ff".repeat(16),
                "ff".repeat(24),
                "ff".repeat(32),
            )
        );
    }

    #[test]
    fn failed_formatting_does_not_consume_records() {
        struct Full;
        impl std::fmt::Write for Full {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(7));
        observer.record(Failure::new(Stage::ClientRead, Error::Io));
        observer.record_aead(
            crate::runtime::crypto::CryptoId {
                worker: WorkerId(7),
                generation: 1,
                sequence: 2,
            },
            test_aead_failure(),
        );
        assert!(failures.write(&mut Full).is_err());
        assert!(failures.write_aead(&mut Full).is_err());
        let mut text = String::new();
        failures.write(&mut text).unwrap();
        assert!(text.starts_with("total=1 retained=1 capacity=128\n"));
        text.clear();
        failures.write_aead(&mut text).unwrap();
        assert!(text.starts_with("total=1 retained=1 overwritten=0 capacity=64\n"));
    }

    #[test]
    fn aead_ring_survives_admission_flood_and_wraps_independently() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(3));
        let id = crate::runtime::crypto::CryptoId {
            worker: WorkerId(3),
            generation: 1,
            sequence: 1,
        };
        let record = test_aead_failure();
        observer.record_aead(id, record);
        for _ in 0..4096 {
            observer.record(Failure::new(Stage::Admission, Error::Overloaded));
        }
        let mut text = String::new();
        failures.write_aead(&mut text).unwrap();
        assert!(text.starts_with("total=1 retained=1 overwritten=0 capacity=64\nseq=1 "));
        for _ in 0..AEAD_CAPACITY {
            observer.record_aead(id, record);
        }
        text.clear();
        failures.write_aead(&mut text).unwrap();
        assert!(text.starts_with("total=65 retained=64 overwritten=1 capacity=64\nseq=2 "));
        assert_eq!(text.lines().count(), AEAD_CAPACITY + 1);
    }
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
        // The generic ring's saturation is tested in the core. Account for the
        // widest sequence and total here without exposing a mutable sequence API.
        let sequence_growth: usize = (1..=CAPACITY)
            .map(|sequence| 16 - format!("{sequence:x}").len())
            .sum();
        let worst_case_len = text.len() + sequence_growth + 20 - CAPACITY.to_string().len();
        assert!(
            worst_case_len <= crate::telemetry::MAX_RESPONSE_BYTES - 256,
            "{}",
            worst_case_len
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
        assert_eq!(failures.0.lock().unwrap().total(), 0);
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
