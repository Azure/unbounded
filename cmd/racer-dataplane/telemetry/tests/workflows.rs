//! End-to-end diagnostic ownership, formatting failure, and request-boundary workflows.

use std::{
    cell::RefCell,
    fmt,
    io::{Read, Write},
    net::{SocketAddr, TcpListener, TcpStream},
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};
use telemetry::{
    Lease, Metrics, Ring,
    server::{self, Event, Handler, Response, Server},
};
use uring_runtime::{Error, Operation, Scope, environment::Cancellation, reactor::Reactor};

telemetry::metrics! { Counter, COUNTERS, COUNTER_COUNT;
    Self::Completed => "completed_total",
}

telemetry::metrics! { Gauge, GAUGES, GAUGE_COUNT;
    Self::Active => "active",
}

/// Shared metrics used by both worker lifecycle and HTTP diagnostics tests.
type Observations = Metrics<Counter, Gauge>;

/// Retain metrics and bounded record snapshots after worker handles have exited.
#[test]
fn worker_exit_preserves_metrics_and_bounded_diagnostic_snapshots() {
    let workers = Observations::shards(2);
    let reader = workers[0].clone();
    let mut ring = Ring::<u8, 3>::default();
    assert!(ring.is_empty());
    assert_eq!(ring.total(), 0);
    assert_eq!(ring.iter().len(), 0);

    // Worker handles exit while their in-flight operation guards remain alive.
    let leases: Vec<_> = workers
        .into_iter()
        .enumerate()
        .map(|(index, worker)| {
            let (lease, record) = std::thread::spawn(move || {
                let lease = worker.lease(Gauge::Active).unwrap();
                worker.add(Counter::Completed, 1);
                (lease, (index as u8 + 1) * 10)
            })
            .join()
            .unwrap();
            ring.push(record);
            lease
        })
        .collect();
    assert_eq!(reader.count(Counter::Completed), 2);
    assert_eq!(reader.gauge(Gauge::Active), 2);
    assert_eq!(ring.iter().collect::<Vec<_>>(), [(1, 10), (2, 20)]);

    ring.push(30);
    let snapshot = ring.clone();
    ring.push(40);
    ring.push(50);
    assert_eq!(ring.total(), 5);
    assert_eq!(ring.len(), 3);
    assert_eq!(ring.iter().collect::<Vec<_>>(), [(3, 30), (4, 40), (5, 50)]);
    assert_eq!(
        snapshot.iter().collect::<Vec<_>>(),
        [(1, 10), (2, 20), (3, 30)]
    );

    let mut scrape = String::new();
    reader.write_prometheus(&mut scrape).unwrap();
    assert_eq!(
        scrape,
        "# TYPE completed_total counter\ncompleted_total 2\n# TYPE active gauge\nactive 2\n"
    );
    std::thread::spawn(move || drop(leases)).join().unwrap();
    scrape.clear();
    reader.write_prometheus(&mut scrape).unwrap();
    assert_eq!(
        scrape,
        "# TYPE completed_total counter\ncompleted_total 2\n# TYPE active gauge\nactive 0\n"
    );
}

/// A cancellable request scope whose deadline can only become earlier.
#[derive(Clone)]
struct RequestScope {
    deadline: Instant,

    cancellation: Cancellation,
}

impl Scope for RequestScope {
    type Error = Error;

    /// Report cancellation before checking the deadline.
    fn check(&self) -> Result<(), Error> {
        if self.cancellation.is_cancelled() {
            Err(Error::Cancelled)
        } else if Instant::now() >= self.deadline {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }

    /// Share cancellation with pending reactor operations.
    fn cancellation(&self) -> Option<&Cancellation> {
        Some(&self.cancellation)
    }
}

impl server::Scope for RequestScope {
    /// Preserve cancellation and choose the earlier deadline.
    fn with_deadline(&self, deadline: Instant) -> Self {
        Self {
            deadline: self.deadline.min(deadline),
            ..self.clone()
        }
    }
}

/// Application adapter exposing metrics and deliberately failing output routes.
struct Diagnostics {
    metrics: Observations,

    events: RefCell<Vec<Event>>,
}

impl Handler for Diagnostics {
    type Connection = Lease;

    /// Count each admitted connection until its reactor-owned buffer is released.
    fn connect(&self) -> Option<Lease> {
        self.metrics.lease(Gauge::Active)
    }

    /// Format trusted metrics or exercise bounded-output failures.
    fn get(&self, path: &str, mut out: &mut dyn fmt::Write) -> Result<Response, fmt::Error> {
        match path {
            "/fail" => {
                out.write_str("private partial output")?;
                Err(fmt::Error)
            }
            "/overflow" => {
                out.write_str(&"x".repeat(server::MAX_RESPONSE_BYTES))?;
                Ok(Response::Text)
            }
            "/metrics" => {
                self.metrics.write_prometheus(&mut out)?;
                Ok(Response::Metrics)
            }
            _ => Ok(Response::NotFound),
        }
    }

