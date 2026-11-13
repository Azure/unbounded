//! Write-back upload to the origin: the [`UploadSink`] seam, the per-cache
//! [`UploadLimiter`], the [`RetryPolicy`] for transient failures, and the
//! in-memory [`MemoryUploadSink`] test double.
//!
//! A sink performs exactly one logical request per call (plus at most one
//! retry after a 401 for a refreshed token). Retrying transient failures and
//! pacing uploads are the caller's job, using [`RetryPolicy`] and
//! [`UploadLimiter`], so the coordinator controls attempt counts
//! (`RACER_WRITE_UPLOAD_ATTEMPTS`) and can stop between attempts when an
//! object is deleted.

use crate::error::{Error, Operation, Result};
use crate::model::{ObjectId, StrongEtag};
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use racer_wire::control::CacheId;
use std::collections::{HashMap, VecDeque};
use std::future::Future;
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant};

/// Put Block List found an existing blob (`409 BlobAlreadyExists` or `412`):
/// another writer created the object first. Never retried.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CommitConflict;

/// Uploads a written object's blocks and commits them.
pub trait UploadSink {
    /// Put one block. The sink computes the Azure CRC-64 of `plaintext` and
    /// sends it as `x-ms-content-crc64`, so the service rejects a corrupted
    /// body. `block_id` is the 64-character base64 block ID.
    fn put_block<'a>(
        &'a self,
        context: &'a OriginContext,
        block_id: &'a [u8; 64],
        plaintext: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()>;

    /// Commit blocks in order with `If-None-Match: *`. Returns the Blob ETag,
    /// or [`CommitConflict`] when the blob already exists. A blob this same
    /// block list already committed (a retry after a lost response) is not a
    /// conflict: its ETag is returned.
    fn put_block_list<'a>(
        &'a self,
        context: &'a OriginContext,
        block_ids: &'a [[u8; 64]],
        content_type: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, std::result::Result<StrongEtag, CommitConflict>>;
}

/// Whether a failed upload request may succeed if repeated: throttling
/// (`429`, `503 ServerBusy`), server errors (`500`, `5xx`), network loss,
/// malformed responses, and local overload. Authorization, missing
/// containers, bad requests, and expired scopes are final.
pub fn retryable(error: &Error) -> bool {
    matches!(
        error,
        Error::Unavailable | Error::BadGateway | Error::Overloaded | Error::Io | Error::Os(_)
    )
}

/// Exponential backoff with jitter for upload retries.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RetryPolicy {
    /// Total attempts, including the first. At least one is always made.
    pub attempts: u32,
    /// Delay before the second attempt, before jitter.
    pub base: Duration,
    /// Largest delay between attempts, before jitter.
    pub cap: Duration,
}

impl Default for RetryPolicy {
    /// `RACER_WRITE_UPLOAD_ATTEMPTS` defaults to 10.
    fn default() -> Self {
        Self {
            attempts: 10,
            base: Duration::from_millis(200),
            cap: Duration::from_secs(30),
        }
    }
}

impl RetryPolicy {
    /// Delay after failed attempt `attempt` (1-based): `min(cap, base *
    /// 2^(attempt-1))`, then jittered uniformly into `[d/2, d]` by `entropy`
    /// so nodes retrying the same throttled account spread out.
    pub fn delay(&self, attempt: u32, entropy: u64) -> Duration {
        let exponent = attempt.saturating_sub(1).min(32);
        let full = self
            .base
            .checked_mul(1u32 << exponent.min(31))
            .unwrap_or(self.cap)
            .min(self.cap);
        let nanos = u64::try_from(full.as_nanos()).unwrap_or(u64::MAX);
        let half = nanos / 2;
        let span = nanos - half;
        let jitter = if span == 0 {
            0
        } else {
            mix(entropy ^ u64::from(attempt)) % (span + 1)
        };
        Duration::from_nanos(half + jitter)
    }

