use super::*;
use crate::{
    http::codec::{Codec, Header, MessageHead, StartLine},
    memory::pipe::PipePool,
    model::{RequestId, ResourceClass},
    runtime::{admission::Admission, reactor::Reactor},
};
use std::{
    future::Future,
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    task::{Context, Poll},
    time::Instant,
};

fn pair() -> (TcpStream, TcpStream) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (socket, _) = listener.accept().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
    peer.set_write_timeout(Some(Duration::from_secs(5)))
        .unwrap();
    (socket, peer)
}
fn poll<F: Future + ?Sized>(future: std::pin::Pin<&mut F>) -> Poll<F::Output> {
    future.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
fn drive<T>(reactor: &Reactor, work: impl Future<Output = T>) -> T {
    let mut work = std::pin::pin!(work);
    let until = Instant::now() + Duration::from_secs(8);
    loop {
        if let Poll::Ready(result) = poll(work.as_mut()) {
            return result;
        }
        assert!(Instant::now() < until);
        if reactor.poll_budgeted(128).unwrap() == 0 {
            std::thread::yield_now();
        }
    }
}

// Match the production service's wake-driven FuturesUnordered and reactor wait,
// rather than repeatedly polling a blocked future with a noop waker.
fn drive_worker<T>(reactor: &Reactor, work: impl Future<Output = T>) -> T {
    use futures::{Stream, stream::FuturesUnordered};
    use std::{sync::Arc, task::Wake};
    struct WakeReactor(crate::runtime::reactor::ReactorWake);
    impl Wake for WakeReactor {
        fn wake(self: Arc<Self>) {
            self.0.wake().unwrap();
        }
    }
    let waker = std::task::Waker::from(Arc::new(WakeReactor(reactor.waker().unwrap())));
    let mut active = FuturesUnordered::new();
    active.push(work);
    let until = Instant::now() + Duration::from_secs(8);
    loop {
        reactor.poll_budgeted(64).unwrap();
        if let Poll::Ready(Some(result)) =
            std::pin::Pin::new(&mut active).poll_next(&mut Context::from_waker(&waker))
        {
            return result;
        }
        assert!(Instant::now() < until, "wake-driven relay watchdog");
        reactor.wait(Duration::from_millis(10)).unwrap();
    }
}
struct Fixture {
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    io: HttpIo,
    pipes: PipePool,
    scope: RequestScope,
}
impl Fixture {
    fn new() -> Self {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.pipes = std::num::NonZeroUsize::new(1).unwrap();
        limits.relay_transfers = std::num::NonZeroUsize::new(1).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        Self {
            io: HttpIo::with_admission(
                reactor.clone(),
                Codec::new(4096, crate::model::PAGE_BYTES + 16),
                admission.clone(),
            ),
            pipes: PipePool::new(admission.clone(), reactor.clone()),
            admission,
            reactor,
            scope: RequestScope::new(RequestId([3; 16]), Instant::now() + Duration::from_secs(8))
                .unwrap(),
        }
    }
    fn connections(
        &self,
        length: usize,
    ) -> (ConnectionLease, ConnectionLease, TcpStream, TcpStream) {
        let (source, writer) = pair();
        let (destination, reader) = pair();
        let mut source = ConnectionLease::from_accepted(source.into(), &self.admission).unwrap();
        let mut destination =
            ConnectionLease::from_accepted(destination.into(), &self.admission).unwrap();
        let reservation = Rc::new(
            self.admission
                .reserve(None, ResourceClass::Relay, 1)
                .unwrap(),
        );
        source.relay_reservation = Some(reservation.clone());
        destination.relay_reservation = Some(reservation);
        source.rx_remaining = Some(length as u64);
        source.tx_remaining = Some(0);
        destination.rx_remaining = Some(0);
        destination.tx_remaining = Some(length as u64);
        (source, destination, writer, reader)
    }
    fn drain(&self) {
        drive(&self.reactor, self.reactor.drain()).unwrap();
    }
}
#[test]
fn truncation_after_success_closes_both_without_error_suffix_or_pool_reuse() {
    let f = Fixture::new();
    let (source, mut destination, mut writer, mut reader) = f.connections(1024);
    destination.tx_remaining = None;
    destination = drive(
        &f.reactor,
        f.io.send_head(
            destination,
            MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![Header {
                    name: "Content-Length".into(),
                    value: b"1024".to_vec(),
                }],
            },
            &f.scope,
        ),
    )
    .unwrap()
    .connection;
    writer.write_all(b"short").unwrap();
    writer.shutdown(std::net::Shutdown::Write).unwrap();
    let pipe = f.pipes.acquire().unwrap();
    assert!(matches!(
        drive(
            &f.reactor,
            f.io.relay_body(source, destination, Some(pipe), &f.scope)
        ),
        Err(Error::Io)
    ));
    let mut bytes = Vec::new();
    reader.read_to_end(&mut bytes).unwrap();
    assert_eq!(bytes, b"HTTP/1.1 200 \r\nContent-Length: 1024\r\n\r\nshort");
    assert_eq!(writer.read(&mut [0]).unwrap(), 0);
    f.drain();
    assert_eq!(f.admission.used(ResourceClass::Connection), 0);
    assert_eq!(f.admission.used(ResourceClass::Relay), 0);
}
#[test]
fn stalled_transit_retains_both_connections_pipe_and_relay_until_cancel_fence() {
    for writing in [false, true] {
        for end in ["drop", "cancel", "deadline", "disconnect"] {
            let mut f = Fixture::new();
            if end == "deadline" {
                f.scope.deadline.0 = Instant::now() + Duration::from_millis(40);
            }
            let (source, destination, writer, reader) = f.connections(16 * 1024 * 1024 + 16);
            let pipe = f.pipes.acquire().unwrap();
            let mut held_writer = Some(writer);
            let producer = if writing {
                let mut writer = held_writer.take().unwrap();
                Some(std::thread::spawn(move || {
                    let _ = writer.write_all(&vec![91; 16 * 1024 * 1024 + 16]);
                }))
            } else {
                None
            };
            let mut work = Box::pin(f.io.relay_body(source, destination, Some(pipe), &f.scope));
            assert!(poll(work.as_mut()).is_pending());
            if writing {
                let until = Instant::now() + Duration::from_millis(15);
                while Instant::now() < until {
                    f.reactor.poll_budgeted(128).unwrap();
                    assert!(poll(work.as_mut()).is_pending());
                }
            }
            assert_eq!(f.admission.used(ResourceClass::Connection), 2);
            assert_eq!(f.admission.used(ResourceClass::Relay), 1);
            assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
            assert!(matches!(f.pipes.acquire(), Err(Error::Overloaded)));
            assert!(matches!(
                f.admission.reserve(None, ResourceClass::Relay, 1),
                Err(Error::Overloaded)
            ));
            let mut reader = Some(reader);
            if end == "disconnect" {
                reader.take();
            }
            if end == "cancel" {
                f.scope.cancel().unwrap();
            }
            if end != "drop" {
                let result = drive(&f.reactor, work.as_mut());
                let expected = match end {
                    "cancel" => Error::Cancelled,
                    "deadline" => Error::DeadlineExceeded,
                    _ => Error::Io,
                };
                assert!(
                    matches!(result, Err(error) if error == expected),
                    "{writing}/{end}"
                );
            }
            drop(work);
            if end == "drop" && !writing {
                assert_eq!(f.reactor.in_flight(), 1);
                assert_eq!(f.admission.used(ResourceClass::Connection), 2);
                assert_eq!(f.admission.used(ResourceClass::Relay), 1);
                assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
            }
            f.drain();
            assert_eq!(f.admission.used(ResourceClass::Connection), 0);
            assert_eq!(f.admission.used(ResourceClass::Relay), 0);
            assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
            assert!(f.admission.used(ResourceClass::Pipe) <= 1);
            if let Some(producer) = producer {
                producer.join().unwrap();
            }
        }
    }
}
#[test]
fn unsupported_splice_with_buffered_pipe_drains_suffix_and_keeps_exact_frame() {
    let f = Fixture::new();
    let length = 1024 * 1024 + 16;
    let (source, mut destination, mut writer, mut reader) = f.connections(length);
    destination.relay_fallback_at = Some(length / 2);
    let producer = std::thread::spawn(move || {
        writer.write_all(&vec![83; length]).unwrap();
        writer
    });
    let consumer = std::thread::spawn(move || {
        let mut body = vec![0; length];
        reader.read_exact(&mut body).unwrap();
        assert!(body.iter().all(|b| *b == 83));
        reader
    });
    let pipe = f.pipes.acquire().unwrap();
    let result = drive(
        &f.reactor,
        f.io.relay_body(source, destination, Some(pipe), &f.scope),
    )
    .unwrap();
    assert!(result.is_reusable());
    drop(result);
    drop(producer.join().unwrap());
    drop(consumer.join().unwrap());
    f.drain();
    assert_eq!(f.admission.used(ResourceClass::Relay), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(f.io.retained_buffer_bytes() <= MAX_PIPE_BYTES);
}

#[test]
fn tcp_backpressure_recovers_and_wakes_queued_pipe_owner_without_losing_frame() {
    use std::os::fd::AsRawFd;
    let f = Fixture::new();
    let length = 16 * 1024 * 1024 + 16;
    let (source, destination, mut writer, mut reader) = f.connections(length);
    let size: libc::c_int = 64 * 1024;
    // SAFETY: live TCP descriptor and correctly sized option storage.
    assert_eq!(
        unsafe {
            libc::setsockopt(
                destination.fd.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as libc::socklen_t,
            )
        },
        0
    );
    let producer = std::thread::spawn(move || {
        writer.write_all(&vec![83; length]).unwrap();
        writer
    });
    let mut pipe = f.pipes.acquire().unwrap();
    pipe.prepare_transit();
    let mut relay = Box::pin(f.io.relay_body(source, destination, Some(pipe), &f.scope));
    let mut waiting = f.pipes.acquire_wait(&f.scope);
    assert!(poll(waiting.as_mut()).is_pending());
    // The reader is deliberately not running. A page cannot fit in the bounded
    // send/receive buffers, so transit must retain its owners under backpressure.
    let pause = Instant::now() + Duration::from_millis(40);
    while Instant::now() < pause {
        f.reactor.poll_budgeted(64).unwrap();
        assert!(poll(relay.as_mut()).is_pending());
        std::thread::yield_now();
    }
    assert!(poll(waiting.as_mut()).is_pending());
    assert_eq!(f.admission.used(ResourceClass::Relay), 1);
    assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
    assert_eq!(f.admission.used(ResourceClass::Connection), 2);
    let consumer = std::thread::spawn(move || {
        let mut body = vec![0; length];
        reader.read_exact(&mut body).unwrap();
        assert!(body.iter().all(|b| *b == 83));
        reader
    });
    let (connection, pipe) = drive_worker(&f.reactor, async { futures::join!(relay, waiting) });
    let mut connection = connection.unwrap();
    let mut pipe = pipe.unwrap();
    assert!(connection.is_reusable());
    assert_eq!(f.admission.used(ResourceClass::Relay), 0);
    assert_eq!(pipe.buffered(), 0);
    assert_eq!(f.admission.used(ResourceClass::Connection), 1);
    // Reusing the pipe must not corrupt bytes already accepted by the socket.
    pipe.try_write(b"replacement").unwrap();
    let mut bytes = [0; 11];
    assert_eq!(pipe.try_read(&mut bytes).unwrap(), 11);
    assert_eq!(&bytes, b"replacement");
    drop(pipe);
    drop(producer.join().unwrap());
    let mut reader = consumer.join().unwrap();
    connection.rx_remaining = Some(0);
    connection.tx_remaining = Some(4);
    let mut suffix = f.io.buffer(4).unwrap();
    suffix.bytes_mut().unwrap().copy_from_slice(b"next");
    drop(drive_worker(&f.reactor, f.io.write_body(connection, suffix, &f.scope)).unwrap());
    let mut next = [0; 4];
    reader.read_exact(&mut next).unwrap();
    assert_eq!(&next, b"next");
    assert_eq!(reader.read(&mut next).unwrap(), 0);
    f.drain();
    assert_eq!(f.admission.used(ResourceClass::Connection), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    // HttpIo retains its bounded reusable read/write buffers after completion.
    assert!(f.io.retained_buffer_bytes() <= MAX_PIPE_BYTES);
    assert_eq!(f.pipes.idle_count(), 1);
}

#[test]
#[ignore = "isolated relay body CPU benchmark; run explicitly"]
fn opaque_body_cpu_benchmark() {
    fn cpu() -> Duration {
        let mut time = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        // SAFETY: correctly sized, live output storage.
        assert_eq!(
            unsafe { libc::clock_gettime(libc::CLOCK_THREAD_CPUTIME_ID, &mut time) },
            0
        );
        Duration::new(time.tv_sec as u64, time.tv_nsec as u32)
    }
    for materialized in [true, false, false, true] {
        let mut wall = Duration::ZERO;
        let mut used = Duration::ZERO;
        for _ in 0..8 {
            let f = Fixture::new();
            let length = 16 * 1024 * 1024 + 16;
            let (source, destination, mut writer, mut reader) = f.connections(length);
            let producer = std::thread::spawn(move || {
                writer.write_all(&vec![83; length]).unwrap();
                writer
            });
            let consumer = std::thread::spawn(move || {
                let mut body = vec![0; length];
                reader.read_exact(&mut body).unwrap();
                assert!(body.iter().all(|b| *b == 83));
                reader
            });
            let start = Instant::now();
            let started_cpu = cpu();
            if materialized {
                let mut source = source;
                let mut buffer = crate::peer::transfer::WireBuffer::reserved(
                    f.admission
                        .reserve(
                            Some(&crate::model::CacheId("bench".into())),
                            ResourceClass::Ciphertext,
                            length,
                        )
                        .unwrap(),
                    length,
                )
                .unwrap();
                let mut offset = 0;
                while offset < length {
                    let done = drive_worker(
                        &f.reactor,
                        f.io.read_body_range(source, buffer, offset..length, &f.scope),
                    )
                    .unwrap();
                    offset += done.bytes;
                    source = done.lease;
                    buffer = done.buffer;
                }
                let (bytes, reservation) = buffer.into_parts();
                let envelope = crate::model::PageEnvelope {
                    page: crate::model::PageId {
                        version: crate::model::ObjectVersion {
                            object: crate::model::ObjectId {
                                cache: crate::model::CacheId("bench".into()),
                                key: crate::model::CacheKey([0; 32]),
                            },
                            etag: crate::model::StrongEtag::test_value("v1"),
                        },
                        number: crate::model::PageNumber(0),
                    },
                    key_id: crate::model::KeyId([0; 16]),
                    nonce: crate::model::Nonce([0; 24]),
                    plaintext_length: (length - 16) as u32,
                    ciphertext_length: length as u32,
                };
                let page = crate::memory::pool::BufferPool::new(f.admission.clone())
                    .ciphertext(reservation, envelope, bytes)
                    .unwrap();
                let done =
                    drive_worker(&f.reactor, f.io.write_body(destination, page, &f.scope)).unwrap();
                drop(done);
                drop(source);
            } else {
                let mut pipe = f.pipes.acquire().unwrap();
                pipe.prepare_transit();
                drop(
                    drive_worker(
                        &f.reactor,
                        f.io.relay_body(source, destination, Some(pipe), &f.scope),
                    )
                    .unwrap(),
                );
            }
            used += cpu() - started_cpu;
            wall += start.elapsed();
            drop(producer.join().unwrap());
            drop(consumer.join().unwrap());
            f.drain();
        }
        eprintln!(
            "body materialized={materialized} bytes={} wall_ms={:.3} relay_cpu_ms={:.3}",
            8 * (16 * 1024 * 1024 + 16),
            wall.as_secs_f64() * 1000.0,
            used.as_secs_f64() * 1000.0
        );
    }
}
