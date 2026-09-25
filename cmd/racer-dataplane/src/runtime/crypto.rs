//! Owned page-AEAD handoff between exactly one I/O thread and its paired engine.
//!
//! This module composes endpoints only; it implements no queue, wakeup, or crypto.
//! Admission stays on I/O. Reserve a job slot AND its eventual completion slot
//! before enqueueing. The permit follows the job through completion consumption,
//! so cancellation, an abandoned future, or shutdown cannot reclaim its capacity,
//! buffers, or key early. Completion publication never waits for new admission.
//!
//! All polling must register the waker and recheck state before returning Pending.
//! Enqueue wakes crypto; completion wakes I/O; consumption wakes capacity waiters;
//! close wakes both sides. Neither side spins or blocks awaiting the other. This
//! is required even when both OS threads share one allowed CPU.

use super::{admission::Reservation, channel::SendFailure, deadline::RequestScope};
use crate::{
    error::{Error, Operation, Result, deferred, pending},
    memory::pool::{CiphertextPage, PlaintextBuffer, VerifiedPage},
    model::identity::{PageId, WorkerId},
    security::keyring::KeyLease,
};
use std::{
    cell::RefCell,
    collections::HashMap,
    num::NonZeroUsize,
    sync::Arc,
    task::{Context, Poll, Waker},
};

/// I/O-generated identity, independent of flights. Never reuse a sequence within
/// a pair generation; restart increments the generation and rejects late results.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CryptoId {
    pub worker: WorkerId,
    pub generation: u64,
    pub sequence: u64,
}

/// Output capacity was admitted on I/O. The engine cannot reach Admission,
/// flights, metadata catalogs, storage, credentials, or any Rc service graph.
pub enum CryptoInput {
    Decrypt {
        ciphertext: CiphertextPage,
        plaintext: Reservation,
    },
    Encrypt {
        page: PageId,
        plaintext: PlaintextBuffer,
        ciphertext: Reservation,
    },
}

pub enum CryptoOutput {
    /// Retain original ciphertext through completion too; I/O decides whether to
    /// retain it for peer copies/persistence after consuming the completion.
    Decrypted(VerifiedPage, CiphertextPage),
    Encrypted(VerifiedPage, CiphertextPage),
}

/// Composition descriptor shared only by the two endpoints and their permits.
/// A future backend must bound queued + executing + unconsumed completions by
/// capacity, not merely bound the number of jobs waiting to execute.
struct Handoff {
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
}

/// Non-cloneable, pair-bound reservation of both submission and completion space.
/// Only the I/O endpoint can mint it. Failed submission returns the entire job.
/// A capacity reservation cannot be duplicated:
/// ```compile_fail
/// use racer_dataplane::runtime::crypto::CryptoPermit;
/// fn duplicate(permit: CryptoPermit) { let _second = permit.clone(); }
/// ```
pub struct CryptoPermit {
    handoff: Arc<Handoff>,
    id: CryptoId,
}

pub struct CryptoJob {
    pub(crate) permit: CryptoPermit,
    pub(crate) input: CryptoInput,
    pub(crate) key: KeyLease,
    pub(crate) scope: RequestScope,
}

impl CryptoPermit {
    pub fn job(self, input: CryptoInput, key: KeyLease, scope: RequestScope) -> CryptoJob {
        CryptoJob {
            permit: self,
            input,
            key,
            scope,
        }
    }
}

impl CryptoJob {
    pub fn id(&self) -> CryptoId {
        self.permit.id
    }
}

/// Failed/canceled work returns its input allocations and reservations intact.
/// Successful work transfers those reservations to the output page leases.
pub enum CryptoOutcome {
    Completed(CryptoOutput),
    Failed { input: CryptoInput, error: Error },
}

/// Only the engine may create a completion after it has stopped accessing input.
/// The key and permit survive success AND failure until I/O consumes the result.
pub struct CryptoCompletion {
    pub(crate) permit: CryptoPermit,
    pub(crate) outcome: CryptoOutcome,
    pub(crate) key: KeyLease,
    pub(crate) scope: RequestScope,
}

impl CryptoCompletion {
    pub fn id(&self) -> CryptoId {
        self.permit.id
    }
}

/// Endpoints move to their respective threads before constructing local services.
/// They are not Clone: there is exactly one producer/consumer in each direction.
pub struct IoCryptoPort {
    handoff: Arc<Handoff>,
}
pub struct CryptoPort {
    handoff: Arc<Handoff>,
}

/// Composition only, without allocating queues or claiming operational readiness.
pub fn pair(
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
) -> (IoCryptoPort, CryptoPort) {
    let handoff = Arc::new(Handoff {
        worker,
        generation,
        capacity,
    });
    (
        IoCryptoPort {
            handoff: handoff.clone(),
        },
        CryptoPort { handoff },
    )
}

