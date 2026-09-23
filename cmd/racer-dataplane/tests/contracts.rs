// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod conformance {
    pub(crate) fn etag(body: &[u8]) -> String {
        crate::metadata::Checksum(*blake3::hash(body).as_bytes())
            .etag()
            .as_str()
            .to_owned()
    }

    pub(crate) fn kernel_child(test: &str, variable: &str) {
        use std::process::{Command, Stdio};
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", test, "--ignored", "--nocapture"])
            .env(variable, "1")
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .spawn()
            .unwrap();
        let end = Instant::now() + Duration::from_secs(45);
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                assert!(status.success());
                break;
            }
            if Instant::now() >= end {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("kernel child timed out: {test}");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    pub(crate) fn kernel_ring(count: usize, config: uring::Config) -> Option<uring::Ring> {
        match uring::Ring::http_test_ring(buffers::io_test_pool(count), config) {
            Ok(ring) => Some(ring),
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::EPERM | libc::ENOSYS | libc::ENOMEM)
                ) || error.kind() == io::ErrorKind::Unsupported =>
            {
                assert!(
                    std::env::var_os("RACER_REQUIRE_URING").is_none(),
                    "io_uring required: {error}"
                );
                eprintln!("SKIP kernel tests: {error}");
                None
            }
            Err(error) => panic!("ring: {error}"),
        }
    }
    use crate::{buffers, uring};
    use std::{
        io::{self, Read, Write},
        net::{SocketAddr, TcpListener},
        sync::{
            Arc, Mutex,
            atomic::{AtomicBool, Ordering},
        },
        time::{Duration, Instant},
    };

    pub(crate) fn ring(count: usize, config: uring::Config) -> uring::Ring {
        uring::Ring::http_test_ring(buffers::io_test_pool(count), config).unwrap()
    }
    pub(crate) fn origin(
        node: usize,
        stop: Arc<AtomicBool>,
        hits: Arc<Mutex<Vec<(usize, String)>>>,
    ) -> (SocketAddr, std::thread::JoinHandle<()>) {
        let reservation = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = reservation.local_addr().unwrap();
        let path = crate::control::tests::test_socket(address, "origin");
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        listener.set_nonblocking(true).unwrap();
        let thread = std::thread::spawn(move || {
            let _reservation = reservation;
            let end = Instant::now() + Duration::from_secs(60);
            while !stop.load(Ordering::Relaxed) && Instant::now() < end {
                let (mut stream, _) = match listener.accept() {
                    Ok(pair) => pair,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        std::thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    Err(e) => panic!("{e}"),
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                let mut bytes = Vec::new();
                while !bytes.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    stream.read_exact(&mut byte).unwrap();
                    bytes.push(byte[0]);
                }
                let request = String::from_utf8(bytes).unwrap();
                let get = request.starts_with("GET ");
                assert!(get || request.starts_with("HEAD "));
                if request
                    .split_whitespace()
                    .nth(1)
                    .unwrap()
                    .starts_with("/multipage-")
                {
                    let size = buffers::BUFFER_SIZE as u64;
                    let length = 2 * size + 17;
                    let range = request
                        .lines()
                        .find_map(|line| line.strip_prefix("Range: bytes="));
                    let (start, finish) = range
                        .map(|range| {
                            let (a, b) = range.split_once('-').unwrap();
                            (a.parse::<u64>().unwrap(), b.parse::<u64>().unwrap())
                        })
                        .unwrap_or((0, length - 1));
                    hits.lock().unwrap().push((node, request.clone()));
                    write!(stream, "HTTP/1.1 {}\r\nContent-Length: {}\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n", if range.is_some() { "206 Partial Content" } else { "200 OK" }, finish - start + 1, crate::metadata::Checksum([7; 32]).etag().as_str()).unwrap();
                    if range.is_some() {
                        write!(stream, "Content-Range: bytes {start}-{finish}/{length}\r\n")
                            .unwrap();
                    }
                    write!(stream, "\r\n").unwrap();
                    if get {
                        let chunk = [1 + (start / size) as u8; 65536];
                        let mut remaining = finish - start + 1;
                        while remaining > 0 {
                            let n = remaining.min(chunk.len() as u64) as usize;
                            stream.write_all(&chunk[..n]).unwrap();
                            remaining -= n as u64;
                        }
                    }
                    continue;
                }
                hits.lock().unwrap().push((node, request));
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", etag(b"abc")).unwrap();
                if get {
                    stream.write_all(b"abc").unwrap();
                }
            }
            std::fs::remove_file(path).unwrap();
        });
        (address, thread)
    }
}

