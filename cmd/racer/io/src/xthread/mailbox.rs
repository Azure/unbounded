//! One bounded MPSC mailbox with typed request/reply.
//!
//! Every message holds one of `capacity` credits from send until its
//! [`Responder`] is finished (responded, failed, or dropped), so in-flight
//! work, not just queue occupancy, is bounded. A request whose handler drops
//! the responder fails only that call with [`Error::Unavailable`]; it never
//! fails the worker. Abandoning a [`Reply`] does not leak its credit: the
//! credit returns when the handler finishes.
use super::Notify;
use crate::{Error, Result};
use std::{
    collections::VecDeque,
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
};

/// Create a mailbox with `capacity` credits.
pub fn channel<Req, Resp>(capacity: usize) -> Result<(Sender<Req, Resp>, Receiver<Req, Resp>)> {
    if capacity == 0 {
        return Err(Error::InvalidConfiguration);
    }
    let shared = Arc::new(Shared {
        queue: Mutex::new(VecDeque::new()),
        credits: Arc::new(Credits {
            available: AtomicUsize::new(capacity),
            waiters: Mutex::new(Vec::new()),
        }),
        capacity,
        closed: AtomicBool::new(false),
        senders: AtomicUsize::new(1),
        receiver: Mutex::new(None),
        notify: Mutex::new(None),
    });
    Ok((
        Sender {
            shared: shared.clone(),
        },
        Receiver { shared },
    ))
}

struct Shared<Req, Resp> {
    queue: Mutex<VecDeque<Envelope<Req, Resp>>>,

    credits: Arc<Credits>,

    capacity: usize,

    closed: AtomicBool,

    senders: AtomicUsize,

    receiver: Mutex<Option<Waker>>,

    notify: Mutex<Option<Arc<dyn Notify>>>,
}

/// The credit pool, shared with responders.
struct Credits {
    available: AtomicUsize,

    waiters: Mutex<Vec<Waker>>,
}

impl Credits {
    /// Take one credit if available.
    fn acquire(&self) -> bool {
        self.available
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |credits| {
                credits.checked_sub(1)
            })
            .is_ok()
    }

    /// Return one credit and wake credit waiters.
    fn release(&self) {
        self.available.fetch_add(1, Ordering::AcqRel);
        self.wake_all();
    }

    fn wake_all(&self) {
        let waiters = std::mem::take(&mut *lock(&self.waiters));
        for waker in waiters {
            waker.wake();
        }
    }
}

impl<Req, Resp> Shared<Req, Resp> {
    /// Wake the receiver task and any external notifier.
    fn wake_receiver(&self) {
        let waker = lock(&self.receiver).take();
        if let Some(waker) = waker {
            waker.wake();
        }
        let notify = lock(&self.notify).clone();
        if let Some(notify) = notify {
            notify.notify();
        }
    }
}

/// Lock ignoring poisoning (no user code runs under these locks).
fn lock<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

/// A producer handle; clone for more producers.
pub struct Sender<Req, Resp> {
    shared: Arc<Shared<Req, Resp>>,
}

impl<Req, Resp> Clone for Sender<Req, Resp> {
    /// Another producer of the same mailbox.
    fn clone(&self) -> Self {
        self.shared.senders.fetch_add(1, Ordering::AcqRel);
        Self {
            shared: self.shared.clone(),
        }
    }
}

impl<Req, Resp> Drop for Sender<Req, Resp> {
    /// The last producer's drop wakes the receiver so it can observe EOF.
    fn drop(&mut self) {
        if self.shared.senders.fetch_sub(1, Ordering::AcqRel) == 1 {
            self.shared.wake_receiver();
        }
    }
}

/// One reserved slot of mailbox capacity.
pub struct Credit<Req, Resp> {
    shared: Option<Arc<Shared<Req, Resp>>>,
}

impl<Req, Resp> Drop for Credit<Req, Resp> {
    /// Return the reserved credit.
    fn drop(&mut self) {
        if let Some(shared) = self.shared.take() {
            shared.credits.release();
        }
    }
}

