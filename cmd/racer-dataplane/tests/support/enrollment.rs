use super::dataplane;
use dataplane::{
    error::Operation,
    model::Limits,
    runtime::{admission::Admission, reactor::Reactor},
};
use std::{
    num::NonZeroUsize,
    rc::Rc,
    time::{Duration, Instant},
};

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

pub fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> dataplane::error::Result<T> {
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

pub fn read_head(stream: &mut impl std::io::Read) -> std::io::Result<String> {
    let mut head = Vec::new();
    while !head.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        stream.read_exact(&mut byte)?;
        head.push(byte[0]);
        assert!(head.len() <= 32768, "oversized fixture head");
    }
    Ok(String::from_utf8(head).unwrap())
}
pub fn fields(head: &str) -> std::collections::BTreeMap<String, String> {
    head.lines()
        .skip(1)
        .filter_map(|line| line.split_once(':'))
        .map(|(key, value)| (key.to_ascii_lowercase(), value.trim().to_owned()))
        .collect()
}
