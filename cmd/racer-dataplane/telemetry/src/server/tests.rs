use super::*;
use std::cell::RefCell;

#[derive(Default)]
struct Routes(RefCell<Vec<Event>>);
impl Handler for Routes {
    type Connection = ();
    fn connect(&self) -> Option<()> {
        Some(())
    }
    fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error> {
        match path {
            "/live" => {
                out.write_str("ok\n")?;
                Ok(Response::Text)
            }
            "/stats" => {
                out.write_str("example_total 1\n")?;
                Ok(Response::Metrics)
            }
            "/ready" => {
                out.write_str("not ready\n")?;
                Ok(Response::Unavailable)
            }
            "/overflow" => {
                for _ in 0..MAX_RESPONSE_BYTES {
                    out.write_char('x')?;
                }
                Ok(Response::Text)
            }
            _ => {
                out.write_str("discarded secret")?;
                Ok(Response::NotFound)
            }
        }
    }
    fn observe(&self, event: Event) {
        self.0.borrow_mut().push(event);
    }
}

#[test]
fn fixed_parser_restrictions_and_header_limit() {
    for request in [
        "GET /live HTTP/1.0\r\n\r\n",
        "GET /live HTTP/1.1\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost:\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost: a\r\nhOsT: b\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\ncontent-length: 0\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 00\r\n\r\n",
        "GET /live HTTP/1.1\r\nHost: a\r\n",
    ] {
        assert_eq!(parse(request.as_bytes()), Route::BadRequest, "{request:?}");
    }
    let many = format!(
        "GET /live HTTP/1.1\r\nHost: a\r\n{}\r\n",
        "X: a\r\n".repeat(16)
    );
    assert_eq!(parse(many.as_bytes()), Route::BadRequest);
    assert_eq!(
        parse(b"POST /live HTTP/1.1\r\nHost: a\r\n\r\n"),
        Route::Method
    );
    assert_eq!(
        parse(
            b"GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\nAuthorization: secret\r\n\r\n"
        ),
        Route::Get("/live")
    );
}

#[test]
fn precise_wire_formats_redaction_and_output_bounds() {
    let handler = Routes::default();
    for (route, status, body, metrics, allow) in [
        (Route::Get("/live"), "200 OK", "ok\n", false, false),
        (
            Route::Get("/stats"),
            "200 OK",
            "example_total 1\n",
            true,
            false,
        ),
        (
            Route::Get("/ready"),
            "503 Service Unavailable",
            "not ready\n",
            false,
            false,
        ),
        (
            Route::Get("/secret"),
            "404 Not Found",
            "not found\n",
            false,
            false,
        ),
        (
            Route::Method,
            "405 Method Not Allowed",
            "method not allowed\n",
            false,
            true,
        ),
        (
            Route::BadRequest,
            "400 Bad Request",
            "bad request\n",
            false,
            false,
        ),
        (
            Route::TooLarge,
            "431 Request Header Fields Too Large",
            "headers too large\n",
            false,
            false,
        ),
    ] {
        let mut bytes = vec![0; MAX_RESPONSE_BYTES];
        let used = respond(&handler, route, &mut bytes).unwrap();
        let content_type = if metrics {
            "text/plain; version=0.0.4; charset=utf-8"
        } else {
            "text/plain; charset=utf-8"
        };
        let allow = if allow { "Allow: GET\r\n" } else { "" };
        assert_eq!(
            std::str::from_utf8(&bytes[..used]).unwrap(),
            format!(
                "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\n{allow}\r\n{body}",
                body.len()
            )
        );
        assert!(!bytes.windows(6).any(|w| w == b"secret"));
    }
    assert_eq!(*handler.0.borrow(), vec![Event::Rejected; 4]);
    for size in [0, 255, 256, 257] {
        assert!(respond(&handler, Route::Get("/live"), &mut vec![0; size]).is_err());
    }
    assert!(
        respond(
            &handler,
            Route::Get("/overflow"),
            &mut vec![0; MAX_RESPONSE_BYTES]
        )
        .is_err()
    );
}