impl<Req, Resp> Credit<Req, Resp> {
    /// Enqueue `request` using this credit; the reply resolves when the
    /// handler responds.
    pub fn call(self, request: Req) -> std::result::Result<Reply<Resp>, (Error, Req)> {
        let cell = Arc::new(ReplyCell::new());
        self.enqueue(request, Some(cell.clone()))?;
        Ok(Reply { cell })
    }

    /// Enqueue `request` without a reply.
    pub fn send(self, request: Req) -> std::result::Result<(), (Error, Req)> {
        self.enqueue(request, None)
    }

    fn enqueue(
        mut self,
        request: Req,
        cell: Option<Arc<ReplyCell<Resp>>>,
    ) -> std::result::Result<(), (Error, Req)> {
        let shared = self.shared.take().expect("unused credit");
        let responder = Responder {
            cell,
            credit: Some(shared.credits.clone()),
        };
        if shared.closed.load(Ordering::Acquire) {
            drop(responder);
            return Err((Error::Unavailable, request));
        }
        {
            let mut queue = lock(&shared.queue);
            // Recheck under the queue lock so close() never strands a message.
            if shared.closed.load(Ordering::Acquire) {
                drop(queue);
                drop(responder);
                return Err((Error::Unavailable, request));
            }
            queue.push_back(Envelope { request, responder });
        }
        shared.wake_receiver();
        Ok(())
    }
}

impl<Req, Resp> Sender<Req, Resp> {
    /// Also call `notify` whenever a message arrives or the mailbox closes.
    pub fn set_notify(&self, notify: Arc<dyn Notify>) {
        *lock(&self.shared.notify) = Some(notify);
    }

    /// Reserve a credit without waiting.
    pub fn try_credit(&self) -> Result<Credit<Req, Resp>> {
        if self.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if !self.shared.credits.acquire() {
            return Err(Error::Overloaded);
        }
        Ok(Credit {
            shared: Some(self.shared.clone()),
        })
    }

    /// Reserve a credit, registering `cx` to be woken when one returns.
    pub fn poll_credit(&self, cx: &mut Context<'_>) -> Poll<Result<Credit<Req, Resp>>> {
        match self.try_credit() {
            Err(Error::Overloaded) => (),
            result => return Poll::Ready(result),
        }
        lock(&self.shared.credits.waiters).push(cx.waker().clone());
        // Close the race with a credit returned before registration.
        match self.try_credit() {
            Err(Error::Overloaded) => Poll::Pending,
            result => Poll::Ready(result),
        }
    }

    /// Wait for a credit.
    pub fn credit(&self) -> impl Future<Output = Result<Credit<Req, Resp>>> + '_ {
        std::future::poll_fn(move |cx| self.poll_credit(cx))
    }

    /// Enqueue a request with a reply, failing with [`Error::Overloaded`]
    /// when no credit is free or [`Error::Unavailable`] once closed.
    pub fn try_call(&self, request: Req) -> std::result::Result<Reply<Resp>, (Error, Req)> {
        match self.try_credit() {
            Ok(credit) => credit.call(request),
            Err(error) => Err((error, request)),
        }
    }

    /// Enqueue a request without a reply.
    pub fn try_send(&self, request: Req) -> std::result::Result<(), (Error, Req)> {
        match self.try_credit() {
            Ok(credit) => credit.send(request),
            Err(error) => Err((error, request)),
        }
    }

    /// Credits currently held by unfinished messages.
    pub fn outstanding(&self) -> usize {
        self.shared.capacity - self.shared.credits.available.load(Ordering::Acquire)
    }

    /// Whether the receiver closed the mailbox.
    pub fn is_closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }
}

/// The single consumer.
pub struct Receiver<Req, Resp> {
    shared: Arc<Shared<Req, Resp>>,
}

impl<Req, Resp> Receiver<Req, Resp> {
    /// Take one message without waiting.
    pub fn try_recv(&self) -> Option<Envelope<Req, Resp>> {
        lock(&self.shared.queue).pop_front()
    }