#[cfg(test)]
mod endpoint_tests {
    use crate::control::{Trust, Updates, proto};
    use crate::handlers::Backend;
    use crate::http_client::Endpoint;
    use std::sync::Arc;

    #[derive(Debug)]
    struct BackendUrlCase {
        name: &'static str,
        url: &'static str,
        valid: bool,
        host: &'static str,
        port: u16,
    }

    fn corpus() -> Vec<BackendUrlCase> {
        let valid = |name, url, host, port| BackendUrlCase {
            name,
            url,
            valid: false,
            host,
            port,
        };
        let invalid = |name, url| BackendUrlCase {
            name,
            url,
            valid: false,
            host: "",
            port: 0,
        };
        vec![
            BackendUrlCase {
                name: "numeric-v4",
                url: "127.0.0.1:80",
                valid: true,
                host: "127.0.0.1:80",
                port: 80,
            },
            BackendUrlCase {
                name: "numeric-v6",
                url: "[2001:DB8::1]:8080",
                valid: true,
                host: "[2001:db8::1]:8080",
                port: 8080,
            },
            valid("ipv4", "http://127.0.0.1", "127.0.0.1", 80),
            valid("ipv4-slash", "http://127.0.0.1/", "127.0.0.1", 80),
            valid("port-min", "http://127.0.0.1:1", "127.0.0.1:1", 1),
            valid(
                "port-max",
                "http://127.0.0.1:65535/",
                "127.0.0.1:65535",
                65535,
            ),
            valid(
                "port-leading-zero",
                "http://127.0.0.1:00080",
                "127.0.0.1:00080",
                80,
            ),
            valid("ipv6", "http://[::1]", "[::1]", 80),
            valid(
                "ipv6-port",
                "http://[2001:DB8::1]:8080/",
                "[2001:DB8::1]:8080",
                8080,
            ),
            valid(
                "ipv6-mapped",
                "http://[::ffff:192.0.2.1]/",
                "[::ffff:192.0.2.1]",
                80,
            ),
            valid(
                "dns",
                "http://origin.ns.svc:8080",
                "origin.ns.svc:8080",
                8080,
            ),
            valid("dns-case", "http://Origin.Example/", "Origin.Example", 80),
            valid(
                "dns-dot",
                "http://origin.example.:80/",
                "origin.example.:80",
                80,
            ),
            valid("dns-single", "http://origin", "origin", 80),
            valid(
                "dns-punycode",
                "http://xn--bcher-kva.example",
                "xn--bcher-kva.example",
                80,
            ),
            valid(
                "dns-hyphen",
                "http://origin-1.example",
                "origin-1.example",
                80,
            ),
            invalid("uppercase-http", "HTTP://127.0.0.1"),
            invalid("mixed-http", "Http://127.0.0.1"),
            invalid("empty-port", "http://127.0.0.1:"),
            invalid("empty-ipv6-port", "http://[::1]:/"),
            invalid("empty-fragment", "http://127.0.0.1#"),
            invalid("slash-empty-fragment", "http://127.0.0.1/#"),
            invalid("fragment", "http://127.0.0.1#x"),
            invalid("credentials", "http://user:pass@127.0.0.1"),
            invalid("empty-credentials", "http://@127.0.0.1"),
            invalid("query", "http://127.0.0.1?q"),
            invalid("empty-query", "http://127.0.0.1?"),
            invalid("basepath", "http://127.0.0.1/base"),
            invalid("double-slash", "http://127.0.0.1//"),
            invalid("encoded-slash", "http://127.0.0.1/%2f"),
            invalid("encoded-root", "http://127.0.0.1%2f"),
            invalid("encoded-only-path", "http://127.0.0.1/%2F"),
            invalid("port-zero", "http://127.0.0.1:0"),
            invalid("port-overflow", "http://127.0.0.1:65536"),
            invalid("port-huge", "http://127.0.0.1:9999999999999999999999"),
            invalid("port-plus", "http://127.0.0.1:+80"),
            invalid("port-negative", "http://127.0.0.1:-1"),
            invalid("port-name", "http://127.0.0.1:http"),
            invalid("https", "https://127.0.0.1"),
            invalid("missing-authority", "http:///"),
            invalid("opaque", "http:127.0.0.1"),
            invalid("space", "http://127.0.0.1 "),
            invalid("tab", "http://127.0.0.1\t"),
            invalid("crlf", "http://127.0.0.1\r\nX:1"),
            invalid("nul", "http://127.0.0.1\0"),
            invalid("delete", "http://127.0.0.1\x7f"),
            invalid("encoded-control", "http://origin%0a.example"),
            invalid("backslash", "http://127.0.0.1\\"),
            invalid("unicode", "http://bücher.example"),
            invalid("unicode-port", "http://127.0.0.1:８０"),
            invalid("zone-escaped", "http://[fe80::1%25eth0]:80"),
            invalid("zone-raw", "http://[fe80::1%eth0]:80"),
            invalid("unbracketed-ipv6", "http://::1"),
            invalid("bracketed-dns", "http://[origin]"),
            invalid("bracketed-ipv4", "http://[127.0.0.1]"),
            invalid("broken-ipv6", "http://[:::1]"),
            invalid("ipvfuture", "http://[v1.foo]"),
            invalid("ipv4-leading-zero", "http://127.000.0.1"),
            invalid("ipv4-short", "http://127.1"),
            invalid("ipv4-integer", "http://2130706433"),
            invalid("ipv4-overflow", "http://256.0.0.1"),
            invalid("dns-empty-label", "http://origin..example"),
            invalid("dns-leading-hyphen", "http://-origin.example"),
            invalid("dns-trailing-hyphen", "http://origin-.example"),
            invalid("dns-underscore", "http://origin_test.example"),
            invalid("dns-comma", "http://origin,example"),
            invalid("dns-percent", "http://%6frigin.example"),
            invalid("ipv4-hex-integer", "http://0x7f000001"),
            invalid("ipv4-hex-uppercase", "http://0X7F000001"),
            invalid("ipv4-hex-dotted", "http://0x7f.0.0.1"),
            invalid("ipv4-hex-mixed-short", "http://127.0X1:8080/"),
            invalid("ipv4-hex-three-parts", "http://127.0.0x1"),
            invalid("ipv4-hex-all-parts", "http://0X7f.0x0.0X0.0x1"),
            invalid("ipv4-hex-octal-mixed", "http://0177.0x0.00.1"),
            invalid("ipv4-hex-final-dot", "http://0x7f000001./"),
            invalid("ipv4-hex-overflow", "http://0x100000000"),
            invalid("ipv4-octal", "http://0177.0.0.1"),
            valid("dns-hex-label", "http://0x7f.example", "0x7f.example", 80),
            valid("dns-hex-suffix", "http://origin.0X7F", "origin.0X7F", 80),
            valid("dns-hex-prefix", "http://0xhost", "0xhost", 80),
            valid("dns-hex-nondigit", "http://0x7g", "0x7g", 80),
            valid("dns-hex-empty-digits", "http://0x", "0x", 80),
            valid("dns-hex-hyphen", "http://0x7f-origin", "0x7f-origin", 80),
            valid(
                "dns-hex-five-labels",
                "http://0x1.2.3.4.5",
                "0x1.2.3.4.5",
                80,
            ),
            valid("dns-hex-letters", "http://dead.beef", "dead.beef", 80),
        ]
    }

