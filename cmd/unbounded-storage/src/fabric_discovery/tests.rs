// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::io::{Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;

use super::{Listener, Manifest, Server, Transport, fetch};

fn manifest() -> Manifest {
    Manifest::new(
        42,
        9,
        vec![
            Listener {
                id: "fabric-0".to_string(),
                transport: Transport::Tcp,
                address: "127.0.0.1:1".to_string(),
            },
            Listener {
                id: "fabric-1".to_string(),
                transport: Transport::Rdma,
                address: "hex:0102".to_string(),
            },
        ],
    )
}

#[test]
fn server_and_client_exchange_typed_manifest() {
    let mut published = manifest();
    published.listeners.reverse();
    let server = Server::bind("127.0.0.1:0".parse().unwrap(), published).unwrap();
    let addr = server.local_addr().unwrap();
    let shutdown = Arc::new(AtomicBool::new(false));
    let server_shutdown = Arc::clone(&shutdown);
    let join = thread::spawn(move || server.serve(server_shutdown).unwrap());

    assert_eq!(fetch(addr).unwrap(), manifest());

    shutdown.store(true, Ordering::Release);
    join.join().unwrap();
}

#[test]
fn server_exposes_only_the_discovery_get_endpoint_as_json() {
    let server = Server::bind("127.0.0.1:0".parse().unwrap(), manifest()).unwrap();
    let addr = server.local_addr().unwrap();
    let shutdown = Arc::new(AtomicBool::new(false));
    let server_shutdown = Arc::clone(&shutdown);
    let join = thread::spawn(move || server.serve(server_shutdown).unwrap());

    let response = raw_request(addr, b"GET /v1/fabric HTTP/1.1\r\nHost: localhost\r\n\r\n");
    assert!(response.starts_with("HTTP/1.1 200 OK\r\n"));
    assert!(response.contains("Content-Type: application/json\r\n"));
    assert!(response.ends_with(
        "\r\n\r\n{\"version\":1,\"peer_id\":42,\"process_incarnation\":9,\"listeners\":[{\"id\":\"fabric-0\",\"transport\":\"tcp\",\"address\":\"127.0.0.1:1\"},{\"id\":\"fabric-1\",\"transport\":\"rdma\",\"address\":\"hex:0102\"}]}"
    ));

    let response = raw_request(addr, b"GET /other HTTP/1.1\r\nHost: localhost\r\n\r\n");
    assert!(response.starts_with("HTTP/1.1 404 Not Found\r\n"));
    let response = raw_request(addr, b"POST /v1/fabric HTTP/1.1\r\nHost: localhost\r\n\r\n");
    assert!(response.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"));

    shutdown.store(true, Ordering::Release);
    join.join().unwrap();
}

#[test]
fn client_does_not_follow_redirects() {
    let (addr, join) = one_response(
        b"HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1/\r\nContent-Length: 0\r\n\r\n",
    );
    assert!(fetch(addr).is_err());
    join.join().unwrap();
}

#[test]
fn client_rejects_oversized_advertised_response() {
    let response = format!(
        "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n",
        256 * 1024 + 1
    );
    let (addr, join) = one_response(response.as_bytes());
    assert!(fetch(addr).is_err());
    join.join().unwrap();
}

fn one_response(response: &[u8]) -> (SocketAddr, thread::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let response = response.to_vec();
    let join = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        let mut request = [0_u8; 1024];
        let _ = stream.read(&mut request).unwrap();
        stream.write_all(&response).unwrap();
    });
    (addr, join)
}

fn raw_request(addr: SocketAddr, request: &[u8]) -> String {
    let mut stream = TcpStream::connect(addr).unwrap();
    stream.write_all(request).unwrap();
    let mut response = String::new();
    stream.read_to_string(&mut response).unwrap();
    response
}