    /// Take one message, or `Ready(None)` once closed (or every sender is
    /// gone) and drained.
    pub fn poll_recv(&self, cx: &mut Context<'_>) -> Poll<Option<Envelope<Req, Resp>>> {
        if let Some(envelope) = self.try_recv() {
            return Poll::Ready(Some(envelope));
        }
        *lock(&self.shared.receiver) = Some(cx.waker().clone());
        if let Some(envelope) = self.try_recv() {
            return Poll::Ready(Some(envelope));
        }
        if self.shared.closed.load(Ordering::Acquire)
            || self.shared.senders.load(Ordering::Acquire) == 0
        {
            return Poll::Ready(self.try_recv());
        }
        Poll::Pending
    }

    /// Wait for the next message.
    pub fn recv(&self) -> impl Future<Output = Option<Envelope<Req, Resp>>> + '_ {
        std::future::poll_fn(move |cx| self.poll_recv(cx))
    }

    /// Register `waker` for the next arrival.
    pub fn register(&self, waker: &Waker) {
        *lock(&self.shared.receiver) = Some(waker.clone());
    }

    /// Reject new messages with [`Error::Unavailable`]; queued messages stay
    /// receivable.
    pub fn close(&self) {
        self.shared.closed.store(true, Ordering::Release);
        self.shared.credits.wake_all();
    }

    /// Queued messages.
    pub fn len(&self) -> usize {
        lock(&self.shared.queue).len()
    }

    /// Whether no message is queued.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

impl<Req, Resp> Drop for Receiver<Req, Resp> {
    /// Close and fail every queued call.
    fn drop(&mut self) {
        self.close();
        let queued = std::mem::take(&mut *lock(&self.shared.queue));
        drop(queued);
    }
}

/// One received message.
pub struct Envelope<Req, Resp> {
    /// The request.
    pub request: Req,

    /// Completes the caller's [`Reply`].
    pub responder: Responder<Resp>,
}

impl<Req, Resp> Envelope<Req, Resp> {
    /// Split into request and responder.
    pub fn into_parts(self) -> (Req, Responder<Resp>) {
        (self.request, self.responder)
    }
}

/// Completes exactly one call. Dropping it unanswered fails the call with
/// [`Error::Unavailable`]. Finishing it returns the message's credit.
pub struct Responder<Resp> {
    cell: Option<Arc<ReplyCell<Resp>>>,

    credit: Option<Arc<Credits>>,
}

impl<Resp> Responder<Resp> {
    /// Whether the caller still waits for the result.
    pub fn is_wanted(&self) -> bool {
        self.cell
            .as_ref()
            .is_some_and(|cell| !lock(&cell.state).abandoned)
    }

    /// Deliver `value`. Returns it back if the caller abandoned the call or
    /// expects no reply.
    pub fn respond(mut self, value: Resp) -> std::result::Result<(), Resp> {
        let result = match self.cell.take() {
            Some(cell) => cell
                .complete(Ok(value))
                .map_err(|value| value.unwrap_or_else(|_| unreachable!("an Ok value comes back"))),
            None => Err(value),
        };
        self.finish();
        result
    }

    /// Fail the call with `error`.
    pub fn fail(mut self, error: Error) {
        if let Some(cell) = self.cell.take() {
            let _ = cell.complete(Err(error));
        }
        self.finish();
    }

    fn finish(&mut self) {
        if let Some(credit) = self.credit.take() {
            credit.release();
        }
    }
}

impl<Resp> Drop for Responder<Resp> {
    /// Fail an unanswered call and return the credit.
    fn drop(&mut self) {
        if let Some(cell) = self.cell.take() {
            let _ = cell.complete(Err(Error::Unavailable));
        }
        self.finish();
    }
}

struct ReplyCell<Resp> {
    state: Mutex<ReplyState<Resp>>,
}

struct ReplyState<Resp> {
    value: Option<Result<Resp>>,

    waker: Option<Waker>,

    abandoned: bool,

    completed: bool,
}

impl<Resp> ReplyCell<Resp> {
    fn new() -> Self {
        Self {
            state: Mutex::new(ReplyState {
                value: None,
                waker: None,
                abandoned: false,
                completed: false,
            }),
        }
    }