    #[test]
    fn b12_raw_url_regression() {
        // Numeric fixtures need no external DNS on the unmodified implementation.
        let mut failures = Vec::new();
        for c in corpus() {
            let url = c.url;
            if ![
                "uppercase-http",
                "empty-port",
                "empty-fragment",
                "port-zero",
            ]
            .contains(&c.name)
            {
                continue;
            }
            if Backend::new(url, "test-origin").is_ok() {
                failures.push(url.to_owned());
            }
        }
        assert!(
            failures.is_empty(),
            "accepted invalid raw URLs: {failures:?}"
        );
    }

    #[test]
    fn b12_url_grammar_and_authority_identity() {
        {
            for c in corpus() {
                let url = c.url;
                let valid = c.valid;
                let syntax = Endpoint::parse(url);
                assert_eq!(syntax.is_ok(), valid, "syntax: {c:?}");
                let backend = Backend::new(url, "test-origin");
                assert_eq!(backend.is_ok(), valid, "backend: {c:?}");
                if valid {
                    let backend = backend.unwrap();
                    let authority = c.host;
                    assert_eq!(backend.host(), authority);
                    assert_eq!(backend.address().port(), c.port);
                    assert_eq!(
                        backend.namespace(),
                        crate::cache::Namespace::new("test-origin").unwrap()
                    );
                }
            }
            let namespace =
                |address, identity| Backend::new(address, identity).unwrap().namespace();
            assert_eq!(
                namespace("127.0.0.1:80", "ns/origin:80"),
                namespace("[::1]:81", "ns/origin:80")
            );
            assert_ne!(
                namespace("127.0.0.1:80", "ns/origin:80"),
                namespace("127.0.0.1:80", "ns/other:80")
            );
        }
    }

