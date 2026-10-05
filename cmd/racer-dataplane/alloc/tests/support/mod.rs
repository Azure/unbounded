//! Shared fixtures compiled by both slab unit tests and public workflow tests.

use super::Charge;
use std::{cell::Cell, rc::Rc};

pub(crate) struct CountingCharge {
    used: Rc<Cell<usize>>,
    bytes: usize,
}

impl CountingCharge {
    pub(crate) fn new(used: &Rc<Cell<usize>>, bytes: usize) -> Self {
        used.set(used.get() + bytes);
        Self {
            used: used.clone(),
            bytes,
        }
    }
}

impl Charge for CountingCharge {
    fn covers(&self, bytes: usize) -> bool {
        self.bytes >= bytes
    }
}

impl Drop for CountingCharge {
    fn drop(&mut self) {
        self.used.set(self.used.get() - self.bytes);
    }
}

#[cfg(feature = "simulation")]
pub(crate) mod simulated {
    use super::super::Error;
    use std::{
        future::Future,
        pin::Pin,
        task::{Context, Poll, Waker},
    };
    use uring_runtime::{Scope, reactor::Reactor};

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    pub(crate) enum TestError {
        Alloc(Error),
        Runtime(uring_runtime::Error),
    }

    impl From<Error> for TestError {
        fn from(error: Error) -> Self {
            Self::Alloc(error)
        }
    }

    impl From<uring_runtime::Error> for TestError {
        fn from(error: uring_runtime::Error) -> Self {
            Self::Runtime(error)
        }
    }

    #[derive(Clone)]
    pub(crate) struct TestScope;

    impl Scope for TestScope {
        type Error = TestError;

        fn check(&self) -> Result<(), TestError> {
            Ok(())
        }
    }

    pub(crate) fn poll<T>(future: &mut Pin<Box<dyn Future<Output = T> + '_>>) -> Poll<T> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(Waker::noop()))
    }

    pub(crate) fn drive<T>(
        reactor: &Reactor<TestScope, ()>,
        mut operation: Pin<Box<dyn Future<Output = T> + '_>>,
    ) -> T {
        for _ in 0..100 {
            if let Poll::Ready(value) = poll(&mut operation) {
                return value;
            }
            reactor.poll_budgeted(64).unwrap();
        }
        panic!("simulation did not complete in 100 turns");
    }
}
