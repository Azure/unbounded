// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Generic single-consumer completion state for storage operations.

use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

pub struct Completion<T> {
    inner: Mutex<CompletionState<T>>,
}

struct CompletionState<T> {
    value: Option<T>,
    waker: Option<Waker>,
}

impl<T> Completion<T> {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(CompletionState {
                value: None,
                waker: None,
            }),
        })
    }

    pub fn set(&self, value: T) {
        let waker = {
            let mut state = self.inner.lock().unwrap();
            state.value = Some(value);
            state.waker.take()
        };
        if let Some(waker) = waker {
            waker.wake();
        }
    }

    pub(super) fn wait(self: Arc<Self>) -> CompletionWait<T> {
        CompletionWait { completion: self }
    }
}

pub(super) struct CompletionWait<T> {
    completion: Arc<Completion<T>>,
}

impl<T> Future for CompletionWait<T> {
    type Output = T;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<T> {
        let mut state = self.completion.inner.lock().unwrap();
        if let Some(value) = state.value.take() {
            Poll::Ready(value)
        } else {
            if !state
                .waker
                .as_ref()
                .is_some_and(|waker| waker.will_wake(cx.waker()))
            {
                state.waker = Some(cx.waker().clone());
            }
            Poll::Pending
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::task::{Wake, Waker};

    use super::*;

    struct FlagWake(AtomicBool);

    impl Wake for FlagWake {
        fn wake(self: Arc<Self>) {
            self.0.store(true, Ordering::Relaxed);
        }
    }

    fn poll<T>(wait: &mut CompletionWait<T>, waker: &Waker) -> Poll<T> {
        let mut cx = Context::from_waker(waker);
        Pin::new(wait).poll(&mut cx)
    }

    #[test]
    fn set_before_wait_returns_value() {
        let completion = Completion::new();
        completion.set(7);
        let flag = Arc::new(FlagWake(AtomicBool::new(false)));
        assert_eq!(
            poll(&mut completion.wait(), &Waker::from(flag)),
            Poll::Ready(7)
        );
    }

    #[test]
    fn set_wakes_waiter() {
        let completion = Completion::new();
        let flag = Arc::new(FlagWake(AtomicBool::new(false)));
        let waker = Waker::from(flag.clone());
        let mut wait = completion.clone().wait();
        assert!(poll(&mut wait, &waker).is_pending());

        completion.set(9);
        assert!(flag.0.load(Ordering::Relaxed));
        assert_eq!(poll(&mut wait, &waker), Poll::Ready(9));
    }

    #[test]
    fn completion_can_cross_threads() {
        let completion = Completion::new();
        let producer = completion.clone();
        std::thread::spawn(move || producer.set(11)).join().unwrap();
        let flag = Arc::new(FlagWake(AtomicBool::new(false)));
        assert_eq!(
            poll(&mut completion.wait(), &Waker::from(flag)),
            Poll::Ready(11)
        );
    }
}