#[derive(Clone)]
struct TestScope {
    deadline: Instant,
    cancellation: uring_runtime::deadline::Cancellation,
}
impl uring_runtime::Scope for TestScope {
    type Error = Error;
    fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            Err(Error::Cancelled)
        } else if environment::now() >= self.deadline {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }
    fn cancellation(&self) -> Option<&uring_runtime::deadline::Cancellation> {
        Some(&self.cancellation)
    }
}
impl Scope for TestScope {
    fn with_deadline(&self, deadline: Instant) -> Self {
        Self {
            deadline: self.deadline.min(deadline),
            ..self.clone()
        }
    }
}
struct Charge(Rc<Cell<usize>>);
impl Charge {
    fn new(count: &Rc<Cell<usize>>) -> Self {
        count.set(count.get() + 1);
        Self(count.clone())
    }
}
impl Drop for Charge {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
struct Tracked {
    routes: Routes,
    connections: Rc<Cell<usize>>,
    deny_next: Cell<bool>,
}
impl Handler for Tracked {
    type Connection = Charge;
    fn connect(&self) -> Option<Charge> {
        if self.deny_next.replace(false) {
            return None;
        }
        Some(Charge::new(&self.connections))
    }
    fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error> {
        self.routes.get(path, out)
    }
    fn observe(&self, event: Event) {
        self.routes.observe(event);
    }
}

#[test]
fn independent_server_dispatch_guard_and_abandoned_buffer_fence() {
    use std::{
        io::{Read, Write as _},
        net::{TcpListener, TcpStream},
        task::{Context, Waker},
    };
    let reactor = Reactor::<TestScope, ()>::new(8, ());
    let memory = Rc::new(Cell::new(0));
    let control = Rc::new(Cell::new(0));
    let connections = Rc::new(Cell::new(0));
    let submissions = reactor
        .reserve_submissions(CONTROL_SLOTS, Charge::new(&control))
        .unwrap();
    let owner = Server::new(submissions, Charge::new(&memory));
    let handler = Tracked {
        routes: Routes::default(),
        connections: connections.clone(),
        deny_next: Cell::new(false),
    };
    let scope = TestScope {
        deadline: environment::now() + Duration::from_secs(10),
        cancellation: uring_runtime::deadline::Cancellation::new().unwrap(),
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let mut server = owner.serve(&reactor, listener.into(), &handler, &scope);
    let mut cx = Context::from_waker(Waker::noop());
    assert!(server.as_mut().poll(&mut cx).is_pending());
    let second = TcpListener::bind("127.0.0.1:0").unwrap();
    let mut duplicate = owner.serve(&reactor, second.into(), &handler, &scope);
    assert_eq!(
        duplicate.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::InvalidConfiguration))
    );
    drop(duplicate);
    let mut socket = TcpStream::connect(address).unwrap();
    socket
        .write_all(b"GET /live HTTP/1.1\r\nHost: local\r\nAuthorization: secret\r\n\r\n")
        .unwrap();
    socket.set_nonblocking(true).unwrap();
    let until = Instant::now() + Duration::from_secs(3);
    let mut response = Vec::new();
    loop {
        assert!(Instant::now() < until);
        assert!(server.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
        let mut bytes = [0; 1024];
        match socket.read(&mut bytes) {
            Ok(0) => break,
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
    }
    assert_eq!(
        std::str::from_utf8(&response).unwrap(),
        "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 3\r\nConnection: close\r\nCache-Control: no-store\r\n\r\nok\n"
    );
    assert_eq!(connections.get(), 0);
    let _slow = TcpStream::connect(address).unwrap();
    let until = Instant::now() + Duration::from_secs(1);
    while connections.get() == 0 {
        assert!(Instant::now() < until);
        assert!(server.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    // The accept creates a buffer; the next poll submits its receive.
    assert!(server.as_mut().poll(&mut cx).is_pending());
    drop(server);
    assert!(!owner.serving.get());
    drop(owner);
    assert_eq!(memory.get(), 1);
    assert_eq!(control.get(), 1);
    assert_eq!(connections.get(), 1);
    let mut drain = reactor.drain();
    let until = Instant::now() + Duration::from_secs(3);
    loop {
        if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < until);
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!(memory.get(), 0);
    assert_eq!(control.get(), 0);
    assert_eq!(connections.get(), 0);
    assert_eq!(*handler.routes.0.borrow(), vec![Event::Accepted; 2]);
}

#[test]
fn refused_connection_preserves_listener_existing_exchange_and_capacity() {
    use std::{
        io::{Read, Write as _},
        net::{TcpListener, TcpStream},
        task::{Context, Waker},
    };
    let reactor = Reactor::<TestScope, ()>::new(8, ());
    let memory = Rc::new(Cell::new(0));
    let control = Rc::new(Cell::new(0));
    let connections = Rc::new(Cell::new(0));
    let submissions = reactor
        .reserve_submissions(CONTROL_SLOTS, Charge::new(&control))
        .unwrap();
    let owner = Server::new(submissions, Charge::new(&memory));
    let handler = Tracked {
        routes: Routes::default(),
        connections: connections.clone(),
        deny_next: Cell::new(false),
    };
    let scope = TestScope {
        deadline: environment::now() + Duration::from_secs(10),
        cancellation: uring_runtime::deadline::Cancellation::new().unwrap(),
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let mut server = owner.serve(&reactor, listener.into(), &handler, &scope);
    let mut cx = Context::from_waker(Waker::noop());
    let until = Instant::now() + Duration::from_secs(1);
    let mut tick = || {
        assert!(Instant::now() < until);
        assert!(server.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };

    let existing = TcpStream::connect(address).unwrap();
    while connections.get() != 1 {
        tick();
    }
    // Submit the existing exchange's receive before refusing another socket.
    tick();
    handler.deny_next.set(true);
    let mut refused = TcpStream::connect(address).unwrap();
    refused.set_nonblocking(true).unwrap();
    while !handler.routes.0.borrow().contains(&Event::Rejected) {
        tick();
    }
    assert_eq!(refused.read(&mut [0; 1]).unwrap(), 0);
    assert!(!handler.deny_next.get());
    assert!(owner.serving.get());
    assert_eq!(owner.resources.active.get(), 1);
    assert_eq!(connections.get(), 1);

    // Fill every connection slot after the refusal to detect leaked capacity.
    let mut sockets = vec![existing];
    for _ in 1..MAX_CONNECTIONS {
        sockets.push(TcpStream::connect(address).unwrap());
    }
    while connections.get() != MAX_CONNECTIONS {
        tick();
    }
    assert_eq!(owner.resources.active.get(), MAX_CONNECTIONS);
    for socket in &mut sockets {
        socket
            .write_all(b"GET /live HTTP/1.1\r\nHost: local\r\n\r\n")
            .unwrap();
        socket.set_nonblocking(true).unwrap();
    }
    for mut socket in sockets {
        let mut response = Vec::new();
        loop {
            tick();
            let mut bytes = [0; 1024];
            match socket.read(&mut bytes) {
                Ok(0) => break,
                Ok(n) => response.extend_from_slice(&bytes[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(e) => panic!("{e}"),
            }
        }
        assert_eq!(
            std::str::from_utf8(&response).unwrap(),
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 3\r\nConnection: close\r\nCache-Control: no-store\r\n\r\nok\n"
        );
    }
    assert_eq!(connections.get(), 0);
    assert_eq!(owner.resources.active.get(), 0);
    let mut expected = vec![Event::Accepted, Event::Accepted, Event::Rejected];
    expected.extend([Event::Accepted; MAX_CONNECTIONS - 1]);
    assert_eq!(*handler.routes.0.borrow(), expected);
    drop(server);
    assert!(!owner.serving.get());
    drop(owner);
    let mut drain = reactor.drain();
    let until = Instant::now() + Duration::from_secs(3);
    loop {
        if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < until);
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!(memory.get(), 0);
    assert_eq!(control.get(), 0);
    assert_eq!(connections.get(), 0);
}