    #[test]
    fn endpoints_require_numeric_authorities_and_control_requires_https() {
        for address in [
            "localhost:80",
            "127.1:80",
            "2130706433:80",
            "0x7f000001:80",
            "127.000.0.1:80",
            "127.0.0.1",
            "127.0.0.1:0",
            "[::1%1]:80",
            "user@127.0.0.1:80",
        ] {
            assert!(Endpoint::parse(address).is_err(), "{address}");
            assert!(
                crate::control::Source::parse(&format!("http://{address}/config")).is_err(),
                "{address}"
            );
        }
        for address in ["127.0.0.1:8443", "[::1]:8443", "localhost:8443"] {
            assert!(
                crate::control::Source::parse(&format!("https://{address}/config?revision=1"))
                    .is_ok()
            );
        }
        for url in [
            "https://user@127.0.0.1:8443/config",
            "https://127.0.0.1:8443/config#fragment",
        ] {
            assert!(crate::control::Source::parse(url).is_err(), "{url}");
        }
    }

    fn envelope(s: proto::Snapshot) -> proto::Configuration {
        proto::Configuration {
            contents: Some(proto::configuration::Contents::Snapshot(s)),
        }
    }

    fn assert_last_good(trust: &Trust, s: proto::Snapshot) {
        let updates = Updates::default();
        updates.subscribe(Arc::new(crate::uring::Wake::new().unwrap()));
        updates
            .publish(trust.prepare(envelope(s.clone())).unwrap())
            .unwrap();
        updates.staged(s.revision, 0, true);
        updates.activated(s.revision, 0);
        let before = updates.latest(0).unwrap();
        let status = updates.status();
        let epoch = updates.applied_epoch();
        for c in corpus().into_iter().filter(|c| !c.valid) {
            let mut bad = s.clone();
            bad.revision += 1;
            bad.epoch += 1;
            bad.volumes[0].origin_socket = c.url.into();
            let prepared = trust.prepare(envelope(bad));
            assert!(prepared.is_err(), "invalid URL passed preparation: {c:?}");
            assert!(prepared.and_then(|p| updates.publish(p)).is_err(), "{c:?}");
            assert!(Arc::ptr_eq(&before, &updates.latest(0).unwrap()));
            assert_eq!(updates.status(), status);
            assert_eq!(updates.applied_epoch(), epoch);
        }
    }

    #[test]
    fn b12_invalid_update_retains_last_good() {
        let (trust, s) = crate::control::tests::fixture();
        assert_last_good(&trust, s);
    }

    #[test]
    #[ignore = "requires B12_EXPORT from TestB12ProductionSnapshots"]
    fn b12_actual_go_snapshots_prepare() {
        let dir = std::path::PathBuf::from(std::env::var("B12_EXPORT").unwrap());
        {
            for name in ["cache"] {
                let config: proto::Configuration = serde_json::from_slice(
                    &std::fs::read(dir.join(format!("{name}.json"))).unwrap(),
                )
                .unwrap();
                let Some(proto::configuration::Contents::Snapshot(s)) = config.contents else {
                    panic!()
                };
                assert_eq!(s.volumes[0].origin_socket, "/dev/racer/volume/origin");
                assert_eq!(s.volumes[0].cache_socket, "/dev/racer/volume/cache");
                let trust = Trust {
                    universe: s.universe.clone().try_into().unwrap(),
                    node: s.node.clone().try_into().unwrap(),
                };
                let prepared = trust.prepare(envelope(s.clone())).unwrap();
                assert_eq!(prepared.volumes()[0].backend().host(), "localhost");
                assert_last_good(&trust, s);
                println!("B12 Go P2PCache snapshot prepared: {name}");
            }
        }
    }
}