    /// Run `operation` until it succeeds, fails with a non-retryable error,
    /// runs out of attempts, or the next delay would pass the scope's
    /// deadline. `sleep(until)` waits on the worker clock. The last error is
    /// returned.
    pub async fn run<T, Op, OpFuture, Sleep, SleepFuture>(
        &self,
        scope: &RequestScope,
        entropy: u64,
        mut sleep: Sleep,
        mut operation: Op,
    ) -> Result<T>
    where
        Op: FnMut(u32) -> OpFuture,
        OpFuture: Future<Output = Result<T>>,
        Sleep: FnMut(Instant) -> SleepFuture,
        SleepFuture: Future<Output = Result<()>>,
    {
        let mut attempt = 1;
        loop {
            scope.check()?;
            let error = match operation(attempt).await {
                Ok(value) => return Ok(value),
                Err(error) => error,
            };
            if !retryable(&error) || attempt >= self.attempts.max(1) {
                return Err(error);
            }
            let until = scope
                .clock()
                .now()
                .checked_add(self.delay(attempt, entropy))
                .filter(|until| *until < scope.deadline.0)
                .ok_or(error)?;
            sleep(until).await?;
            attempt += 1;
        }
    }
}

/// SplitMix64 finalizer, for jitter.
fn mix(mut x: u64) -> u64 {
    x = x.wrapping_add(0x9E37_79B9_7F4A_7C15);
    x = (x ^ (x >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    x = (x ^ (x >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    x ^ (x >> 31)
}

/// Outcome of [`UploadLimiter::try_acquire`].
#[derive(Debug)]
pub enum Admission {
    /// Send now; hold the permit until the Put Block completes.
    Ready(UploadPermit),
    /// The byte budget is spent; try again at this instant.
    Wait(Instant),
    /// `max_concurrent_blocks` Put Blocks are in flight; try again after
    /// one completes.
    Busy,
}

#[derive(Debug)]
struct LimiterState {
    bytes_per_second: u64,
    max_concurrent: u32,
    /// Byte credit scaled by 1e9, so refill is `rate * elapsed_nanos`. May go
    /// negative: a block larger than the one-second burst is admitted when
    /// the bucket is not in debt, and later blocks wait out the debt.
    credit: i128,
    refilled: Option<Instant>,
    in_flight: u32,
}

const NANOS: i128 = 1_000_000_000;

/// Per-cache, node-wide upload pacing: a token bucket on bytes per second
/// (one second of burst; `0` means unlimited) and a cap on concurrent Put
/// Blocks. Time comes from the caller (the worker clock), so the limiter is
/// deterministic under simulation. Clones share state.
#[derive(Clone, Debug)]
pub struct UploadLimiter(Arc<Mutex<LimiterState>>);

/// One admitted Put Block; releases its concurrency slot on drop.
#[derive(Debug)]
pub struct UploadPermit(Arc<Mutex<LimiterState>>);

impl Drop for UploadPermit {
    fn drop(&mut self) {
        if let Ok(mut state) = self.0.lock() {
            state.in_flight = state.in_flight.saturating_sub(1);
        }
    }
}

impl UploadLimiter {
    /// `max_concurrent` of `0` is treated as `1`.
    pub fn new(bytes_per_second: u64, max_concurrent: u32) -> Self {
        Self(Arc::new(Mutex::new(LimiterState {
            bytes_per_second,
            max_concurrent: max_concurrent.max(1),
            credit: i128::from(bytes_per_second) * NANOS,
            refilled: None,
            in_flight: 0,
        })))
    }

    /// Apply new limits (a republished cache). Credit is clamped to the new
    /// burst; in-flight permits are kept.
    pub fn reconfigure(&self, bytes_per_second: u64, max_concurrent: u32) {
        if let Ok(mut state) = self.0.lock() {
            if state.bytes_per_second != bytes_per_second {
                state.bytes_per_second = bytes_per_second;
                state.credit = state.credit.min(i128::from(bytes_per_second) * NANOS);
            }
            state.max_concurrent = max_concurrent.max(1);
        }
    }

    /// Admit one Put Block of `bytes` at `now`.
    pub fn try_acquire(&self, bytes: u64, now: Instant) -> Admission {
        let Ok(mut state) = self.0.lock() else {
            return Admission::Busy;
        };
        if state.in_flight >= state.max_concurrent {
            return Admission::Busy;
        }
        let rate = i128::from(state.bytes_per_second);
        if rate > 0 {
            if let Some(last) = state.refilled {
                let elapsed = now.saturating_duration_since(last).as_nanos();
                let refill = rate.saturating_mul(i128::try_from(elapsed).unwrap_or(i128::MAX));
                state.credit = state.credit.saturating_add(refill).min(rate * NANOS);
            }
            state.refilled = Some(state.refilled.map_or(now, |last| last.max(now)));
            if state.credit < 0 {
                let wait = (-state.credit + rate - 1) / rate;
                let wait = Duration::from_nanos(u64::try_from(wait).unwrap_or(u64::MAX));
                return Admission::Wait(now.checked_add(wait).unwrap_or(now));
            }
            state.credit -= i128::from(bytes) * NANOS;
        }
        state.in_flight += 1;
        Admission::Ready(UploadPermit(self.0.clone()))
    }

    /// Put Blocks currently admitted.
    pub fn in_flight(&self) -> u32 {
        self.0.lock().map_or(0, |state| state.in_flight)
    }
}

/// Node-wide limiters by cache, shared by every worker. Entries are weak, so
/// a cache with no live limiter holds nothing; dead entries are swept on
/// growth as in [`super::azure::AzureLimits`].
#[derive(Debug, Default)]
pub struct UploadLimits(Mutex<HashMap<CacheId, Weak<Mutex<LimiterState>>>>);

impl UploadLimits {
    /// The limiter for `cache`, created or updated with these limits.
    pub fn limiter(
        &self,
        cache: &CacheId,
        bytes_per_second: u64,
        max_concurrent: u32,
    ) -> UploadLimiter {
        let Ok(mut caches) = self.0.lock() else {
            return UploadLimiter::new(bytes_per_second, max_concurrent);
        };
        if let Some(state) = caches.get(cache).and_then(Weak::upgrade) {
            let limiter = UploadLimiter(state);
            limiter.reconfigure(bytes_per_second, max_concurrent);
            return limiter;
        }
        if caches.len() >= 16 {
            caches.retain(|_, state| state.strong_count() > 0);
        }
        let limiter = UploadLimiter::new(bytes_per_second, max_concurrent);
        caches.insert(cache.clone(), Arc::downgrade(&limiter.0));
        limiter
    }
}

/// Which [`MemoryUploadSink`] call a fault applies to.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UploadCall {
    PutBlock,
    PutBlockList,
}

/// A committed blob in a [`MemoryUploadSink`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryBlob {
    pub data: Vec<u8>,
    pub etag: StrongEtag,
    pub content_type: Option<String>,
    pub block_ids: Vec<[u8; 64]>,
}

#[derive(Debug, Default)]
struct MemoryState {
    uncommitted: HashMap<ObjectId, HashMap<[u8; 64], Vec<u8>>>,
    committed: HashMap<ObjectId, MemoryBlob>,
    faults: VecDeque<(UploadCall, Error)>,
    lose_commit_responses: u32,
    calls: Vec<UploadCall>,
    next_etag: u64,
}

/// In-memory [`UploadSink`] for tests and simulation: blocks are staged per
/// object, Put Block List commits only when the object does not exist yet
/// (like `If-None-Match: *`, except that the same block list committed again
/// returns the existing ETag, as the Azure sink does), and faults can be injected per call. A commit
/// naming a block that was never staged fails with `InvalidRequest`, as Azure
/// answers `400 InvalidBlockList`. Committing discards the other staged
/// blocks, as Azure does.
#[derive(Debug, Default)]
pub struct MemoryUploadSink(Mutex<MemoryState>);

impl MemoryUploadSink {
    pub fn new() -> Self {
        Self::default()
    }

    /// Fail the next `call` with `error` (queued in order; each fault is
    /// consumed by the first matching call).
    pub fn fail_next(&self, call: UploadCall, error: Error) {
        self.state().faults.push_back((call, error));
    }

    /// Commit the next Put Block List but answer it with `Unavailable`, as
    /// if the `201` was lost on the way back.
    pub fn lose_next_commit_response(&self) {
        self.state().lose_commit_responses += 1;
    }

    /// Make `object` exist, as if another writer committed it first.
    pub fn insert_existing(&self, object: &ObjectId, data: Vec<u8>) -> StrongEtag {
        let mut state = self.state();
        let etag = next_etag(&mut state);
        state.committed.insert(
            object.clone(),
            MemoryBlob {
                data,
                etag: etag.clone(),
                content_type: None,
                block_ids: Vec::new(),
            },
        );
        etag
    }

    /// The committed blob for `object`.
    pub fn committed(&self, object: &ObjectId) -> Option<MemoryBlob> {
        self.state().committed.get(object).cloned()
    }

    /// Staged, uncommitted blocks for `object`.
    pub fn staged(&self, object: &ObjectId) -> usize {
        self.state().uncommitted.get(object).map_or(0, HashMap::len)
    }

    /// Every call made so far, including failed ones, in order.
    pub fn calls(&self) -> Vec<UploadCall> {
        self.state().calls.clone()
    }

    fn state(&self) -> std::sync::MutexGuard<'_, MemoryState> {
        self.0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn fault(state: &mut MemoryState, call: UploadCall) -> Result<()> {
        state.calls.push(call);
        match state.faults.iter().position(|(c, _)| *c == call) {
            Some(index) => Err(state.faults.remove(index).map_or(Error::Internal, |f| f.1)),
            None => Ok(()),
        }
    }
}

fn next_etag(state: &mut MemoryState) -> StrongEtag {
    state.next_etag += 1;
    StrongEtag::parse(format!("\"0x{:016X}\"", state.next_etag).as_bytes())
        .unwrap_or_else(|_| unreachable!("formatted ETags are valid"))
}

impl UploadSink for MemoryUploadSink {
    fn put_block<'a>(
        &'a self,
        context: &'a OriginContext,
        block_id: &'a [u8; 64],
        plaintext: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let mut state = self.state();
            Self::fault(&mut state, UploadCall::PutBlock)?;
            state
                .uncommitted
                .entry(context.object.clone())
                .or_default()
                .insert(*block_id, plaintext.to_vec());
            Ok(())
        })
    }

    fn put_block_list<'a>(
        &'a self,
        context: &'a OriginContext,
        block_ids: &'a [[u8; 64]],
        content_type: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, std::result::Result<StrongEtag, CommitConflict>> {
        Box::pin(async move {
            scope.check()?;
            let mut state = self.state();
            Self::fault(&mut state, UploadCall::PutBlockList)?;
            if let Some(existing) = state.committed.get(&context.object) {
                // Our own earlier commit (its response was lost) is success.
                if existing.block_ids == block_ids {
                    return Ok(Ok(existing.etag.clone()));
                }
                return Ok(Err(CommitConflict));
            }
            let staged = state.uncommitted.get(&context.object);
            let mut data = Vec::new();
            for id in block_ids {
                let block = staged
                    .and_then(|s| s.get(id))
                    .ok_or(Error::InvalidRequest)?;
                data.extend_from_slice(block);
            }
            state.uncommitted.remove(&context.object);
            let etag = next_etag(&mut state);
            state.committed.insert(
                context.object.clone(),
                MemoryBlob {
                    data,
                    etag: etag.clone(),
                    content_type: content_type.map(str::to_owned),
                    block_ids: block_ids.to_vec(),
                },
            );
            if state.lose_commit_responses > 0 {
                state.lose_commit_responses -= 1;
                return Err(Error::Unavailable);
            }
            Ok(Ok(etag))
        })
    }
}

#[cfg(test)]
mod tests;
