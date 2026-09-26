//! Bounded sliding whole-page window with ordered, independently leased slices.
//! A stream pins its version and length once. A late error terminates that stream;
//! it cannot replace headers or reopen against a newer version.
use super::{
    dispatch::WorkerDirectory,
    fill::{Fill, PageResult},
    flight::AcquisitionBudget,
};
use crate::{
    error::{Error, Operation, Result},
    memory::delivery::{Delivery, ReaderLease},
    model::{
        context::OriginContext,
        identity::{PageId, PageNumber},
        metadata::ObjectMetadata,
        range::ResolvedRange,
    },
    runtime::deadline::RequestScope,
    topology::membership::MembershipLease,
};
use std::{
    collections::VecDeque,
    future::poll_fn,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
    time::Instant,
};

/// Only client range progress creates a new allowance. An explicit aggregate
/// budget is conserved across pages, just like budgets passed to peer acquisition.
enum RangeBudget {
    ClientPages { deadline: Instant },
    Shared(AcquisitionBudget),
}
impl RangeBudget {
    fn next_page(&mut self, pending: bool) -> Result<Option<AcquisitionBudget>> {
        match self {
            Self::ClientPages { deadline } => {
                if crate::runtime::environment::now() >= *deadline {
                    return Err(Error::DeadlineExceeded);
                }
                Ok(Some(AcquisitionBudget::new(*deadline, 8, 16)))
            }
            Self::Shared(budget) => {
                let attempts = budget.remaining_attempts().min(8);
                let links = budget.remaining_links().min(16);
                if pending && (attempts == 0 || links < 4) {
                    return Ok(None);
                }
                budget.partition(attempts, links).map(Some)
            }
        }
    }
    fn complete(&mut self, remaining: AcquisitionBudget) -> Result<()> {
        match self {
            Self::ClientPages { .. } => Ok(()),
            Self::Shared(budget) => budget.reunite(remaining),
        }
    }
}

