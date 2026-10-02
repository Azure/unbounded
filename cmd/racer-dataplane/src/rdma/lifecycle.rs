//! Racer request cancellation and crypto-role composition for generic verbs.
use crate::{
    error::{Error, Result},
    runtime::{deadline::RequestScope, worker::CryptoService},
};
#[cfg(test)]
pub use rdma_verbs::simulation;
pub use rdma_verbs::{
    DeviceHandle, Endpoint, IoPort, NativePort, NativeService, QueuePairHandle, Region, Ticket,
    Window, pair,
};
use std::task::{Context, Poll, Waker};

pub(crate) async fn wait<T, E: Into<Error>>(
    scope: &RequestScope,
    mut poll: impl FnMut(&mut Context<'_>) -> Poll<std::result::Result<T, E>>,
) -> Result<T> {
    let cancellation = scope.cancellation.subscribe()?;
    std::future::poll_fn(|cx| {
        cancellation.register(cx.waker());
        scope.check()?;
        poll(cx).map_err(Into::into)
    })
    .await
}

/// Compose around PageCryptoEngine on the existing paired crypto thread.
pub struct WithNative<S> {
    inner: S,
    native: NativeService,
}
impl<S> WithNative<S> {
    pub fn new(inner: S, port: NativePort) -> Self {
        Self {
            inner,
            native: NativeService::new(port),
        }
    }
}
impl<S: CryptoService> CryptoService for WithNative<S> {
    fn register_driver(&self, waker: &Waker) {
        self.inner.register_driver(waker);
        self.native.register_driver(waker);
    }
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        self.inner.start(scope)
    }
    fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        self.inner.poll_budgeted(budget)?;
        self.native.poll_budgeted(budget).map_err(Into::into)
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        Box::pin(async move {
            self.native.close();
            futures::future::poll_fn(|cx| -> Poll<Result<()>> {
                self.native.register_driver(cx.waker());
                self.native.poll_budgeted(1)?;
                if self.native.drained() {
                    Poll::Ready(Ok(()))
                } else {
                    // The worker's bounded tick retries failed native fences.
                    Poll::Pending
                }
            })
            .await?;
            self.inner.drain(scope).await
        })
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        self.inner.shutdown(scope)
    }
}