    /// Record fixed transport events in observation order.
    fn observe(&self, event: Event) {
        self.events.borrow_mut().push(event);
    }
}

/// Send one ordinary GET and collect its response through connection close.
fn exchange(
    address: SocketAddr,
    path: &str,
    service: &mut Operation<'_, (), Error>,
    reactor: &Reactor<RequestScope, ()>,
) -> Vec<u8> {
    exchange_request(
        address,
        format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes(),
        service,
        reactor,
    )
}

/// Send bounded raw request bytes while explicitly polling the service and reactor.
fn exchange_request(
    address: SocketAddr,
    request: &[u8],
    service: &mut Operation<'_, (), Error>,
    reactor: &Reactor<RequestScope, ()>,
) -> Vec<u8> {
    let mut socket = TcpStream::connect_timeout(&address, Duration::from_secs(1)).unwrap();
    socket
        .set_write_timeout(Some(Duration::from_secs(1)))
        .unwrap();
    socket.write_all(request).unwrap();
    socket.set_nonblocking(true).unwrap();
    let deadline = Instant::now() + Duration::from_secs(3);
    let mut cx = Context::from_waker(Waker::noop());
    let mut response = Vec::new();
    loop {
        assert!(Instant::now() < deadline, "request exchange stalled");
        assert!(service.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
        let mut bytes = [0; 1024];
        match socket.read(&mut bytes) {
            Ok(0) => return response,
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("request exchange: {e}"),
        }
    }
}

/// Output failure must close only that connection without sending private partial text.
#[test]
fn failed_and_oversized_handler_output_closes_only_that_connection() {
    let reactor = Reactor::<RequestScope, ()>::new(8, ());
    let submissions = reactor
        .reserve_submissions(server::CONTROL_SLOTS, ())
        .unwrap();
    let owner = Server::new(submissions, ());
    let handler = Diagnostics {
        metrics: Observations::shards(1).pop().unwrap(),
        events: RefCell::new(Vec::new()),
    };
    let scope = RequestScope {
        deadline: Instant::now() + Duration::from_secs(15),
        cancellation: Cancellation::new().unwrap(),
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let mut service = owner.serve(&reactor, listener.into(), &handler, &scope);

    for path in ["/fail", "/overflow"] {
        // No partial header or body is sent when formatting fails.
        assert!(exchange(address, path, &mut service, &reactor).is_empty());
        assert_eq!(handler.metrics.gauge(Gauge::Active), 0);
    }
    handler.metrics.add(Counter::Completed, 7);
    let response = exchange(address, "/metrics", &mut service, &reactor);
    let body = "# TYPE completed_total counter\ncompleted_total 7\n# TYPE active gauge\nactive 1\n";
    assert_eq!(
        String::from_utf8(response).unwrap(),
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain; version=0.0.4; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\n\r\n{body}",
            body.len()
        )
    );
    assert_eq!(handler.metrics.gauge(Gauge::Active), 0);
    assert_eq!(
        *handler.events.borrow(),
        [
            Event::Accepted,
            Event::IoError,
            Event::Accepted,
            Event::IoError,
            Event::Accepted
        ]
    );

    scope.cancellation.cancel().unwrap();
    let mut cx = Context::from_waker(Waker::noop());
    assert_eq!(
        service.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    );
    drop(service);
    drop(owner);
    drain(&reactor);
}

/// A complete header at the byte limit is valid; the same full buffer without its
/// terminator is rejected, and both exchanges release their connection lease.
#[test]
fn exact_request_limit_distinguishes_complete_and_unterminated_headers() {
    let reactor = Reactor::<RequestScope, ()>::new(8, ());
    let submissions = reactor
        .reserve_submissions(server::CONTROL_SLOTS, ())
        .unwrap();
    let owner = Server::new(submissions, ());
    let handler = Diagnostics {
        metrics: Observations::shards(1).pop().unwrap(),
        events: RefCell::new(Vec::new()),
    };
    let scope = RequestScope {
        deadline: Instant::now() + Duration::from_secs(15),
        cancellation: Cancellation::new().unwrap(),
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let mut service = owner.serve(&reactor, listener.into(), &handler, &scope);
    let mut request = b"GET /metrics HTTP/1.1\r\nHost: local\r\nAuthorization: ".to_vec();
    request.resize(server::MAX_REQUEST_BYTES - 4, b'x');
    request.extend_from_slice(b"\r\n\r\n");
    assert_eq!(request.len(), server::MAX_REQUEST_BYTES);

    let body = "# TYPE completed_total counter\ncompleted_total 0\n# TYPE active gauge\nactive 1\n";
    let expected = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: text/plain; version=0.0.4; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\n\r\n{body}",
        body.len()
    );
    assert_eq!(
        exchange_request(address, &request, &mut service, &reactor),
        expected.as_bytes()
    );
    assert_eq!(handler.metrics.gauge(Gauge::Active), 0);

    request[server::MAX_REQUEST_BYTES - 4..].fill(b'x');
    assert_eq!(
        exchange_request(address, &request, &mut service, &reactor),
        b"HTTP/1.1 431 Request Header Fields Too Large\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 18\r\nConnection: close\r\nCache-Control: no-store\r\n\r\nheaders too large\n"
    );
    assert_eq!(handler.metrics.gauge(Gauge::Active), 0);

    // The rejected full buffer must not poison subsequent response formatting.
    assert_eq!(
        exchange(address, "/metrics", &mut service, &reactor),
        expected.as_bytes()
    );
    assert_eq!(handler.metrics.gauge(Gauge::Active), 0);
    assert_eq!(
        *handler.events.borrow(),
        [
            Event::Accepted,
            Event::Accepted,
            Event::Rejected,
            Event::Accepted
        ]
    );
    scope.cancellation.cancel().unwrap();
    let mut cx = Context::from_waker(Waker::noop());
    assert_eq!(
        service.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    );
    drop(service);
    drop(owner);
    drain(&reactor);
}

/// Fence every abandoned operation after releasing the service and its owner.
fn drain(reactor: &Reactor<RequestScope, ()>) {
    let mut cx = Context::from_waker(Waker::noop());
    let mut drain = reactor.drain();
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < deadline, "reactor drain stalled");
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}