enum WindowPage {
    Waiting(Operation<'static, (Result<PageResult>, AcquisitionBudget)>),
    Ready(Result<PageResult>),
}

pub struct RangeStreams {
    // Retains the same worker-local Fill graph as the coordinator. Actual page
    // acquisition always goes through the stable directory owner.
    _fill: Option<Rc<Fill>>,
    directory: Arc<WorkerDirectory>,
    delivery: Rc<Delivery>,
    window_pages: usize,
}
/// The body stays on the ingress reactor, including its delivery pipe leases.
/// ```compile_fail
/// use racer_dataplane::read::range_stream::RangeStream;
/// fn send<T: Send>() {}
/// send::<RangeStream>();
/// ```
pub struct RangeStream {
    metadata: ObjectMetadata,
    range: ResolvedRange,
    context: OriginContext,
    membership: MembershipLease,
    scope: RequestScope,
    directory: Arc<WorkerDirectory>,
    delivery: Rc<Delivery>,
    window_pages: usize,
    budget: RangeBudget,
    next_page: Option<PageNumber>,
    ready: VecDeque<(PageNumber, WindowPage)>,
    terminated: bool,
}
impl RangeStreams {
    pub fn new(
        fill: Rc<Fill>,
        directory: Arc<WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
    ) -> Self {
        Self {
            _fill: Some(fill),
            directory,
            delivery,
            window_pages,
        }
    }
    /// Construct delivery against an installed directory without retaining an
    /// additional Fill handle. Seeded pages undergo the same full validation;
    /// every unseeded page still goes through its stable directory owner.
    pub fn from_directory(
        directory: Arc<WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
    ) -> Self {
        Self {
            _fill: None,
            directory,
            delivery,
            window_pages,
        }
    }
    pub fn directory(&self) -> &Arc<WorkerDirectory> {
        &self.directory
    }
    /// Open a client range with a bounded allowance for each distinct page and
    /// the original request deadline. Use open_with_budget for an aggregate cap.
    pub fn open(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
    ) -> Result<RangeStream> {
        let budget = RangeBudget::ClientPages {
            deadline: scope.deadline.0,
        };
        self.open_budget(metadata, range, context, membership, scope, budget, None)
    }
    /// Bootstrap supplies its already acquired page zero. The same original
    /// budget then belongs to the stream, with no retry/fanout reset.
    #[allow(clippy::too_many_arguments)]
    pub fn open_with_budget(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
        budget: AcquisitionBudget,
        seed: Option<PageResult>,
    ) -> Result<RangeStream> {
        self.open_budget(
            metadata,
            range,
            context,
            membership,
            scope,
            RangeBudget::Shared(budget),
            seed,
        )
    }
    #[allow(clippy::too_many_arguments)]
    fn open_budget(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
        budget: RangeBudget,
        seed: Option<PageResult>,
    ) -> Result<RangeStream> {
        scope.check()?;
        if self.window_pages == 0 {
            return Err(Error::InvalidConfiguration);
        }
        if context.object != metadata.version.object
            || range.end() > metadata.length
            || range.start() >= range.end()
        {
            return Err(Error::InvalidRange);
        }
        let first = range.first_page();
        let mut stream = RangeStream {
            metadata,
            range,
            context,
            membership,
            scope,
            directory: self.directory.clone(),
            delivery: self.delivery.clone(),
            window_pages: self.window_pages,
            budget,
            next_page: Some(first),
            ready: VecDeque::new(),
            terminated: false,
        };
        if let Some(seed) = seed {
            stream.validate(&seed, first)?;
            stream.ready.push_back((first, WindowPage::Ready(Ok(seed))));
            stream.advance(first);
        }
        Ok(stream)
    }
}
impl RangeStream {
    fn validate(&self, result: &PageResult, number: PageNumber) -> Result<()> {
        validate_pin(&self.metadata, &result.metadata)?;
        result.validate_for(&PageId {
            version: self.metadata.version.clone(),
            number,
        })
    }
    fn advance(&mut self, page: PageNumber) {
        self.next_page = (page != self.range.last_page()).then(|| PageNumber(page.0 + 1));
    }
    /// Admit each distinct page once into a bounded sliding window. Client page
    /// progress gets its own allowance under the original deadline; explicit
    /// aggregate budgets partition credits. Pending futures live in the stream,
    /// so dropping next_slice cannot restart a page or refill its retries.
    pub fn next_slice(&mut self) -> Operation<'_, Option<ReaderLease>> {
        Box::pin(async move {
            if self.terminated {
                return Ok(None);
            }
            if let Err(error) = self.scope.check() {
                self.terminated = true;
                self.ready.clear();
                return Err(error);
            }
            if self.ready.is_empty() && self.next_page.is_none() {
                self.terminated = true;
                return Ok(None);
            }
            // Schedule delivery before starting more page work. Waiting requests
            // cannot pin newly acquired pages merely to discover pipe exhaustion.
            let pipe = match self.delivery.admit(&self.scope).await {
                Ok(pipe) => pipe,
                Err(error) => {
                    self.terminated = true;
                    self.next_page = None;
                    self.ready.clear();
                    return Err(error);
                }
            };
            while self.ready.len() < self.window_pages {
                let Some(number) = self.next_page else {
                    break;
                };
                let page = PageId {
                    version: self.metadata.version.clone(),
                    number,
                };
                let child = match self.budget.next_page(!self.ready.is_empty()) {
                    Ok(Some(child)) => child,
                    Ok(None) => break,
                    Err(error) => {
                        self.terminated = true;
                        self.ready.clear();
                        return Err(error);
                    }
                };
                let result = self.directory.start_page(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &self.scope,
                    child,
                );
                let failed = result.is_err();
                let entry = match result {
                    Ok(future) => WindowPage::Waiting(future),
                    Err(error) => WindowPage::Ready(Err(error)),
                };
                if failed {
                    self.next_page = None;
                } else {
                    self.advance(number);
                }
                self.ready.push_back((number, entry));
            }
            poll_fn(|cx| poll_window(&mut self.ready, &mut self.budget, cx)).await;
            if let Err(error) = self.scope.check() {
                self.terminated = true;
                self.ready.clear();
                return Err(error);
            }
            let Some((number, WindowPage::Ready(result))) = self.ready.pop_front() else {
                self.terminated = true;
                return Ok(None);
            };
            let lease = result.and_then(|result| {
                self.validate(&result, number)?;
                let slice = self
                    .range
                    .slice_at(result.plaintext.page().number)?
                    .ok_or(Error::CorruptRecord)?;
                self.delivery.attach_reserved(result.plaintext, slice, pipe)
            });
            match lease {
                Ok(lease) => Ok(Some(lease)),
                Err(error) => {
                    self.terminated = true;
                    self.next_page = None;
                    self.ready.clear();
                    Err(error)
                }
            }
        })
    }
    pub fn cancel(&mut self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.terminated = true;
            self.next_page = None;
            self.ready.clear();
            self.scope.cancel()
        })
    }
    pub fn buffered_pages(&self) -> usize {
        self.ready.len()
    }
}
fn poll_window(
    window: &mut VecDeque<(PageNumber, WindowPage)>,
    budget: &mut RangeBudget,
    cx: &mut Context<'_>,
) -> Poll<()> {
    for (_, entry) in window.iter_mut() {
        if let WindowPage::Waiting(future) = entry {
            if let Poll::Ready(completion) = future.as_mut().poll(cx) {
                let result = match completion {
                    Ok((result, remaining)) => budget.complete(remaining).and(result),
                    Err(error) => Err(error),
                };
                *entry = WindowPage::Ready(result);
            }
        }
    }
    if window
        .front()
        .is_none_or(|(_, entry)| matches!(entry, WindowPage::Ready(_)))
    {
        Poll::Ready(())
    } else {
        Poll::Pending
    }
}
impl Drop for RangeStream {
    fn drop(&mut self) {
        // ReaderLease and runtime completion owners retain already submitted I/O.
        // This cancels acquisition only; it cannot revoke another reader's lease.
        if !self.terminated {
            let _ = self.scope.cancel();
        }
    }
}
fn validate_pin(expected: &ObjectMetadata, actual: &ObjectMetadata) -> Result<()> {
    if expected.version != actual.version {
        return Err(Error::VersionUnavailable);
    }
    if expected.length != actual.length {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{
        identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag},
        metadata::ExpiresAt,
        range::{ByteRange, PAGE_BYTES},
    };
    fn remaining_credits(budget: &RangeBudget) -> (u32, u8) {
        let RangeBudget::Shared(budget) = budget else {
            panic!("expected aggregate budget")
        };
        (budget.remaining_attempts(), budget.remaining_links())
    }
    #[test]
    fn client_page_progress_outlives_attempt_and_link_totals_without_refilling_retries() {
        let deadline = Instant::now() + std::time::Duration::from_secs(60);
        let mut budget = RangeBudget::ClientPages { deadline };
        // Model a healthy remote page that spends a four-link route and transfers
        // acquisition credits to its destination. Neither spend can accumulate
        // into a range-length limit, and unused credits cannot grow later pages.
        for _ in 0..100 {
            let mut page = budget.next_page(false).unwrap().unwrap();
            assert_eq!(page.remaining_attempts(), 8);
            assert_eq!(page.remaining_links(), 16);
            assert_eq!(page.begin_attempt(Instant::now(), deadline), Ok(deadline));
            page.charge_links(4).unwrap();
            let mut remote = page.partition(4, 4).unwrap();
            for _ in 0..4 {
                remote.begin_attempt(Instant::now(), deadline).unwrap();
            }
            assert_eq!(
                remote.begin_attempt(Instant::now(), deadline),
                Err(Error::Unavailable)
            );
            budget.complete(page).unwrap();
        }
        let mut page = budget.next_page(false).unwrap().unwrap();
        for _ in 0..8 {
            page.begin_attempt(Instant::now(), deadline).unwrap();
        }
        assert_eq!(
            page.begin_attempt(Instant::now(), deadline),
            Err(Error::Unavailable)
        );
        page.charge_links(16).unwrap();
        assert_eq!(page.charge_links(1), Err(Error::HopBudgetExhausted));
        assert_eq!(
            page.begin_attempt(deadline, deadline),
            Err(Error::DeadlineExceeded)
        );
        let mut expired = RangeBudget::ClientPages {
            deadline: Instant::now(),
        };
        assert!(matches!(
            expired.next_page(false),
            Err(Error::DeadlineExceeded)
        ));
    }
    #[test]
    fn explicit_aggregate_range_budget_never_refills_spent_pages() {
        let deadline = Instant::now() + std::time::Duration::from_secs(60);
        let mut budget = RangeBudget::Shared(AcquisitionBudget::new(deadline, 2, 4));
        let mut page = budget.next_page(false).unwrap().unwrap();
        assert!(budget.next_page(true).unwrap().is_none());
        page.begin_attempt(Instant::now(), deadline).unwrap();
        page.begin_attempt(Instant::now(), deadline).unwrap();
        page.charge_links(4).unwrap();
        budget.complete(page).unwrap();
        let mut next = budget.next_page(false).unwrap().unwrap();
        assert_eq!(next.deadline(), deadline);
        assert_eq!(
            next.begin_attempt(Instant::now(), deadline),
            Err(Error::Unavailable)
        );
        assert_eq!(next.charge_links(1), Err(Error::HopBudgetExhausted));
    }
    #[test]
    fn pending_client_window_never_restarts_failed_pages_and_honors_original_deadline() {
        use crate::{
            memory::pipe::PipePool,
            model::identity::{MembershipVersion, RequestId, WorkerId},
            runtime::{admission::Admission, reactor::Reactor, worker::WorkerMap},
            topology::membership::Membership,
        };
        use std::{cell::Cell, time::Duration};
        for expire in [false, true] {
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let streams = RangeStreams::from_directory(
                Arc::new(
                    WorkerDirectory::new(
                        Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                        vec![WorkerId(0)],
                        2,
                    )
                    .unwrap(),
                ),
                Rc::new(Delivery::new(
                    Rc::new(PipePool::new(admission, reactor)),
                    Duration::from_secs(30),
                )),
                2,
            );
            let mut metadata = metadata();
            metadata.length = 100 * PAGE_BYTES;
            let scope =
                RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = streams
                .open(
                    metadata.clone(),
                    ByteRange::From(0).resolve(metadata.length).unwrap(),
                    OriginContext {
                        object: metadata.version.object,
                        metadata: None,
                        authorization: None,
                    },
                    Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap()),
                    scope.clone(),
                )
                .unwrap();
            let attempts = Rc::new(Cell::new(0));
            let gate = Rc::new(Cell::new(false));
            // Script both admitted acquisitions, avoiding full-page allocation.
            // A full pending window must prevent admission of page two onward.
            for number in 0..2 {
                let mut budget = stream.budget.next_page(number != 0).unwrap().unwrap();
                let attempts = attempts.clone();
                let gate = gate.clone();
                stream.ready.push_back((
                    PageNumber(number),
                    WindowPage::Waiting(Box::pin(async move {
                        for _ in 0..8 {
                            budget.begin_attempt(Instant::now(), budget.deadline())?;
                            attempts.set(attempts.get() + 1);
                        }
                        poll_fn(|_| {
                            if gate.get() {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        })
                        .await;
                        let result = budget
                            .begin_attempt(Instant::now(), budget.deadline())
                            .map(|_| unreachable!("same page received fresh retry credits"));
                        Ok((result, budget))
                    })),
                ));
                stream.advance(PageNumber(number));
            }
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..10 {
                // Each temporary next_slice future is dropped while pending.
                assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
                assert_eq!(attempts.get(), 16);
                assert_eq!(stream.buffered_pages(), 2);
                assert_eq!(stream.next_page, Some(PageNumber(2)));
            }
            let expected = if expire {
                stream.scope.deadline.0 = Instant::now();
                Error::DeadlineExceeded
            } else {
                gate.set(true);
                Error::Unavailable
            };
            assert!(matches!(
                stream.next_slice().as_mut().poll(&mut cx),
                Poll::Ready(Err(error)) if error == expected
            ));
            assert_eq!(attempts.get(), 16);
            assert_eq!(stream.buffered_pages(), 0);
            assert!(matches!(
                stream.next_slice().as_mut().poll(&mut cx),
                Poll::Ready(Ok(None))
            ));
        }
    }
    fn metadata() -> ObjectMetadata {
        ObjectMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: PAGE_BYTES + 7,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        }
    }
    #[test]
    fn out_of_order_completion_waits_for_front_and_returns_only_unused_credits() {
        use std::{
            cell::Cell,
            time::{Duration, Instant},
        };
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut budget = AcquisitionBudget::new(deadline, 6, 8);
        let mut first_budget = budget.partition(3, 4).unwrap();
        let mut last_budget = budget.partition(3, 4).unwrap();
        first_budget
            .begin_attempt(Instant::now(), deadline)
            .unwrap();
        last_budget.begin_attempt(Instant::now(), deadline).unwrap();
        last_budget.charge_links(3).unwrap();
        let gate = Rc::new(Cell::new(false));
        let opened = gate.clone();
        let first = Box::pin(async move {
            poll_fn(|_| {
                if opened.get() {
                    Poll::Ready(())
                } else {
                    Poll::Pending
                }
            })
            .await;
            Ok((Err(Error::VersionUnavailable), first_budget))
        });
        let last = Box::pin(async move { Ok((Err(Error::Unavailable), last_budget)) });
        let mut budget = RangeBudget::Shared(budget);
        let mut window = VecDeque::from([
            (PageNumber(0), WindowPage::Waiting(first)),
            (PageNumber(1), WindowPage::Waiting(last)),
        ]);
        let waker = futures::task::noop_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_pending());
        assert!(matches!(
            window[1].1,
            WindowPage::Ready(Err(Error::Unavailable))
        ));
        assert_eq!(remaining_credits(&budget), (2, 1));
        // Re-polling a completed page must not return its credits twice.
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_pending());
        assert_eq!(remaining_credits(&budget), (2, 1));
        gate.set(true);
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_ready());
        assert!(matches!(
            window.pop_front(),
            Some((
                PageNumber(0),
                WindowPage::Ready(Err(Error::VersionUnavailable))
            ))
        ));
        let RangeBudget::Shared(budget) = budget else {
            unreachable!()
        };
        assert_eq!(
            (
                budget.remaining_attempts(),
                budget.remaining_links(),
                budget.deadline()
            ),
            (4, 5, deadline)
        );
    }
    #[test]
    fn abandoning_window_drops_waiters_without_refunding_outstanding_spends() {
        use std::time::{Duration, Instant};
        struct Dropped(Rc<std::cell::Cell<bool>>);
        impl Drop for Dropped {
            fn drop(&mut self) {
                self.0.set(true);
            }
        }
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut budget = AcquisitionBudget::new(deadline, 3, 4);
        let child = budget.partition(3, 4).unwrap();
        let mut budget = RangeBudget::Shared(budget);
        let dropped = Rc::new(std::cell::Cell::new(false));
        let guard = Dropped(dropped.clone());
        let future = Box::pin(async move {
            let _guard = guard;
            std::future::pending::<()>().await;
            Ok((Err(Error::Cancelled), child))
        });
        let mut window = VecDeque::from([(PageNumber(0), WindowPage::Waiting(future))]);
        let waker = futures::task::noop_waker();
        assert!(
            poll_window(&mut window, &mut budget, &mut Context::from_waker(&waker)).is_pending()
        );
        window.clear();
        assert!(dropped.get());
        assert_eq!(remaining_credits(&budget), (0, 0));
    }
    #[test]
    fn stream_pin_rejects_version_or_length_changes_but_not_expiration() {
        let expected = metadata();
        let mut actual = expected.clone();
        actual.expires_at = ExpiresAt(std::time::SystemTime::now());
        assert_eq!(validate_pin(&expected, &actual), Ok(()));
        actual.length += 1;
        assert_eq!(validate_pin(&expected, &actual), Err(Error::CorruptRecord));
        actual.version.etag = StrongEtag::test_value("v2");
        assert_eq!(
            validate_pin(&expected, &actual),
            Err(Error::VersionUnavailable)
        );
    }
    #[test]
    fn ordered_slices_cross_page_boundary_without_object_sized_plan() {
        let range = ByteRange::Closed {
            first: PAGE_BYTES - 2,
            last: PAGE_BYTES + 6,
        }
        .resolve(PAGE_BYTES + 7)
        .unwrap();
        assert_eq!(range.first_page(), PageNumber(0));
        assert_eq!(range.last_page(), PageNumber(1));
        let first = range.slice_at(PageNumber(0)).unwrap().unwrap();
        let last = range.slice_at(PageNumber(1)).unwrap().unwrap();
        assert_eq!((first.offset, first.length), ((PAGE_BYTES - 2) as u32, 2));
        assert_eq!((last.offset, last.length), (0, 7));
        assert!(range.slice_at(PageNumber(2)).unwrap().is_none());
    }

    #[test]
    fn responses_stream_more_than_three_pages_only_with_client_sized_http_framing() {
        use crate::{
            client::response::Responses,
            http::{codec::Codec, io::HttpIo, pool::ConnectionLease},
            memory::{
                pipe::PipePool,
                pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
            },
            model::{
                envelope::{KeyId, Nonce, PageEnvelope},
                identity::{RequestId, WorkerId},
                limits::ResourceClass,
            },
            read::serve::ReadResponse,
            runtime::{admission::Admission, reactor::Reactor, worker::WorkerMap},
            topology::membership::Membership,
        };
        use std::{
            io::{Read, Write},
            os::unix::net::UnixStream,
            time::{Duration, Instant},
        };
        let total = 4 * PAGE_BYTES + 17;
        let range = ByteRange::From(PAGE_BYTES).resolve(total).unwrap();
        for capped in [true, false] {
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(
                    32768,
                    if capped {
                        PAGE_BYTES + 16
                    } else {
                        i64::MAX as u64
                    },
                ),
                admission.clone(),
            ));
            let delivery = Rc::new(Delivery::new(
                Rc::new(PipePool::new(admission.clone(), reactor.clone())),
                Duration::from_secs(30),
            ));
            let directory = Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    8,
                )
                .unwrap(),
            );
            let mut metadata = metadata();
            metadata.length = total;
            let scope =
                RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let membership = Arc::new(
                Membership::validate(crate::model::identity::MembershipVersion(1), vec![]).unwrap(),
            );
            let mut ready = VecDeque::new();
            for number in 1..=4 {
                let length = if number == 4 { 17 } else { PAGE_BYTES as usize };
                let page = PageId {
                    version: metadata.version.clone(),
                    number: PageNumber(number),
                };
                let plaintext = VerifiedPage {
                    inner: Arc::new(VerifiedBytes {
                        page: page.clone(),
                        bytes: vec![number as u8; length],
                        reservation: admission
                            .reserve(
                                Some(&metadata.version.object.cache),
                                ResourceClass::Plaintext,
                                length,
                            )
                            .unwrap(),
                    }),
                };
                let ciphertext = CiphertextPage {
                    inner: Arc::new(CiphertextBytes {
                        envelope: PageEnvelope {
                            page,
                            key_id: KeyId([1; 16]),
                            nonce: Nonce([2; 24]),
                            plaintext_length: length as u32,
                            ciphertext_length: length as u32 + 16,
                        },
                        bytes: vec![0; length + 16],
                        reservation: admission
                            .reserve(
                                Some(&metadata.version.object.cache),
                                ResourceClass::Ciphertext,
                                length + 16,
                            )
                            .unwrap(),
                    }),
                };
                ready.push_back((
                    PageNumber(number),
                    WindowPage::Ready(Ok(PageResult {
                        metadata: metadata.clone(),
                        plaintext,
                        ciphertext,
                    })),
                ));
            }
            let stream = RangeStream {
                metadata: metadata.clone(),
                range,
                context: OriginContext {
                    object: metadata.version.object.clone(),
                    metadata: None,
                    authorization: None,
                },
                membership,
                scope: scope.clone(),
                directory,
                delivery: delivery.clone(),
                window_pages: 4,
                budget: RangeBudget::Shared(AcquisitionBudget::new(scope.deadline.0, 0, 0)),
                next_page: None,
                ready,
                terminated: false,
            };
            let response = ReadResponse {
                metadata,
                range: Some(range),
                body: Some(stream),
            };
            let responses = Responses::new(io.clone(), delivery);
            let (server, mut client) = UnixStream::pair().unwrap();
            let reader = std::thread::spawn(move || {
                client
                    .set_read_timeout(Some(Duration::from_secs(60)))
                    .unwrap();
                client
                    .write_all(b"GET / HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n\r\n")
                    .unwrap();
                let mut head = Vec::new();
                let mut byte = [0; 1];
                while !head.ends_with(b"\r\n\r\n") {
                    if client.read(&mut byte).unwrap() == 0 {
                        assert!(capped);
                        assert!(head.is_empty());
                        return;
                    }
                    head.push(byte[0]);
                }
                assert!(!capped);
                let head = String::from_utf8(head).unwrap();
                assert!(head.starts_with("HTTP/1.1 206"));
                assert!(head.contains(&format!("Content-Length: {}\r\n", 3 * PAGE_BYTES + 17)));
                let mut scratch = [0; 65536];
                for number in 1..=4 {
                    let mut left = if number == 4 { 17 } else { PAGE_BYTES as usize };
                    while left != 0 {
                        let count = left.min(scratch.len());
                        client.read_exact(&mut scratch[..count]).unwrap();
                        assert!(scratch[..count].iter().all(|value| *value == number));
                        left -= count;
                    }
                }
            });
            let work = async {
                let connection = ConnectionLease::from_accepted(server.into(), &admission)?;
                let head = io.receive_head(connection, &scope).await?;
                responses
                    .send(head.connection, response, &scope)
                    .await
                    .map(drop)
            };
            let mut work = std::pin::pin!(work);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let result = loop {
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    break result;
                }
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            };
            if capped {
                assert_eq!(result, Err(Error::InvalidRequest));
            } else {
                result.unwrap();
            }
            reader.join().unwrap();
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }
}
