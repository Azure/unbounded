//! Helper-lane composition for checkpoint validation and encoding.
//!
//! A helper thread already owns the crypto engine of each lane it serves. The
//! control worker's helper also owns that lane's checkpoint encoder, so a
//! publication never starts a thread of its own.

use crate::error::Error;
use crate::error::Operation;
use crate::lifecycle::FailureReporter;
use crate::lifecycle::Park;
use crate::lifecycle::Service;
use crate::runtime::RequestScope;
use crate::store::checkpoint::EncoderPort;
use std::task::Context;
use std::task::Waker;
use std::time::Duration;

/// The services one helper thread drives: the lane's crypto engine and, on
/// the control worker's helper, its checkpoint encoder.
pub(super) struct Helper {
    crypto: Box<dyn Service<RequestScope>>,
    encode: EncoderPort,
}
impl Helper {
    pub(super) fn new(crypto: Box<dyn Service<RequestScope>>, encode: EncoderPort) -> Self {
        Self { crypto, encode }
    }
}
impl Service<RequestScope> for Helper {
    fn set_failure_reporter(&mut self, reporter: FailureReporter<Error>) {
        self.crypto.set_failure_reporter(reporter.clone());
        self.encode.set_failure_reporter(reporter);
    }

    fn waker(&self) -> Result<Waker, Error> {
        self.crypto.waker()
    }

    fn register_driver(&self, waker: &Waker) {
        self.crypto.register_driver(waker);
        self.encode.register_driver(waker);
    }

    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        let Self { crypto, encode } = self;
        Box::pin(async move {
            crypto.start(scope).await?;
            encode.start(scope).await
        })
    }

    fn poll_budgeted(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<(), Error> {
        self.crypto.poll_budgeted(cx, work_budget)?;
        self.encode.poll_budgeted(cx, work_budget)
    }

    fn wait_timeout(&self, maximum: Duration) -> Duration {
        maximum.min(self.crypto.wait_timeout(maximum))
    }

    fn parker(&self) -> Box<dyn Park<Error>> {
        self.crypto.parker()
    }

    /// Both owners keep running to completion: a failed crypto drain must not
    /// skip publishing an accepted encode, and the reverse also holds.
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        let Self { crypto, encode } = self;
        Box::pin(async move {
            let (crypto, encode) = futures::join!(crypto.drain(scope), encode.drain(scope));
            crypto.and(encode)
        })
    }

    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        let Self { crypto, encode } = self;
        Box::pin(async move {
            let (crypto, encode) = futures::join!(crypto.shutdown(scope), encode.shutdown(scope));
            crypto.and(encode)
        })
    }
}
