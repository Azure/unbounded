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
};

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
            while self.ready.len() < self.window_pages {
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
}
