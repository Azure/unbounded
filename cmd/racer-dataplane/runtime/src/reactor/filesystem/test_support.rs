use super::*;
use crate::reactor::tests::fixtures::Reactor;
use std::time::Instant;

pub(super) fn poll<T, E>(future: &mut Operation<'_, T, E>) -> Poll<Result<T, E>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}

pub(super) fn drive<T, E>(reactor: &Reactor, mut future: Operation<'_, T, E>) -> Result<T, E> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(result) = poll(&mut future) {
            return result;
        }
        assert!(Instant::now() < deadline, "reactor failed to make progress");
        reactor.poll_budgeted(8).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}
