//! Bounded sliding whole-page window with ordered, independently leased slices.
//! A stream pins its version and length once. Completed transient page failures
//! can continue using unspent ingress credits, never a new version or budget.
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
};

enum WindowPage {
    Waiting(Operation<'static, (Result<PageResult>, AcquisitionBudget)>),
    Ready(Result<PageResult>),
}

#[cfg(test)]
thread_local! {
    pub(crate) static CONTINUATIONS: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
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
    budget: AcquisitionBudget,
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
    pub fn open(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
    ) -> Result<RangeStream> {
        let budget = super::serve::default_budget(&scope);
        self.open_with_budget(metadata, range, context, membership, scope, budget, None)
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
    /// Partition the original credits across a bounded sliding window. Pending
    /// futures live in the stream, so dropping next_slice cannot restart a page.
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
            while self.ready.len() < self.window_pages
                && !self
                    .ready
                    .iter()
                    .any(|(_, entry)| matches!(entry, WindowPage::Ready(Err(_))))
            {
                let Some(number) = self.next_page else {
                    break;
                };
                if self.budget.remaining_attempts() == 0 && !self.ready.is_empty() {
                    break;
                }
                let page = PageId {
                    version: self.metadata.version.clone(),
                    number,
                };
                // Keep enough credit in each admitted page for the full candidate
                // chain. Smaller concurrency is preferable to splitting a route
                // below its four-link normal allowance. Credits are still bounded
                // by the one ingress budget, not replenished as the window slides.
                let remaining = self.range.last_page().0 - number.0 + 1;
                let attempts = self.budget.remaining_attempts().min(8);
                let links = self.budget.remaining_links().min(16);
                if !self.ready.is_empty() && remaining > 0 && (attempts == 0 || links < 4) {
                    break;
                }
                let child = self.budget.partition(attempts, links)?;
                let result = self.directory.start_page(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &self.scope,
                    child,
                );
                let entry = match result {
                    Ok(future) => WindowPage::Waiting(future),
                    Err(error) => WindowPage::Ready(Err(error)),
                };
                // The failed page stays in the window. If it later continues,
                // preserve the remainder of the range rather than ending early.
                self.advance(number);
                self.ready.push_back((number, entry));
            }
            poll_fn(|cx| {
                loop {
                    if poll_window(&mut self.ready, &mut self.budget, cx).is_pending() {
                        return Poll::Pending;
                    }
                    let Some((number, WindowPage::Ready(Err(error)))) = self.ready.front() else {
                        return Poll::Ready(());
                    };
                    if !continuable(error, &self.budget) {
                        return Poll::Ready(());
                    }
                    // A completed child has returned only its unused credits.
                    // Spend one original attempt for every continuation, including
                    // local admission failures that did no network work. This
                    // bounds retries even when a child returns all its credits.
                    if let Err(error) = self.scope.check().and_then(|()| {
                        self.budget
                            .begin_attempt(std::time::Instant::now(), self.scope.deadline.0)
                            .map(|_| ())
                    }) {
                        self.ready.front_mut().unwrap().1 = WindowPage::Ready(Err(error));
                        return Poll::Ready(());
                    }
                    let number = *number;
                    #[cfg(test)]
                    CONTINUATIONS.with(|count| count.set(count.get() + 1));
                    let child = match self.budget.partition(
                        self.budget.remaining_attempts().min(8),
                        self.budget.remaining_links().min(16),
                    ) {
                        Ok(child) => child,
                        Err(error) => {
                            self.ready.front_mut().unwrap().1 = WindowPage::Ready(Err(error));
                            return Poll::Ready(());
                        }
                    };
                    self.ready.front_mut().unwrap().1 = match self.directory.start_page(
                        PageId {
                            version: self.metadata.version.clone(),
                            number,
                        },
                        self.membership.clone(),
                        &self.context,
                        &self.scope,
                        child,
                    ) {
                        Ok(future) => WindowPage::Waiting(future),
                        Err(error) => WindowPage::Ready(Err(error)),
                    };
                    // Keep the replacement in the window across next_slice drop.
                    // Later completed pages and leases are neither redownloaded
                    // nor discarded while the ordered front makes progress.
                }
            })
            .await;
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
                self.delivery.attach(result.plaintext, slice)
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
fn continuable(error: &Error, budget: &AcquisitionBudget) -> bool {
    matches!(
        error,
        Error::Unavailable | Error::HopBudgetExhausted | Error::Overloaded | Error::Io
    ) && budget.remaining_attempts() > 1
        && budget.remaining_links() > 0
}
fn poll_window(
    window: &mut VecDeque<(PageNumber, WindowPage)>,
    budget: &mut AcquisitionBudget,
    cx: &mut Context<'_>,
) -> Poll<()> {
    for (_, entry) in window.iter_mut() {
        if let WindowPage::Waiting(future) = entry {
            if let Poll::Ready(completion) = future.as_mut().poll(cx) {
                let result = match completion {
                    Ok((result, remaining)) => return_credits(budget, remaining).and(result),
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
fn return_credits(budget: &mut AcquisitionBudget, remaining: AcquisitionBudget) -> Result<()> {
    budget.reunite(remaining)
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
    fn completed_page_failure_continuation_is_bounded_and_cancelable() {
        use crate::{
            memory::pipe::PipePool,
            model::identity::{MembershipVersion, RequestId, WorkerId},
            runtime::{admission::Admission, reactor::Reactor, worker::WorkerMap},
            topology::membership::Membership,
        };
        use std::time::{Duration, Instant};
        for (error, cancel, expired) in [
            (Error::Unavailable, false, false),
            (Error::CorruptRecord, false, false),
            (Error::VersionUnavailable, false, false),
            (Error::Unavailable, true, false),
            (Error::Unavailable, false, true),
        ] {
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let delivery = Rc::new(Delivery::new(
                Rc::new(PipePool::new(admission, reactor)),
                Duration::from_secs(1),
            ));
            // An unavailable owner is a completed local failure. It cannot mint
            // new acquisition credits or spin indefinitely without network I/O.
            let directory = Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    8,
                )
                .unwrap(),
            );
            let metadata = metadata();
            let scope =
                RequestScope::new(RequestId([3; 16]), Instant::now() + Duration::from_secs(10))
                    .unwrap();
            let budget = AcquisitionBudget::new(
                if expired {
                    Instant::now()
                } else {
                    scope.deadline.0
                },
                32,
                96,
            );
            let mut stream = RangeStreams::from_directory(directory, delivery, 2)
                .open_with_budget(
                    metadata.clone(),
                    ByteRange::From(0).resolve(metadata.length).unwrap(),
                    OriginContext {
                        object: metadata.version.object.clone(),
                        metadata: None,
                        authorization: None,
                    },
                    Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap()),
                    scope.clone(),
                    budget,
                    None,
                )
                .unwrap();
            stream
                .ready
                .push_back((PageNumber(0), WindowPage::Ready(Err(error))));
            stream.next_page = Some(PageNumber(1));
            if cancel {
                scope.cancel().unwrap();
            }
            CONTINUATIONS.with(|count| count.set(0));
            let result = futures::executor::block_on(stream.next_slice());
            let expected = if cancel {
                Error::Cancelled
            } else if expired {
                Error::DeadlineExceeded
            } else {
                error
            };
            assert!(matches!(result, Err(actual) if actual == expected));
            assert!(stream.terminated);
            assert!(stream.ready.is_empty());
            assert!(
                futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .is_none()
            );
            let count = CONTINUATIONS.with(std::cell::Cell::get);
            if error == Error::Unavailable && !cancel && !expired {
                assert!(count > 0 && count <= 31);
                assert!(stream.budget.remaining_attempts() < 32);
            } else {
                assert_eq!(count, 0);
                assert_eq!(stream.budget.remaining_attempts(), 32);
            }
        }
    }

    #[test]
    fn continuation_requires_original_credits_and_never_retries_security_or_pin_errors() {
        use std::time::{Duration, Instant};
        let deadline = Instant::now() + Duration::from_secs(10);
        for error in [
            Error::Unavailable,
            Error::HopBudgetExhausted,
            Error::Overloaded,
            Error::Io,
        ] {
            assert!(continuable(&error, &AcquisitionBudget::new(deadline, 2, 1)));
            for (attempts, links) in [(0, 96), (1, 96), (32, 0)] {
                assert!(!continuable(
                    &error,
                    &AcquisitionBudget::new(deadline, attempts, links)
                ));
            }
        }
        for error in [
            Error::Unauthorized,
            Error::Replay,
            Error::CorruptRecord,
            Error::MissingKey,
            Error::OriginRejected,
            Error::OriginForbidden,
            Error::VersionUnavailable,
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::InvalidRequest,
            Error::InvalidRange,
        ] {
            assert!(
                !continuable(&error, &AcquisitionBudget::new(deadline, 32, 96)),
                "{error:?}"
            );
        }
        let mut original = AcquisitionBudget::new(deadline, 32, 96);
        let mut retries = 0;
        // An admission failure can return every child credit without doing I/O.
        // The continuation itself still consumes one original attempt.
        while continuable(&Error::Overloaded, &original) {
            original.begin_attempt(Instant::now(), deadline).unwrap();
            let child = original
                .partition(original.remaining_attempts().min(8), 16)
                .unwrap();
            original.reunite(child).unwrap();
            retries += 1;
        }
        assert_eq!(retries, 31);
        assert_eq!(
            (original.remaining_attempts(), original.remaining_links()),
            (1, 96)
        );
        assert_eq!(original.deadline(), deadline);
        let mut expired = AcquisitionBudget::new(Instant::now(), 32, 96);
        assert_eq!(
            expired.begin_attempt(Instant::now(), deadline),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(
            (expired.remaining_attempts(), expired.remaining_links()),
            (32, 96)
        );
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
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (2, 1)
        );
        // Re-polling a completed page must not return its credits twice.
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_pending());
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (2, 1)
        );
        gate.set(true);
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_ready());
        assert!(matches!(
            window.pop_front(),
            Some((
                PageNumber(0),
                WindowPage::Ready(Err(Error::VersionUnavailable))
            ))
        ));
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
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (0, 0)
        );
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
                budget: AcquisitionBudget::new(scope.deadline.0, 0, 0),
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
