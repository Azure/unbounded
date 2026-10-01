//! Shared client/peer coordinator. Fresh admission, explicit version pins, and
//! page-zero bootstrap use the same metadata owner and Fill flights.
pub mod candidates;
pub mod dispatch;
pub mod drivers {
    //! Worker-owned acquisition futures survive ingress future cancellation. The worker
    //! polls this bounded queue alongside reactor completions until shutdown is fenced.
    use crate::error::{Error, Operation, Result};
    use std::{
        cell::{Cell, RefCell},
        collections::VecDeque,
        rc::Rc,
        sync::{
            Arc,
            atomic::{AtomicBool, Ordering},
        },
        task::{Context, Poll, Waker},
    };

    thread_local! {
        // Only selection is thread-local. Workers own the actual queues and permits
        // retain their original owner across nested scopes and deferred submission.
        static CURRENT: RefCell<Option<Rc<DriverQueue>>> = const { RefCell::new(None) };
    }
    const MAX_DRIVERS: usize = 1024;
    #[derive(Default)]
    pub struct DriverQueue {
        drivers: RefCell<VecDeque<Driver>>,
        new: RefCell<Vec<Operation<'static, ()>>>,
        count: Cell<usize>,
        owner: RefCell<Option<Waker>>,
    }
    struct Driver {
        operation: Operation<'static, ()>,
        wake: Arc<Runnable>,
    }
    pub(crate) struct Runnable {
        ready: AtomicBool,
        owner: futures::task::AtomicWaker,
    }
    impl Runnable {
        pub(crate) fn new() -> Arc<Self> {
            Arc::new(Self {
                ready: AtomicBool::new(true),
                owner: futures::task::AtomicWaker::new(),
            })
        }
        pub(crate) fn poll<T>(
            self: &Arc<Self>,
            future: std::pin::Pin<&mut impl std::future::Future<Output = T>>,
            cx: &mut Context<'_>,
            force: bool,
        ) -> Poll<T> {
            self.owner.register(cx.waker());
            if !self.ready.swap(false, Ordering::AcqRel) && !force {
                return Poll::Pending;
            }
            let waker = Waker::from(self.clone());
            future.poll(&mut Context::from_waker(&waker))
        }
    }
    impl std::task::Wake for Runnable {
        fn wake(self: Arc<Self>) {
            self.wake_by_ref();
        }
        fn wake_by_ref(self: &Arc<Self>) {
            self.ready.store(true, Ordering::Release);
            self.owner.wake();
        }
    }
    pub struct QueueGuard {
        previous: Option<Rc<DriverQueue>>,
    }
    impl Drop for QueueGuard {
        fn drop(&mut self) {
            CURRENT.with(|current| current.replace(self.previous.take()));
        }
    }
    impl DriverQueue {
        /// Discard simulated process tasks without polling acquisition or write work.
        #[cfg(test)]
        pub(crate) fn simulation_crash(&self) {
            let drivers = std::mem::take(&mut *self.drivers.borrow_mut());
            let new = std::mem::take(&mut *self.new.borrow_mut());
            self.count.set(self.count.get() - drivers.len() - new.len());
            drop((drivers, new));
        }
        /// Select this queue only during poll/drop, never across an async suspension.
        pub fn scope<F: std::future::Future>(self: &Rc<Self>, future: F) -> Queued<F> {
            Queued {
                queue: self.clone(),
                future: Some(Box::pin(future)),
            }
        }
        pub fn enter(self: &Rc<Self>) -> QueueGuard {
            QueueGuard {
                previous: CURRENT.with(|current| current.replace(Some(self.clone()))),
            }
        }
        pub fn pending(&self) -> usize {
            self.count.get()
        }
        pub fn poll(self: &Rc<Self>, cx: &mut Context<'_>, budget: usize) {
            let _queue = self.enter();
            *self.owner.borrow_mut() = Some(cx.waker().clone());
            // Child operations use a separate inbox. Never recursively poll futures
            // already borrowed by this worker's outer polling turn.
            let Ok(mut drivers) = self.drivers.try_borrow_mut() else {
                return;
            };
            drivers.extend(self.new.borrow_mut().drain(..).map(|operation| Driver {
                operation,
                wake: Runnable::new(),
            }));
            for _ in 0..budget.min(drivers.len()) {
                let Some(mut driver) = drivers.pop_front() else {
                    break;
                };
                match driver
                    .wake
                    .poll(std::pin::Pin::new(&mut driver.operation), cx, false)
                {
                    Poll::Ready(_) => self.count.set(self.count.get() - 1),
                    Poll::Pending => drivers.push_back(driver),
                }
            }
        }
    }
    pub struct Queued<F> {
        queue: Rc<DriverQueue>,
        future: Option<std::pin::Pin<Box<F>>>,
    }
    impl<F: std::future::Future> std::future::Future for Queued<F> {
        type Output = F::Output;
        fn poll(self: std::pin::Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
            let this = self.get_mut();
            let _queue = this.queue.enter();
            this.future
                .as_mut()
                .expect("queued future")
                .as_mut()
                .poll(cx)
        }
    }
    impl<F> Drop for Queued<F> {
        fn drop(&mut self) {
            let _queue = self.queue.enter();
            self.future.take();
        }
    }
    fn current() -> Option<Rc<DriverQueue>> {
        CURRENT.with(|current| current.borrow().clone())
    }
    pub struct Permit {
        queue: Rc<DriverQueue>,
        reserved: bool,
    }
    impl Permit {
        pub fn submit(mut self, driver: Operation<'static, ()>) {
            self.queue.new.borrow_mut().push(driver);
            self.reserved = false;
            let waker = self.queue.owner.borrow().clone();
            if let Some(waker) = waker {
                waker.wake();
            }
        }
    }
    impl Drop for Permit {
        fn drop(&mut self) {
            if self.reserved {
                self.queue.count.set(self.queue.count.get() - 1);
            }
        }
    }
    pub fn reserve() -> Result<Permit> {
        let queue = current().ok_or(Error::InvalidConfiguration)?;
        if queue.count.get() >= MAX_DRIVERS {
            return Err(Error::Overloaded);
        }
        queue.count.set(queue.count.get() + 1);
        Ok(Permit {
            queue,
            reserved: true,
        })
    }
    pub fn spawn(driver: Operation<'static, ()>) -> Result<()> {
        reserve()?.submit(driver);
        Ok(())
    }
    pub fn poll(cx: &mut Context<'_>, budget: usize) {
        if let Some(queue) = current() {
            queue.poll(cx, budget);
        }
    }
    pub fn pending() -> usize {
        current().map_or(0, |queue| queue.pending())
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use futures::{channel::oneshot, task::noop_waker};
        use std::{cell::Cell, rc::Rc};
        #[test]
        fn blocked_driver_is_not_repolled_until_its_own_wake() {
            let queue = Rc::new(DriverQueue::default());
            let _owner = queue.enter();
            let polls = Rc::new(Cell::new(0));
            let observed = polls.clone();
            let wake = Rc::new(RefCell::new(None::<Waker>));
            let saved = wake.clone();
            spawn(Box::pin(std::future::poll_fn(move |cx| {
                observed.set(observed.get() + 1);
                *saved.borrow_mut() = Some(cx.waker().clone());
                Poll::Pending
            })))
            .unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..100 {
                queue.poll(&mut cx, 64);
            }
            assert_eq!(polls.get(), 1);
            wake.borrow().as_ref().unwrap().wake_by_ref();
            queue.poll(&mut cx, 64);
            assert_eq!(polls.get(), 2);
            queue.simulation_crash();
            assert_eq!(queue.pending(), 0);
        }
        #[test]
        fn workers_isolate_permits_children_and_capacity_on_one_thread() {
            let a = Rc::new(DriverQueue::default());
            let b = Rc::new(DriverQueue::default());
            let _a = a.enter();
            let permit = reserve().unwrap();
            let completed = Rc::new(Cell::new(false));
            let child = completed.clone();
            {
                let _b = b.enter();
                permit.submit(Box::pin(async move {
                    spawn(Box::pin(async move {
                        child.set(true);
                        Ok(())
                    }))?;
                    Ok(())
                }));
                let permits: Vec<_> = (0..MAX_DRIVERS).map(|_| reserve().unwrap()).collect();
                assert!(matches!(reserve(), Err(Error::Overloaded)));
                assert_eq!(a.pending(), 1);
                drop(permits);
            }
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            b.poll(&mut cx, 8);
            assert!(!completed.get());
            assert_eq!(b.pending(), 0);
            a.poll(&mut cx, 8);
            assert!(!completed.get());
            assert_eq!(a.pending(), 1);
            a.poll(&mut cx, 8);
            assert!(completed.get());
            assert_eq!(a.pending(), 0);
        }
        #[test]
        fn owned_operation_progresses_after_request_receiver_disappears() {
            let queue = Rc::new(DriverQueue::default());
            let _owner = queue.enter();
            let complete = Rc::new(Cell::new(false));
            let observed = complete.clone();
            let (completion, fence) = oneshot::channel::<()>();
            let (reply, reader) = oneshot::channel::<()>();
            spawn(Box::pin(async move {
                fence.await.map_err(|_| Error::Io)?;
                observed.set(true);
                let _ = reply.send(());
                Ok(())
            }))
            .unwrap();
            let waker = noop_waker();
            let mut cx = Context::from_waker(&waker);
            poll(&mut cx, 8);
            drop(reader);
            assert!(!complete.get());
            assert_eq!(pending(), 1);
            completion.send(()).unwrap();
            poll(&mut cx, 8);
            assert!(complete.get());
            assert_eq!(pending(), 0);
        }
        #[test]
        fn nested_bootstrap_driver_is_polled_without_recursive_table_borrow() {
            let queue = Rc::new(DriverQueue::default());
            let _owner = queue.enter();
            let (send, mut result) = oneshot::channel();
            spawn(Box::pin(async move {
                let (child, receive) = oneshot::channel();
                spawn(Box::pin(async move {
                    child.send(7).map_err(|_| Error::Cancelled)?;
                    Ok(())
                }))?;
                let value = receive.await.map_err(|_| Error::Unavailable)?;
                let _ = send.send(value);
                Ok(())
            }))
            .unwrap();
            let waker = noop_waker();
            let mut cx = Context::from_waker(&waker);
            for _ in 0..4 {
                poll(&mut cx, 8);
            }
            assert_eq!(result.try_recv().unwrap(), Some(7));
            assert_eq!(pending(), 0);
        }
        #[test]
        fn acquisition_requires_an_installed_worker_queue() {
            assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
            let queue = Rc::new(DriverQueue::default());
            {
                let _owner = queue.enter();
                let permit = reserve().unwrap();
                assert_eq!(queue.pending(), 1);
                drop(permit);
                assert_eq!(queue.pending(), 0);
            }
            assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
        }
    }
}
pub mod fill;
pub mod flight;
pub mod hedge {
    //! Opt-in, node-shared speculative page capacity. No background task or byte bypass.
    //!
    //! RACER_PAGE_HEDGE_SLOTS=0 disables hedging. RACER_PAGE_HEDGE_DELAY_MS defaults
    //! to 100; RACER_PAGE_HEDGE_BYTES defaults to one maximum duplicate
    //! plaintext+ciphertext pair (32 MiB + 16).
    //! Only noncandidate plaintext fixed-page reads with two direct healthy neighbors
    //! qualify. Their distinct destinations are pinned first hops over HTTP. Metadata,
    //! subscription selection, ciphertext relays, native paths and full GETs do not race.
    //! One CopyOnly secondary may start; normal acquisition credits are partitioned,
    //! never replenished. Duplicate escrow is held in addition to actual buffer quota,
    //! intentionally over-accounting rather than requiring a transport-wide reservation
    //! handoff. Admission also preflights two full contender buffers before credits:
    //! the existing serial page plus escrow plus both contenders require 64 MiB of
    //! plaintext capacity in an otherwise empty worker. Low quotas fall back serially.
    //! Pair exchanges have a local one-third-remaining cap; signed authority stays
    //! unchanged. Serial continuation skips the consumed primary, keeps the CopyOnly
    //! secondary eligible for Acquire, and preserves credits for later candidates.
    //! One authorized cold fallback requires six original attempts/ten links; the
    //! standard eight/sixteen allowance can hedge without budget inflation. A second
    //! cold fallback needs additional original credits and is skipped if underfunded.
    //! A valid winner waits for the losing exchange/crypto fence before return:
    //! this can limit the latency benefit and is not an early-publication implementation.
    use crate::{
        error::{Error, Result},
        telemetry::metrics::{Event, Metrics},
    };
    use std::{
        collections::BTreeMap,
        sync::{Arc, Mutex},
        task::{Context, Poll, Waker},
        time::{Duration, Instant},
    };

