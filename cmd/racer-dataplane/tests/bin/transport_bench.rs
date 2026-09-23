// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn options(kind: Kind, args: &str) -> io::Result<Options> {
    Options::parse(kind, args.split_whitespace().map(str::to_owned))?.ok_or_else(|| invalid("help"))
}

#[test]
fn cli_bounds_and_modes() {
    assert!(options(Kind::Metadata, "client --connections-per-worker 128").is_ok());
    for args in [
        "client --connections-per-worker 0",
        "client --duration 0",
        "client --request-timeout 31",
        "client --connect 127.0.0.1:0",
        "server --body file",
        "client --rail fake",
    ] {
        assert!(options(Kind::Metadata, args).is_err(), "{args}");
    }
    assert!(options(Kind::TcpPage, "server --body file --slab-dir .").is_ok());
    assert!(options(Kind::TcpPage, "server --body file").is_err());
    assert!(options(Kind::Rdma, "client --rail fake:1:0 --depth 16").is_ok());
    for args in [
        "client",
        "client --rail fake:1:0 --depth 17",
        "client --rail fake:1:0 --duration 250",
    ] {
        assert!(options(Kind::Rdma, args).is_err());
    }
}

#[test]
fn histogram_bounds_and_aggregation() {
    let mut total = Histogram::default();
    for ns in [0, 1, 63, 64, 127, 128, 129, 1024, 1_000_000, 30_000_000_000] {
        let mut h = Histogram::default();
        h.record(Duration::from_nanos(ns));
        let upper = h.percentile(99);
        assert!(upper >= ns && upper <= ns + ns / 64 + 1, "{ns}: {upper}");
        total.merge(&h);
    }
    assert_eq!(total.count, 10);
    assert!(total.percentile(50) >= 127);
    assert_eq!(Histogram::default().percentile(50), 0);
}

#[test]
fn real_peer_descriptor_and_payload_contracts() {
    for kind in [Kind::Metadata, Kind::TcpPage, Kind::Rdma] {
        let f = fixture::Fixture::new(kind);
        let descriptor = f
            .descriptor(Instant::now() + Duration::from_secs(5))
            .unwrap();
        f.validate_descriptor(&descriptor).unwrap();
        let (_, budget) = crate::cache::peer_wire::budget_descriptor(&descriptor).unwrap();
        assert!(budget.is_some());
        let mut corrupt = descriptor.clone();
        *corrupt.last_mut().unwrap() ^= 1;
        assert!(f.validate_descriptor(&corrupt).is_err());
        let mut body = if kind == Kind::Metadata {
            fixture::record().to_bytes().to_vec()
        } else {
            (0..BUFFER_SIZE).map(fixture::pattern).collect()
        };
        f.validate_body(&body).unwrap();
        body[kind.bytes() - 1] ^= 1;
        assert!(f.validate_body(&body).is_err());
        assert!(f.descriptor(Instant::now()).is_err());
    }
}

#[test]
fn measurement_barrier_and_window_exclude_setup_and_drain() {
    let o = options(Kind::Metadata, "client --warmup 1 --duration 1").unwrap();
    let shared = Arc::new(Shared::default());
    let mut m = Measure::new(&o, shared.clone(), 0);
    m.warm_until = Instant::now();
    m.progress(true, false).unwrap();
    assert_eq!(shared.ready.load(Ordering::Acquire), 0);
    m.progress(true, true).unwrap();
    assert_eq!(shared.ready.load(Ordering::Acquire), 1);
    let start = Instant::now();
    shared
        .window
        .set(Window {
            start,
            end: start + Duration::from_secs(1),
        })
        .ok()
        .unwrap();
    m.completion(start - Duration::from_millis(1), start);
    m.completion(start, start + Duration::from_millis(1));
    m.completion(start, start + Duration::from_secs(2));
    assert_eq!(m.histogram.count, 1);
}

#[test]
fn static_rdma_configuration_uses_real_authorization() {
    for server in [true, false] {
        let tls = fixture::tls(server).unwrap();
        let o = options(
            Kind::Rdma,
            if server {
                "server --rail fake:1:0"
            } else {
                "client --rail fake:1:0"
            },
        )
        .unwrap();
        rdma::context(&o, &tls).unwrap();
    }
}

#[test]
fn ephemeral_tls_and_production_trust_isolation() {
    use crate::tls::{ExpectedPeer, TlsContext, TlsProgress, TlsSession};
    use std::net::{TcpListener, TcpStream};
    fn handshake(
        client: &TlsContext,
        server: &TlsContext,
        expected: ExpectedPeer,
    ) -> io::Result<()> {
        let listener = TcpListener::bind("127.0.0.1:0")?;
        let a = TcpStream::connect(listener.local_addr()?)?;
        let (b, _) = listener.accept()?;
        a.set_nonblocking(true)?;
        b.set_nonblocking(true)?;
        let mut client = TlsSession::client(client, a.into(), expected)?;
        let mut server = TlsSession::server(
            server,
            b.into(),
            ExpectedPeer::Identity(fixture::identity(false)),
        )?;
        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            let a = client.handshake()?;
            let b = server.handshake()?;
            if a == TlsProgress::Complete(()) && b == TlsProgress::Complete(()) {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(timed_out("test TLS handshake"));
            }
            thread::sleep(Duration::from_millis(1));
        }
    }
    let client = fixture::tls(false).unwrap();
    let server = fixture::tls(true).unwrap();
    handshake(
        &client,
        &server,
        ExpectedPeer::Identity(fixture::identity(true)),
    )
    .unwrap();
    assert!(
        handshake(
            &client,
            &server,
            ExpectedPeer::Identity(fixture::identity(false))
        )
        .is_err()
    );
    let authority = crate::tls::tests::Authority::new();
    let production = TlsContext::bootstrap(&authority.bundle()).unwrap();
    assert!(
        handshake(
            &production,
            &server,
            ExpectedPeer::Identity(fixture::identity(true))
        )
        .is_err()
    );
}
