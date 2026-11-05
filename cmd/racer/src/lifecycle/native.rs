//! Lifecycle hosting for services paired with an RDMA native host.
use super::{FailureReporter, Service};
use racer_io::{Operation, Scope};
use rdma_verbs::{Error, NativeHost, WithNative};
use std::task::{Context, Waker};

impl<S: Scope, T: Service<S>, N: NativeHost> Service<S> for WithNative<T, N>
where
    S::Error: From<Error>,
{
    /// Pass through to the inner service.
    fn set_failure_reporter(&mut self, reporter: FailureReporter<S::Error>) {
        self.inner_mut().set_failure_reporter(reporter);
    }

    /// Pass through to the inner service.
    fn waker(&self) -> Result<Waker, S::Error> {
        self.inner().waker()
    }

    /// Register `waker` with both.
    fn register_driver(&self, waker: &Waker) {
        self.inner().register_driver(waker);
        self.native().register_driver(waker);
    }

    /// Drive native activation while the inner service starts.
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.start_alongside(|inner| inner.start(scope))
    }

    /// Poll the inner service, then the native side, each with `budget`.
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<(), S::Error> {
        self.inner_mut().poll_budgeted(cx, budget)?;
        self.native_mut().poll_budgeted(budget).map_err(Into::into)
    }

    /// The earlier of the inner service's and the native side's bounds.
    fn wait_timeout(&self, maximum: std::time::Duration) -> std::time::Duration {
        self.native()
            .wait_timeout(self.inner().wait_timeout(maximum))
    }

    /// Stop the inner service only. Native work keeps going.
    fn stop_admission(&mut self) -> Result<(), S::Error> {
        self.inner_mut().stop_admission()
    }

    /// Drain the native side and the inner service together.
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.close_alongside(|inner| inner.drain(scope))
    }

    /// Close the native side, then the inner service.
    fn close(&mut self) -> Result<(), S::Error> {
        self.native_mut().close();
        self.inner_mut().close()
    }

    /// Drain the native side and fence the inner service together.
    fn fence<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.close_alongside(|inner| inner.fence(scope))
    }

    /// Pass through to the inner service.
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner_mut().shutdown(scope)
    }
}