    pub const DUPLICATE_BYTES: usize = crate::model::PAGE_BYTES as usize * 2 + 16;
    #[derive(Clone, Copy)]
    pub struct Config {
        pub delay: Duration,
        pub slots: usize,
        pub bytes: usize,
    }
    impl Default for Config {
        fn default() -> Self {
            Self {
                delay: Duration::from_millis(100),
                slots: 0,
                bytes: DUPLICATE_BYTES,
            }
        }
    }
    impl Config {
        pub fn validate(self) -> Result<()> {
            if self.slots > 32
                || self.delay.is_zero()
                || self.delay > Duration::from_secs(30)
                || self.bytes > DUPLICATE_BYTES * 32
                || (self.slots > 0 && self.bytes < DUPLICATE_BYTES)
            {
                return Err(Error::InvalidConfiguration);
            }
            Ok(())
        }
    }
    struct Alarm {
        due: Instant,
        wake: Option<Waker>,
    }
    struct State {
        next: u64,
        alarms: BTreeMap<u64, Alarm>,
    }
    pub(crate) struct Hedges {
        config: Config,
        state: Mutex<State>,
        metrics: Metrics,
    }
    pub(crate) struct Permit {
        owner: Arc<Hedges>,
        id: u64,
    }
    impl Hedges {
        pub(crate) fn new(config: Config, metrics: Metrics) -> Result<Arc<Self>> {
            config.validate()?;
            Ok(Arc::new(Self {
                config,
                metrics,
                state: Mutex::new(State {
                    next: 0,
                    alarms: BTreeMap::new(),
                }),
            }))
        }
        pub(crate) fn enabled(&self) -> bool {
            self.config.slots > 0
        }
        pub(crate) fn suppressed(&self) {
            let _ = self.metrics.record(Event::PageHedgeSuppressed, 1);
        }
        pub(crate) fn acquire(self: &Arc<Self>) -> Result<Permit> {
            let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
            if state.alarms.len() >= self.config.slots
                || (state.alarms.len() + 1) * DUPLICATE_BYTES > self.config.bytes
            {
                return Err(Error::Overloaded);
            }
            state.next = state.next.checked_add(1).ok_or(Error::Unavailable)?;
            let id = state.next;
            state.alarms.insert(
                id,
                Alarm {
                    due: crate::runtime::environment::now() + self.config.delay,
                    wake: None,
                },
            );
            Ok(Permit {
                owner: self.clone(),
                id,
            })
        }
        pub(crate) fn poll(&self) {
            let now = crate::runtime::environment::now();
            let wakes: Vec<_> = self
                .state
                .lock()
                .map(|mut state| {
                    state
                        .alarms
                        .values_mut()
                        .filter(|a| now >= a.due)
                        .filter_map(|a| a.wake.take())
                        .collect()
                })
                .unwrap_or_default();
            for wake in wakes {
                wake.wake();
            }
        }
    }
    impl Permit {
        pub(crate) fn delay(&self, cx: &mut Context<'_>) -> Poll<()> {
            let mut state = self.owner.state.lock().expect("hedge alarm lock");
            let alarm = state.alarms.get_mut(&self.id).expect("live hedge alarm");
            if crate::runtime::environment::now() >= alarm.due {
                Poll::Ready(())
            } else {
                alarm.wake = Some(cx.waker().clone());
                Poll::Pending
            }
        }
        pub(crate) fn started(&self) {
            let _ = self.owner.metrics.record(Event::PageHedgeStarted, 1);
            let _ = self
                .owner
                .metrics
                .record(Event::PageHedgeDuplicateBytes, DUPLICATE_BYTES as u64);
        }
        pub(crate) fn won(&self) {
            let _ = self.owner.metrics.record(Event::PageHedgeWon, 1);
        }
    }
    impl Drop for Permit {
        fn drop(&mut self) {
            if let Ok(mut state) = self.owner.state.lock() {
                state.alarms.remove(&self.id);
            }
        }
    }