impl IoCryptoPort {
    /// Atomically reserve both directions. Saturation parks with a wakeup rather
    /// than waiting while holding a job-only slot. Reject wrong-worker/stale IDs.
    pub fn poll_reserve(&self, _cx: &mut Context<'_>, _id: CryptoId) -> Poll<Result<CryptoPermit>> {
        Poll::Ready(pending("crypto.reserve"))
    }

    /// Validate the permit belongs to this pair. Closed or rejected submission
    /// returns all ownership; an accepted job lives independently of its waiter.
    pub fn try_submit(&self, job: CryptoJob) -> std::result::Result<(), SendFailure<CryptoJob>> {
        Err(SendFailure {
            command: job,
            error: Error::Unimplemented("crypto.submit"),
        })
    }

    /// Drain even abandoned/stale completions before returning their credits.
    pub fn poll_completion(&self, _cx: &mut Context<'_>) -> Poll<Result<Option<CryptoCompletion>>> {
        Poll::Ready(pending("crypto.completion"))
    }

    /// Refuse new reservations/submissions, but keep completions available.
    pub fn close_submissions(&self) -> Result<()> {
        pending("crypto.close_submissions")
    }
}

impl CryptoPort {
    /// None means closed and all accepted jobs consumed, not temporarily empty.
    pub fn poll_job(&mut self, _cx: &mut Context<'_>) -> Poll<Result<Option<CryptoJob>>> {
        Poll::Ready(pending("crypto.job"))
    }

    /// Uses the job's reserved completion slot, including during drain. A backend
    /// failure returns ownership to the engine for retry/fenced teardown.
    pub fn complete(
        &mut self,
        completion: CryptoCompletion,
    ) -> std::result::Result<(), SendFailure<CryptoCompletion>> {
        Err(SendFailure {
            command: completion,
            error: Error::Unimplemented("crypto.complete"),
        })
    }
}

/// Local submission facade, driven by WorkerService, not by the waiting future.
/// The bounded waiter table outlives canceled futures. Future drop abandons only
/// delivery; the engine/queue retains resources until I/O reaps the completion.
/// I/O checks generation/sequence before delivery and never publishes stale work.
pub struct CryptoClient {
    port: IoCryptoPort,
    waiters: RefCell<HashMap<CryptoId, Option<Waker>>>,
}

impl CryptoClient {
    pub fn new(port: IoCryptoPort) -> Self {
        Self {
            port,
            waiters: RefCell::new(HashMap::new()),
        }
    }

    /// Allocate a unique ID in this pair's generation, reserve both queue slots,
    /// clone the original scope into the owned job, then submit. On rejection,
    /// retain ownership for a wakeable retry or release locally before acceptance.
    /// Record the waiter before enqueue so even immediate completion cannot race
    /// registration. Sequence overflow must drain/restart, never wrap in place.
    pub fn execute<'a>(
        &'a self,
        _input: CryptoInput,
        _key: KeyLease,
        _scope: &'a RequestScope,
    ) -> Operation<'a, CryptoOutput> {
        deferred("crypto.execute")
    }

    /// Called by I/O even when no user futures remain, before admitting more work.
    pub fn poll_budgeted(&self, _work_budget: usize) -> Result<()> {
        pending("crypto.poll_completions")
    }

    pub fn close_submissions(&self) -> Result<()> {
        self.port.close_submissions()
    }

    /// Deadline cancels delivery, not the ownership fence. Keep polling until all
    /// accepted jobs complete; a timeout cannot authorize dropping live buffers.
    pub fn drain<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("crypto.drain")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_owned_messages_and_endpoints_cross_threads() {
        fn send<T: Send + 'static>() {}
        send::<CryptoInput>();
        send::<CryptoOutput>();
        send::<CryptoJob>();
        send::<CryptoCompletion>();
        send::<CryptoPermit>();
        send::<IoCryptoPort>();
        send::<CryptoPort>();
    }

    #[test]
    fn endpoints_share_one_bounded_pair_descriptor() {
        let (io, engine) = pair(WorkerId(7), 12, NonZeroUsize::new(3).unwrap());
        assert!(Arc::ptr_eq(&io.handoff, &engine.handoff));
        assert_eq!(io.handoff.worker, WorkerId(7));
        assert_eq!(engine.handoff.capacity.get(), 3);
        assert_eq!(engine.handoff.generation, 12);
        let (other, _) = pair(WorkerId(7), 13, NonZeroUsize::new(3).unwrap());
        assert!(!Arc::ptr_eq(&io.handoff, &other.handoff));
    }

    // Type-check the ownership path without manufacturing a key, permit, or page.
    // On rejection the caller can retry the very same resource-bearing job.
    #[allow(dead_code)]
    fn round_trip_api(
        io: IoCryptoPort,
        mut engine: CryptoPort,
        job: CryptoJob,
        completion: CryptoCompletion,
    ) {
        if let Err(failure) = io.try_submit(job) {
            let _: CryptoId = failure.command.id();
            let _retry = io.try_submit(failure.command);
        }
        if let Err(failure) = engine.complete(completion) {
            let _retry = engine.complete(failure.command);
        }
    }
}
