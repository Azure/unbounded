// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::{
    io::{Read, Write},
    net::TcpListener,
    os::unix::net::UnixStream,
    sync::mpsc,
    thread,
};

#[test]
fn early_handler_delivers_http_before_storage_then_serves_written_file() {
    let Some(mut ring) = crate::conformance::kernel_ring(8, crate::uring::Config::default()) else {
        return;
    };
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    let backend = Backend::new(&origin.local_addr().unwrap().to_string(), "test-origin").unwrap();
    let upstream = thread::spawn(move || {
        for method in ["HEAD", "GET"] {
            let end = Instant::now() + Duration::from_secs(3);
            origin.set_nonblocking(true).unwrap();
            let mut socket = loop {
                match origin.accept() {
                    Ok((socket, _)) => break socket,
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < end, "origin accept stalled");
                        thread::sleep(Duration::from_millis(1));
                    }
                    Err(error) => panic!("origin accept: {error}"),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let request = request(&mut socket);
            assert!(request.starts_with(&format!("{method} /pipeline ")));
            let tag = crate::conformance::etag(b"abcdef");
            if method == "HEAD" {
                write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\nETag: {tag}\r\nContent-Type: application/octet-stream\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n").unwrap();
            } else {
                assert!(request.contains("Range: bytes=0-5\r\n"));
                assert!(request.contains(&format!("If-Match: {tag}\r\n")));
                write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: 6\r\nContent-Range: bytes 0-5/6\r\nETag: {tag}\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\nabcdef").unwrap();
            }
        }
    });
    let io = crate::slab_io::Io::testing(1 << 30, BUFFER_SIZE as u64, true);
    let mut cache = io.scope(|| cache(&backend, 1));
    cache.set_metrics(ring.metrics().clone());
    let registry = crate::metrics::Registry::new(1, Arc::new(crate::control::Updates::default()));
    registry.register(0, ring.metrics());
    let operations = || {
        let mut text = String::new();
        io.render(&mut text);
        text.lines()
            .find(|line| line.starts_with("racer_dataplane_slab_io_operations_total "))
            .unwrap()
            .to_string()
    };
    let before = operations();
    let paused = io.exhaust_and_pause_refill();
    let handler = Handler::new(cache, backend);
    let path = std::env::temp_dir().join(format!("p{}.sock", std::process::id()));
    let listener =
        http::Listener::bind_unix(crate::socket::UnixPath::new(path.to_str().unwrap()).unwrap())
            .unwrap();
    let mut server = http::Server::new(listener, handler, http::Config::default());
    let (delivered, received) = mpsc::channel();
    let (resume, released) = mpsc::channel();
    let client = thread::spawn(move || {
        for warm in [false, true] {
            if warm {
                released.recv_timeout(Duration::from_secs(3)).unwrap();
            }
            let mut socket = UnixStream::connect(&path).unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let tag = crate::conformance::etag(b"abcdef");
            write!(socket, "GET /pipeline HTTP/1.1\r\nHost: cache\r\nRange: bytes=1-4\r\nIf-Match: {tag}\r\nConnection: close\r\n\r\n").unwrap();
            let head = request(&mut socket).to_ascii_lowercase();
            assert!(head.starts_with("http/1.1 206 "), "{head}");
            for field in [
                "content-length: 4\r\n".to_string(),
                "content-range: bytes 1-4/6\r\n".into(),
                "content-type: application/octet-stream\r\n".into(),
                format!("etag: {tag}\r\n"),
            ] {
                assert!(head.contains(&field), "{head}");
            }
            let mut body = Vec::new();
            socket.read_to_end(&mut body).unwrap();
            assert_eq!(body, b"bcde");
            delivered.send(()).unwrap();
        }
    });
    let end = Instant::now() + Duration::from_secs(8);
    let mut drive = |ready: &mut dyn FnMut(&Ring) -> bool| {
        loop {
            assert!(Instant::now() < end, "pipeline handler stalled");
            ring.progress().unwrap();
            server.poll(&mut ring, 16).unwrap();
            server.handler_mut().poll_background(&mut ring, 16).unwrap();
            ring.metrics().publish();
            if ready(&ring) {
                break;
            }
            thread::yield_now();
        }
    };
    drive(&mut |_| received.try_recv().is_ok());
    let text = registry.render();
    assert!(
        text.contains("racer_dataplane_page_serve_total{decision=\"early_buffer\"} 1\n"),
        "{text}"
    );
    assert!(text.contains(
        "racer_dataplane_page_stage_total{stage=\"payload_write\",outcome=\"completed\"} 0\n"
    ));
    assert_eq!(operations(), before, "paused storage submitted payload I/O");
    drop(paused);
    drive(&mut |_| {
        registry.render().contains(
            "racer_dataplane_page_stage_total{stage=\"payload_write\",outcome=\"completed\"} 1\n",
        )
    });
    resume.send(()).unwrap();
    drive(&mut |_| received.try_recv().is_ok());
    assert!(
        registry
            .render()
            .contains("racer_dataplane_page_serve_total{decision=\"file\"} 1\n")
    );
    client.join().unwrap();
    upstream.join().unwrap();
    server.shutdown(&mut ring).unwrap();
    server.handler_mut().shutdown(&mut ring).unwrap();
    drop(server);
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}