    /// Validation is inside each future. Cancellation is requested, never mistaken
    /// for completion: even a validated winner waits for the losing exchange fence.
    pub(crate) async fn race<T>(
        primary: impl std::future::Future<Output = Result<T>>,
        secondary: impl std::future::Future<Output = Result<T>>,
        primary_scope: &crate::runtime::deadline::RequestScope,
        secondary_scope: &crate::runtime::deadline::RequestScope,
        parent: &crate::runtime::deadline::RequestScope,
        permit: &Permit,
    ) -> Result<T> {
        parent.check()?;
        let mut primary = Box::pin(primary);
        let mut secondary = Box::pin(secondary);
        let registration = parent.cancellation.subscribe()?;
        let mut a_done = false;
        let mut b_done = false;
        let mut launched = false;
        let mut winner = None;
        let mut fatal = None;
        std::future::poll_fn(|cx| {
            registration.register(cx.waker());
            if let Err(error) = parent.check() {
                fatal = Some(error);
            }
            if fatal.is_some() || winner.is_some() {
                if !a_done {
                    let _ = primary_scope.cancel();
                }
                if !b_done {
                    let _ = secondary_scope.cancel();
                }
            }
            if !a_done {
                if let Poll::Ready(result) = primary.as_mut().poll(cx) {
                    a_done = true;
                    match result {
                        Ok(value) if winner.is_none() && fatal.is_none() => winner = Some(value),
                        Err(error)
                            if !recoverable(error) && winner.is_none() && fatal.is_none() =>
                        {
                            fatal = Some(error)
                        }
                        _ => {}
                    }
                    if !launched {
                        b_done = true;
                    }
                }
            }
            if !launched
                && !b_done
                && fatal.is_none()
                && winner.is_none()
                && permit.delay(cx).is_ready()
            {
                launched = true;
            }
            if launched && !b_done {
                if fatal.is_some() || winner.is_some() {
                    let _ = secondary_scope.cancel();
                }
                if let Poll::Ready(result) = secondary.as_mut().poll(cx) {
                    b_done = true;
                    match result {
                        Ok(value) if winner.is_none() && fatal.is_none() => {
                            permit.won();
                            winner = Some(value);
                        }
                        Err(error)
                            if !recoverable(error) && winner.is_none() && fatal.is_none() =>
                        {
                            fatal = Some(error)
                        }
                        _ => {}
                    }
                }
            }
            if winner.is_some() || fatal.is_some() {
                if !a_done {
                    let _ = primary_scope.cancel();
                }
                if !b_done {
                    let _ = secondary_scope.cancel();
                }
                if !launched {
                    b_done = true;
                }
            }
            if a_done && b_done {
                Poll::Ready(if let Some(error) = fatal {
                    Err(error)
                } else {
                    winner.take().ok_or(Error::Unavailable)
                })
            } else {
                Poll::Pending
            }
        })
        .await
    }
    fn recoverable(error: Error) -> bool {
        matches!(
            error,
            Error::Unavailable | Error::Io | Error::Overloaded | Error::CorruptRecord | Error::MissingKey | Error::Cancelled
        // Malformed framing is local to that contender, not authority to
        // cancel another independently authenticated usable page.
        | Error::InvalidRequest | Error::HeaderTooLarge
        )
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::{
            model::RequestId,
            runtime::{
                deadline::RequestScope,
                environment::{SimulationClock, now},
            },
        };
        use std::{cell::Cell, future::Future, rc::Rc};
        fn scope() -> RequestScope {
            RequestScope::new(RequestId([1; 16]), now() + Duration::from_secs(10)).unwrap()
        }
        fn controller(slots: usize, bytes: usize) -> Arc<Hedges> {
            Hedges::new(
                Config {
                    slots,
                    bytes,
                    delay: Duration::from_millis(10),
                },
                Metrics::default(),
            )
            .unwrap()
        }
        #[test]
        fn malformed_speculative_http_and_wire_heads_do_not_veto_valid_primary() {
            for kind in ["invalid", "large", "wire"] {
                for same_poll in [false, true] {
                    let clock = SimulationClock::new(933);
                    let _env = clock.environment(0).enter();
                    let owner = controller(1, DUPLICATE_BYTES);
                    let permit = owner.acquire().unwrap();
                    let a = scope();
                    let b = scope();
                    let parent = scope();
                    let ready = Cell::new(false);
                    let malformed_ready = Cell::new(false);
                    let primary = std::future::poll_fn(|_| {
                        if ready.get() {
                            Poll::Ready(Ok(42))
                        } else {
                            Poll::Pending
                        }
                    });
                    let secondary = async {
                        std::future::poll_fn(|_| {
                            if malformed_ready.get() {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        })
                        .await;
                        let codec =
                            crate::http::Codec::new(if kind == "large" { 8 } else { 4096 }, 0);
                        let bytes: &[u8] = if kind == "invalid" {
                            b"not-http\r\n\r\n"
                        } else {
                            b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
                        };
                        let parsed = codec.decode_head(bytes);
                        let error = match parsed {
                            Err(e) => e,
                            Ok(Some((head, _))) => {
                                crate::peer::protocol::decode_envelope(head, true)
                                    .err()
                                    .expect("missing signed wire envelope")
                            }
                            _ => panic!("complete malformed frame"),
                        };
                        assert!(matches!(
                            error,
                            Error::InvalidRequest | Error::HeaderTooLarge
                        ));
                        Err(error)
                    };
                    let mut work = Box::pin(race(primary, secondary, &a, &b, &parent, &permit));
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    assert!(work.as_mut().poll(&mut cx).is_pending());
                    clock.advance(Duration::from_millis(10));
                    // Start both contenders and park them before the readiness turn.
                    assert!(work.as_mut().poll(&mut cx).is_pending());
                    malformed_ready.set(true);
                    if same_poll {
                        ready.set(true);
                    }
                    let result = work.as_mut().poll(&mut cx);
                    if same_poll {
                        assert_eq!(result, Poll::Ready(Ok(42)));
                    } else {
                        assert!(result.is_pending());
                        assert!(!a.cancellation.is_cancelled());
                        ready.set(true);
                        assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(Ok(42)));
                    }
                }
            }
        }
        #[test]
        fn fast_primary_never_launches_secondary_or_records_duplicate_bytes() {
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let result = futures::executor::block_on(race(
                async { Ok(1) },
                async {
                    panic!("unexpected speculative launch");
                    #[allow(unreachable_code)]
                    Ok(2)
                },
                &a,
                &b,
                &parent,
                &permit,
            ));
            assert_eq!(result, Ok(1));
            assert_eq!(owner.metrics.count(Event::PageHedgeStarted), 0);
            assert_eq!(owner.metrics.count(Event::PageHedgeDuplicateBytes), 0);
        }
        #[test]
        fn canceled_parent_never_polls_unlaunched_attempts() {
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            parent.cancel().unwrap();
            let never = || async {
                panic!("canceled parent submitted work");
                #[allow(unreachable_code)]
                Ok::<_, Error>(())
            };
            assert_eq!(
                futures::executor::block_on(race(never(), never(), &a, &b, &parent, &permit)),
                Err(Error::Cancelled)
            );
        }
        #[test]
        fn worker_owned_race_keeps_permit_after_caller_detaches_until_loser_fence() {
            let clock = SimulationClock::new(906);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let queue = Rc::new(super::super::drivers::DriverQueue::default());
            let _guard = queue.enter();
            let (send, receive) = futures::channel::oneshot::channel();
            let fence = Rc::new(Cell::new(false));
            let primary_fence = fence.clone();
            let completion = Rc::new(Cell::new(false));
            let done = completion.clone();
            super::super::drivers::spawn(Box::pin(async move {
                let a = scope();
                let b = scope();
                let parent = scope();
                let primary = std::future::poll_fn(|cx| {
                    if primary_fence.get() {
                        Poll::Ready(Err(Error::Cancelled))
                    } else {
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                });
                let result = race(primary, async { Ok(3) }, &a, &b, &parent, &permit).await;
                drop(permit);
                done.set(true);
                let _ = send.send(result);
                Ok(())
            }))
            .unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            queue.poll(&mut cx, 4);
            clock.advance(Duration::from_millis(10));
            queue.poll(&mut cx, 4);
            drop(receive);
            assert!(!completion.get());
            assert!(matches!(owner.acquire(), Err(Error::Overloaded)));
            fence.set(true);
            queue.poll(&mut cx, 4);
            assert!(completion.get());
            assert!(owner.acquire().is_ok());
            assert_eq!(queue.pending(), 0);
        }
        #[test]
        fn shared_slots_bytes_and_alarm_wake_are_bounded() {
            let clock = SimulationClock::new(901);
            let _env = clock.environment(0).enter();
            let owner = controller(2, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            assert!(matches!(owner.clone().acquire(), Err(Error::Overloaded)));
            let wake = std::sync::Arc::new(crate::test_support::WakeCounter::default());
            let waker = std::task::Waker::from(wake.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(permit.delay(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            owner.poll();
            assert_eq!(wake.count(), 1);
            owner.poll();
            assert_eq!(wake.count(), 1);
            assert!(permit.delay(&mut cx).is_ready());
            drop(permit);
            assert!(owner.acquire().is_ok());
            assert!(
                !Hedges::new(Config::default(), Metrics::default())
                    .unwrap()
                    .enabled()
            );
        }
        #[test]
        fn validated_secondary_waits_for_primary_fence_and_keeps_slot() {
            let clock = SimulationClock::new(902);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let fence = Rc::new(Cell::new(false));
            let primary = std::future::poll_fn(|_| {
                if fence.get() {
                    Poll::Ready(Err(Error::Cancelled))
                } else {
                    Poll::Pending
                }
            });
            let mut race = Box::pin(race(primary, async { Ok(42) }, &a, &b, &parent, &permit));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(race.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            assert!(race.as_mut().poll(&mut cx).is_pending());
            assert!(a.cancellation.is_cancelled());
            assert!(!parent.cancellation.is_cancelled());
            assert!(matches!(owner.acquire(), Err(Error::Overloaded)));
            fence.set(true);
            assert_eq!(race.as_mut().poll(&mut cx), Poll::Ready(Ok(42)));
            drop(race);
            drop(permit);
            assert!(owner.acquire().is_ok());
        }
        #[test]
        fn invalid_fast_copy_does_not_displace_valid_slow_primary() {
            let clock = SimulationClock::new(903);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let ready = Cell::new(false);
            let primary = std::future::poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Ok(7))
                } else {
                    Poll::Pending
                }
            });
            let mut race = Box::pin(race(
                primary,
                async { Err(Error::CorruptRecord) },
                &a,
                &b,
                &parent,
                &permit,
            ));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(race.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            assert!(race.as_mut().poll(&mut cx).is_pending());
            assert!(!a.cancellation.is_cancelled());
            ready.set(true);
            assert_eq!(race.as_mut().poll(&mut cx), Poll::Ready(Ok(7)));
            assert_eq!(owner.metrics.count(Event::PageHedgeWon), 0);
        }
        #[test]
        fn parent_cancellation_drains_both_and_never_returns_a_winner() {
            let clock = SimulationClock::new(904);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let fence = Cell::new(false);
            let child = || {
                std::future::poll_fn(|_| {
                    if fence.get() {
                        Poll::Ready(Ok(9))
                    } else {
                        Poll::Pending
                    }
                })
            };
            let mut race = Box::pin(race(child(), child(), &a, &b, &parent, &permit));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(race.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            assert!(race.as_mut().poll(&mut cx).is_pending());
            parent.cancel().unwrap();
            assert!(race.as_mut().poll(&mut cx).is_pending());
            assert!(a.cancellation.is_cancelled() && b.cancellation.is_cancelled());
            assert!(matches!(owner.acquire(), Err(Error::Overloaded)));
            fence.set(true);
            assert_eq!(
                race.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            );
        }
    }
}
pub mod metadata;
pub mod range_stream;
#[cfg(test)]
mod remote_tests;
pub mod subscription;

use self::{
    fill::Fill,
    flight::AcquisitionBudget,
    metadata::MetadataService,
    range_stream::{RangeStream, RangeStreams},
};
use crate::{
    client::{ClientRequest, ReadKind},
    control::state::SnapshotStore,
    error::{Error, Operation, Result},
    model::{
        ByteRange, MetadataSelector, ObjectId, ObjectMetadata, OriginContext, PeerOriginContext,
        ResolvedRange,
    },
    peer::{
        protocol::{FetchMode, Operation as PeerOperation, PeerResponse, VerifiedRequest},
        server::LocalPageService,
    },
    runtime::deadline::RequestScope,
    security::credentials::{ChargedOriginContext, CredentialCrypto},
    topology::membership::MembershipLease,
};
use std::rc::Rc;

pub struct ReadResponse {
    pub metadata: ObjectMetadata,
    pub range: Option<ResolvedRange>,
    pub body: Option<RangeStream>,
}
pub trait ReadService {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse>;
}
pub struct Coordinator {
    snapshots: Rc<SnapshotStore>,
    pub(super) metadata: Rc<MetadataService>,
    pub(super) fill: Rc<Fill>,
    streams: Rc<RangeStreams>,
    pub(super) credentials: Rc<CredentialCrypto>,
    availability: Rc<crate::control::state::Availability>,
}
// Metadata/bootstrap allowance. Normal pinned client ranges admit bounded page
// acquisitions separately; this is not a ceiling on successful pages delivered.
pub(crate) fn default_budget(scope: &RequestScope) -> AcquisitionBudget {
    AcquisitionBudget::new(scope.deadline.0, 32, 96)
}
pub(crate) fn inherited_budget(
    route: &crate::topology::routing::RouteBudget,
    scope: &RequestScope,
) -> Result<AcquisitionBudget> {
    scope.check()?;
    if route.remaining_links > 8 || route.remaining_links == 0 || route.visited.is_empty() {
        return Err(Error::HopBudgetExhausted);
    }
    let mut budget = AcquisitionBudget::new(
        scope.deadline.0.min(route.deadline.0),
        route.remaining_attempts,
        route
            .remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?,
    );
    if route.remaining_links > 4 {
        budget.note_route_failure();
    }
    Ok(budget)
}
impl Coordinator {
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&std::sync::Arc<hedge::Hedges>> {
        self.fill.hedge_owner()
    }
    pub fn new(
        snapshots: Rc<SnapshotStore>,
        metadata: Rc<MetadataService>,
        fill: Rc<Fill>,
        streams: Rc<RangeStreams>,
        credentials: Rc<CredentialCrypto>,
        availability: Rc<crate::control::state::Availability>,
    ) -> Self {
        Self {
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
            availability,
        }
    }
    pub(crate) fn open_context(&self, envelope: PeerOriginContext) -> Result<ChargedOriginContext> {
        let request = envelope.request;
        let attempt = envelope.attempt;
        self.credentials.open_charged(envelope, request, attempt)
    }
    fn read_budgeted<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
        mut budget: AcquisitionBudget,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            scope.check()?;
            let snapshot = self.snapshots.current()?;
            if !snapshot
                .caches
                .iter()
                .any(|c| c.id == request.origin.object.cache)
                || !self.availability.metadata(&request.origin.object.cache)
            {
                return Err(Error::Unavailable);
            }
            let membership = snapshot.membership.clone();
            let directory = self.streams.directory();
            let ClientRequest { kind, origin } = request;
            match kind {
                ReadKind::Subscription {
                    pin,
                    range,
                    page_credits,
                    byte_credits,
                    ordered,
                } => {
                    let selector = pin.map_or(MetadataSelector::Fresh, MetadataSelector::Pinned);
                    let metadata = directory
                        .resolve_with_budget(
                            selector.clone(),
                            membership.clone(),
                            &origin,
                            scope,
                            &mut budget,
                        )
                        .await?;
                    validate_metadata(&metadata, &origin.object, &selector)?;
                    if metadata.length == 0 && range.is_none() {
                        return Ok(ReadResponse {
                            metadata,
                            range: None,
                            body: None,
                        });
                    }
                    let range =
                        resolve_range(range.unwrap_or(ByteRange::From(0)), metadata.length)?;
                    let mut body = self.streams.open(
                        metadata.clone(),
                        range,
                        origin,
                        membership,
                        scope.clone(),
                    )?;
                    body.configure_subscription(page_credits, byte_credits, ordered)?;
                    Ok(ReadResponse {
                        metadata,
                        range: Some(range),
                        body: Some(body),
                    })
                }
                ReadKind::Head | ReadKind::HeadPinned { .. } => {
                    let selector = match kind {
                        ReadKind::HeadPinned { etag } => MetadataSelector::Pinned(etag),
                        _ => MetadataSelector::Fresh,
                    };
                    let metadata = directory
                        .resolve_with_budget(
                            selector.clone(),
                            membership,
                            &origin,
                            scope,
                            &mut budget,
                        )
                        .await?;
                    validate_metadata(&metadata, &origin.object, &selector)?;
                    Ok(ReadResponse {
                        metadata,
                        range: None,
                        body: None,
                    })
                }
            }
        })
    }
}
impl ReadService for Coordinator {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        self.read_budgeted(request, scope, default_budget(scope))
    }
}
impl LocalPageService for Coordinator {
    fn serve_peer<'a>(
        &'a self,
        verified: VerifiedRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            let request = verified.into_signed().request;
            crate::peer::check_membership(&request, &membership)?;
            if request.route.request != scope.request
                || request.origin.request != request.route.request
                || request.origin.attempt != request.route.attempt
            {
                return Err(Error::Unauthorized);
            }
            let object = match &request.operation {
                PeerOperation::Subscribe { .. } => return Err(Error::InvalidRequest),
                PeerOperation::Bootstrap { object, .. } => object,
                PeerOperation::Page { page, .. } => &page.version.object,
                PeerOperation::Metadata { object, .. } => object,
            };
            if object != &request.origin.object {
                return Err(Error::InvalidRequest);
            }
            if !self
                .snapshots
                .current()?
                .caches
                .iter()
                .any(|c| c.id == object.cache)
                || !self.availability.metadata(&object.cache)
            {
                return Ok(PeerResponse::Miss);
            }
            let mut effective = scope.clone();
            effective.deadline.0 = effective.deadline.0.min(request.route.deadline.0);
            effective.check()?;
            // Copy-only never opens credentials and never invokes acquisition.
            // The ingress lease is still retained while serving those bytes.
            let result = match request.operation {
                PeerOperation::Bootstrap {
                    object,
                    mode: FetchMode::CopyOnly,
                } => {
                    let context = OriginContext {
                        object,
                        metadata: None,
                        authorization: None,
                    };
                    self.metadata.bootstrap_copy(&context, &effective).await
                }
                PeerOperation::Page {
                    page,
                    mode: FetchMode::CopyOnly,
                } => self
                    .fill
                    .copy_only(&page, &effective)
                    .await
                    .and_then(|copy| match copy {
                        Some((metadata, ciphertext)) => {
                            metadata.immutable().validate_page(ciphertext.envelope())?;
                            if ciphertext.envelope().page != page {
                                return Err(Error::CorruptRecord);
                            }
                            Ok(PeerResponse::Page {
                                metadata,
                                ciphertext,
                            })
                        }
                        None => Ok(PeerResponse::Miss),
                    }),
                PeerOperation::Metadata {
                    object,
                    selector,
                    mode: FetchMode::CopyOnly,
                } => {
                    let context = OriginContext {
                        object: object.clone(),
                        metadata: None,
                        authorization: None,
                    };
                    self.metadata
                        .copy_only(selector.clone(), &context, &effective)
                        .await
                        .and_then(|value| match value {
                            Some(metadata) => {
                                validate_metadata(&metadata, &object, &selector)?;
                                Ok(PeerResponse::Metadata(metadata))
                            }
                            None => Ok(PeerResponse::Miss),
                        })
                }
                operation => {
                    let mut budget = inherited_budget(&request.route, &effective)?;
                    let context = self.open_context(request.origin)?;
                    match operation {
                        PeerOperation::Bootstrap {
                            mode: FetchMode::Acquire,
                            ..
                        } => {
                            self.fill.record_peer_bootstrap()?;
                            self.metadata
                                .bootstrap_peer(membership, &context, &effective, &mut budget)
                                .await
                        }
                        PeerOperation::Page {
                            page,
                            mode: FetchMode::Acquire,
                        } => self
                            .fill
                            .acquire_ciphertext(
                                page.clone(),
                                membership,
                                &context,
                                &effective,
                                &mut budget,
                            )
                            .await
                            .and_then(|page_result| {
                                page_result.validate_metadata()?;
                                Ok(PeerResponse::Page {
                                    metadata: page_result.metadata,
                                    ciphertext: page_result.ciphertext,
                                })
                            }),
                        PeerOperation::Metadata {
                            object,
                            selector,
                            mode: FetchMode::Acquire,
                        } => self
                            .metadata
                            .resolve_with_budget(
                                selector.clone(),
                                membership,
                                &context,
                                &effective,
                                &mut budget,
                            )
                            .await
                            .and_then(|metadata| {
                                validate_metadata(&metadata, &object, &selector)?;
                                Ok(PeerResponse::Metadata(metadata))
                            }),
                        _ => Err(Error::InvalidRequest),
                    }
                }
            };
            match result {
                Ok(response) => Ok(response),
                Err(error) => {
                    self.fill
                        .observe_peer_error(&effective, request.route.attempt, error);
                    peer_error(error)
                }
            }
        })
    }
}
fn validate_metadata(
    metadata: &ObjectMetadata,
    object: &ObjectId,
    selector: &MetadataSelector,
) -> Result<()> {
    if &metadata.version.object != object {
        return Err(Error::CorruptRecord);
    }
    if let MetadataSelector::Pinned(etag) = selector {
        if &metadata.version.etag != etag {
            return Err(Error::VersionUnavailable);
        }
    }
    Ok(())
}
fn resolve_range(range: ByteRange, length: u64) -> Result<ResolvedRange> {
    range.resolve(length).map_err(|error| match error {
        Error::UnsatisfiableRange => Error::UnsatisfiableRangeWithLength(length),
        other => other,
    })
}
fn peer_error(error: Error) -> Result<PeerResponse> {
    match error {
        Error::NotFound => Ok(PeerResponse::NotFound),
        Error::VersionUnavailable => Ok(PeerResponse::VersionUnavailable),
        Error::Overloaded => Ok(PeerResponse::Overloaded),
        Error::OriginRejected => Ok(PeerResponse::OriginRejected),
        Error::OriginForbidden => Ok(PeerResponse::OriginForbidden),
        Error::Unavailable | Error::HopBudgetExhausted | Error::DeadlineExceeded => {
            Ok(PeerResponse::Unavailable)
        }
        error => Err(error),
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::AttemptId;
    #[test]
    fn remote_budget_charges_final_incoming_link_and_never_restores_attempts() {
        let now = crate::runtime::environment::now();
        let scope = RequestScope::new(
            crate::model::RequestId([1; 16]),
            now + std::time::Duration::from_secs(60),
        )
        .unwrap();
        let mut route = crate::topology::routing::RouteBudget {
            membership: crate::model::MembershipVersion(1),
            request: scope.request,
            attempt: AttemptId([2; 16]),
            destination: crate::model::NodeId("destination".into()),
            visited: vec![crate::model::NodeId("sender".into())],
            remaining_links: 1,
            remaining_attempts: 0,
            deadline: scope.deadline,
        };
        let mut budget = inherited_budget(&route, &scope).unwrap();
        assert_eq!(budget.remaining_links(), 0);
        assert_eq!(
            budget.begin_attempt(now, scope.deadline.0),
            Err(Error::Unavailable)
        );
        route.remaining_links = 8;
        route.remaining_attempts = 3;
        route.deadline.0 = now + std::time::Duration::from_secs(5);
        let mut budget = inherited_budget(&route, &scope).unwrap();
        assert_eq!(budget.route_links(), 7);
        assert_eq!(
            budget.begin_attempt(now, scope.deadline.0),
            Ok(route.deadline.0)
        );
        assert_eq!(budget.remaining_attempts(), 2);
        route.remaining_links = 0;
        assert!(matches!(
            inherited_budget(&route, &scope),
            Err(Error::HopBudgetExhausted)
        ));
    }
    #[test]
    fn peer_failures_are_not_copy_misses() {
        assert!(matches!(
            peer_error(Error::NotFound),
            Ok(PeerResponse::NotFound)
        ));
        assert!(matches!(
            peer_error(Error::VersionUnavailable),
            Ok(PeerResponse::VersionUnavailable)
        ));
        assert!(matches!(
            peer_error(Error::OriginRejected),
            Ok(PeerResponse::OriginRejected)
        ));
        assert!(matches!(
            peer_error(Error::Unauthorized),
            Err(Error::Unauthorized)
        ));
        assert!(matches!(
            peer_error(Error::OriginForbidden),
            Ok(PeerResponse::OriginForbidden)
        ));
        assert!(matches!(
            peer_error(Error::Overloaded),
            Ok(PeerResponse::Overloaded)
        ));
        assert!(matches!(
            peer_error(Error::CorruptRecord),
            Err(Error::CorruptRecord)
        ));
    }
}
