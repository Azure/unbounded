use racer_dataplane::{
    error::Operation,
    model::Limits,
    runtime::{admission::Admission, reactor::Reactor},
};
use std::{num::NonZeroUsize, rc::Rc, time::{Duration, Instant}};

pub fn reactor() -> Rc<Reactor> {
    let n = NonZeroUsize::new(1024 * 1024).unwrap();
    let reactor = Rc::new(Reactor::new(Rc::new(Admission::new(Limits {
        plaintext_bytes: n,
        ciphertext_bytes: n,
        dirty_bytes: n,
        registered_bytes: n,
        request_context_bytes: n,
        flights: n,
        waiters_per_flight: n,
        queue_entries: NonZeroUsize::new(32).unwrap(),
        connections_per_neighbor: n,
        client_connections: n,
        pipes: n,
        range_window_pages: n,
        header_bytes: n,
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    }))));
    reactor.init().unwrap();
    reactor
}

pub fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> racer_dataplane::error::Result<T> {
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        if let std::task::Poll::Ready(result) = future.as_mut().poll(
            &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        ) {
            return result;
        }
        assert!(Instant::now() < deadline, "enrollment reactor stalled");
        if reactor.poll_budgeted(8)? == 0 {
            reactor.wait(Duration::from_millis(1))?;
        }
    }
}