    /// Store the result once, or hand it back if abandoned.
    fn complete(&self, value: Result<Resp>) -> std::result::Result<(), Result<Resp>> {
        let mut state = lock(&self.state);
        if state.abandoned || state.completed {
            return Err(value);
        }
        state.completed = true;
        state.value = Some(value);
        let waker = state.waker.take();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
        Ok(())
    }
}

/// The caller's side of one call: a future of the handler's result.
pub struct Reply<Resp> {
    cell: Arc<ReplyCell<Resp>>,
}

impl<Resp> std::fmt::Debug for Reply<Resp> {
    /// Show only whether a result arrived.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Reply")
            .field("completed", &lock(&self.cell.state).completed)
            .finish()
    }
}

impl<Resp> Reply<Resp> {
    /// Take the result if it arrived.
    pub fn try_take(&mut self) -> Option<Result<Resp>> {
        lock(&self.cell.state).value.take()
    }
}

impl<Resp> Future for Reply<Resp> {
    type Output = Result<Resp>;

    /// Ready with the handler's result.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<Resp>> {
        let mut state = lock(&self.cell.state);
        if let Some(value) = state.value.take() {
            return Poll::Ready(value);
        }
        if state.completed {
            return Poll::Ready(Err(Error::Unavailable));
        }
        state.waker = Some(cx.waker().clone());
        Poll::Pending
    }
}

impl<Resp> Drop for Reply<Resp> {
    /// Abandon the call; a late response is handed back to the responder.
    fn drop(&mut self) {
        let mut state = lock(&self.cell.state);
        state.abandoned = true;
        state.waker = None;
        let value = state.value.take();
        drop(state);
        drop(value);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::xthread::tests::WakeCounter;

    fn poll<F: Future + Unpin>(future: &mut F, waker: &Waker) -> Poll<F::Output> {
        Pin::new(future).poll(&mut Context::from_waker(waker))
    }

    #[test]
    fn calls_round_trip_and_credits_return_on_response() {
        let (sender, receiver) = channel::<u32, u32>(2).unwrap();
        let waker = Waker::noop();
        let mut first = sender.try_call(1).unwrap();
        let _second = sender.try_call(2).unwrap();
        assert_eq!(sender.try_call(3).unwrap_err(), (Error::Overloaded, 3));
        assert_eq!(sender.outstanding(), 2);
        assert!(poll(&mut first, waker).is_pending());
        let (request, responder) = receiver.try_recv().unwrap().into_parts();
        assert!(responder.is_wanted());
        responder.respond(request * 10).unwrap();
        assert_eq!(poll(&mut first, waker), Poll::Ready(Ok(10)));
        assert_eq!(sender.outstanding(), 1);
        // The second call's credit stays held until its handler finishes.
        let envelope = receiver.try_recv().unwrap();
        assert_eq!(sender.outstanding(), 1);
        drop(envelope);
        assert_eq!(sender.outstanding(), 0);
    }

    #[test]
    fn dropped_responders_fail_only_their_call() {
        let (sender, receiver) = channel::<&str, ()>(4).unwrap();
        let mut bad = sender.try_call("bad").unwrap();
        let mut good = sender.try_call("good").unwrap();
        drop(receiver.try_recv().unwrap());
        receiver.try_recv().unwrap().responder.respond(()).unwrap();
        assert_eq!(bad.try_take(), Some(Err(Error::Unavailable)));
        assert_eq!(good.try_take(), Some(Ok(())));
        let mut failed = sender.try_call("x").unwrap();
        receiver
            .try_recv()
            .unwrap()
            .responder
            .fail(Error::InvalidInput);
        assert_eq!(failed.try_take(), Some(Err(Error::InvalidInput)));
        assert_eq!(sender.outstanding(), 0);
    }

    #[test]
    fn abandoned_replies_hand_values_back_and_release_credits() {
        let (sender, receiver) = channel::<u8, Vec<u8>>(1).unwrap();
        drop(sender.try_call(1).unwrap());
        assert_eq!(sender.outstanding(), 1, "the handler still owns the work");
        let envelope = receiver.try_recv().unwrap();
        assert!(!envelope.responder.is_wanted());
        assert_eq!(envelope.responder.respond(vec![7]), Err(vec![7]));
        assert_eq!(sender.outstanding(), 0);
        sender.try_send(2).unwrap();
        let envelope = receiver.try_recv().unwrap();
        assert!(!envelope.responder.is_wanted());
        assert_eq!(envelope.responder.respond(vec![]), Err(vec![]));
    }

    #[test]
    fn close_rejects_new_messages_and_keeps_queued_ones() {
        let (sender, receiver) = channel::<u8, u8>(4).unwrap();
        let mut queued = sender.try_call(1).unwrap();
        receiver.close();
        assert!(sender.is_closed());
        assert_eq!(sender.try_call(2).unwrap_err(), (Error::Unavailable, 2));
        let envelope = receiver.try_recv().unwrap();
        envelope.responder.respond(5).unwrap();
        assert_eq!(queued.try_take(), Some(Ok(5)));
        let waker = Waker::noop();
        assert!(matches!(
            receiver.poll_recv(&mut Context::from_waker(waker)),
            Poll::Ready(None)
        ));
        assert_eq!(sender.try_credit().map(|_| ()), Err(Error::Unavailable));
        let queued_on_drop = {
            let (sender, receiver) = channel::<u8, u8>(1).unwrap();
            let reply = sender.try_call(1).unwrap();
            drop(receiver);
            reply
        };
        let mut queued_on_drop = queued_on_drop;
        assert_eq!(queued_on_drop.try_take(), Some(Err(Error::Unavailable)));
    }

    #[test]
    fn credits_wake_waiters_and_senders_notify_from_other_threads() {
        let (sender, receiver) = channel::<u32, u32>(1).unwrap();
        let notify = Arc::new(WakeCounter::default());
        sender.set_notify(notify.clone());
        let counter = Arc::new(WakeCounter::default());
        let waker = Waker::from(counter.clone());
        let held = sender.try_credit().unwrap();
        assert!(
            sender
                .poll_credit(&mut Context::from_waker(&waker))
                .is_pending()
        );
        drop(held);
        assert_eq!(counter.count(), 1);
        let remote = sender.clone();
        std::thread::spawn(move || {
            remote.try_send(9).unwrap();
        })
        .join()
        .unwrap();
        assert_eq!(notify.count(), 1);
        assert_eq!(receiver.try_recv().unwrap().request, 9);
        let receiver_waker = Arc::new(WakeCounter::default());
        receiver.register(&Waker::from(receiver_waker.clone()));
        drop(sender);
        assert_eq!(
            receiver_waker.count(),
            1,
            "the last sender wakes the receiver"
        );
        assert!(matches!(
            receiver.poll_recv(&mut Context::from_waker(Waker::noop())),
            Poll::Ready(None)
        ));
    }

    #[test]
    fn many_producers_deliver_every_call() {
        let producers = if cfg!(miri) { 2 } else { 4 };
        let per = if cfg!(miri) { 8 } else { 500 };
        let (sender, receiver) = channel::<usize, usize>(8).unwrap();
        let threads: Vec<_> = (0..producers)
            .map(|producer| {
                let sender = sender.clone();
                std::thread::spawn(move || {
                    let mut replies = Vec::new();
                    let mut sent = 0;
                    while sent < per {
                        match sender.try_call(producer * per + sent) {
                            Ok(reply) => {
                                replies.push(reply);
                                sent += 1;
                            }
                            Err((Error::Overloaded, _)) => std::thread::yield_now(),
                            Err((error, _)) => panic!("{error}"),
                        }
                    }
                    let mut sum = 0;
                    for mut reply in replies {
                        loop {
                            if let Some(value) = reply.try_take() {
                                sum += value.unwrap();
                                break;
                            }
                            std::thread::yield_now();
                        }
                    }
                    sum
                })
            })
            .collect();
        drop(sender);
        let mut handled = 0;
        while handled < producers * per {
            match receiver.try_recv() {
                Some(envelope) => {
                    let value = envelope.request;
                    envelope.responder.respond(value).unwrap();
                    handled += 1;
                }
                None => std::thread::yield_now(),
            }
        }
        let total: usize = threads.into_iter().map(|t| t.join().unwrap()).sum();
        let n = producers * per;
        assert_eq!(total, n * (n - 1) / 2);
    }
}
