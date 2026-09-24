// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod conformance {
    /// Consume this run's production-compiler export, prepared before test execution.
    /// The harness runs controlplane's placement_export test into a fresh directory
    /// with RACER_PLACEMENT_EXPORT set, then writes export-receipt.json only after
    /// success: {"producer":"racer-controlplane/placement_export::export_dataplane_placement",
    /// "run_id":"<unique run token>"}. Pass that directory and the same token as
    /// RACER_PLACEMENT_EXPORT and RACER_PLACEMENT_EXPORT_RUN_ID to these tests.
    /// Missing, incomplete, or stale artifacts fail; no nested Cargo or skip fallback.
    pub(crate) fn compiler_snapshots() -> &'static std::path::Path {
        static EXPORT: std::sync::OnceLock<Result<std::path::PathBuf, String>> =
            std::sync::OnceLock::new();
        let result = EXPORT.get_or_init(|| {
            let dir = std::env::var_os("RACER_PLACEMENT_EXPORT")
                .map(std::path::PathBuf::from)
                .ok_or(
                    "RACER_PLACEMENT_EXPORT must name this run's preexported compiler artifacts",
                )?;
            let run_id = std::env::var("RACER_PLACEMENT_EXPORT_RUN_ID")
                .map_err(|_| "RACER_PLACEMENT_EXPORT_RUN_ID must identify this preparation run")?;
            validate_compiler_export(&dir, &run_id).map_err(|error| {
                format!(
                    "compiler artifact preparation failed at {}: {error}",
                    dir.display()
                )
            })?;
            Ok(dir)
        });
        result
            .as_ref()
            .unwrap_or_else(|error| panic!("{error}"))
            .as_path()
    }

    const COMPILER_PRODUCER: &str =
        "racer-controlplane/placement_export::export_dataplane_placement";

    fn validate_compiler_export(dir: &std::path::Path, run_id: &str) -> io::Result<()> {
        let invalid = |message| io::Error::new(io::ErrorKind::InvalidData, message);
        if !dir.is_absolute() || run_id.is_empty() {
            return Err(invalid(
                "compiler export requires an absolute path and nonempty run ID",
            ));
        }
        let read_json = |name: &str| -> io::Result<Vec<u8>> {
            let path = dir.join(name);
            let meta = std::fs::symlink_metadata(&path)?;
            if !meta.is_file() || meta.len() > 65536 {
                return Err(invalid(
                    "compiler export JSON must be a regular file within 64 KiB",
                ));
            }
            let mut bytes = Vec::new();
            std::fs::File::open(path)?
                .take(65537)
                .read_to_end(&mut bytes)?;
            if bytes.len() > 65536 {
                return Err(invalid("compiler export JSON exceeds 64 KiB"));
            }
            Ok(bytes)
        };
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Receipt {
            producer: String,
            run_id: String,
        }
        let receipt: Receipt = serde_json::from_slice(&read_json("export-receipt.json")?)?;
        if receipt.producer != COMPILER_PRODUCER || receipt.run_id != run_id {
            return Err(invalid(
                "compiler export producer or run ID mismatch; regenerate artifacts",
            ));
        }
        let files: Vec<String> = serde_json::from_slice(&read_json("files.json")?)?;
        // All recipients for 2, 3, and 7 nodes at five slot geometries, including
        // one-slot HRW placement with live forwarding recipients.
        let expected: std::collections::BTreeSet<_> = [2, 3, 7]
            .into_iter()
            .flat_map(|nodes| {
                [1, 8, 17, 64, 262_144].into_iter().flat_map(move |slots| {
                    (0..nodes).map(move |node| format!("p{slots}-n{nodes}-fresh-{node}.pb"))
                })
            })
            .collect();
        if files.len() != expected.len()
            || files
                .iter()
                .cloned()
                .collect::<std::collections::BTreeSet<_>>()
                != expected
        {
            return Err(invalid(
                "compiler export manifest is incomplete or unexpected",
            ));
        }
        let mut required = expected;
        for file in &files {
            let (prefix, _) = file.rsplit_once('-').unwrap();
            required.insert(format!("{prefix}-ids.json"));
            required.insert(format!("{prefix}-owners.json"));
        }
        for file in required {
            let path = dir.join(file);
            let meta = std::fs::symlink_metadata(&path)?;
            if !meta.is_file() || meta.len() == 0 || meta.len() > 64 * 1024 * 1024 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("invalid compiler artifact: {}", path.display()),
                ));
            }
        }
        Ok(())
    }

    #[test]
    fn product_v1_corpus_preserves_membership_ownership_and_rejects_malformed() {
        use prost::Message;
        let dir = compiler_snapshots();
        let files: Vec<String> = serde_json::from_slice(&std::fs::read(dir.join("files.json")).unwrap()).unwrap();
        for file in files {
            let snapshot = crate::control::proto::Snapshot::decode(std::fs::read(dir.join(&file)).unwrap().as_slice()).unwrap();
            let trust = crate::control::Trust { universe: snapshot.universe.clone().try_into().unwrap(), node: snapshot.node.clone().try_into().unwrap() };
            let wrap = |s| crate::control::proto::Configuration { contents: Some(crate::control::proto::configuration::Contents::Snapshot(s)) };
            let prepared = trust.prepare(wrap(snapshot.clone())).unwrap();
            let (prefix, _) = file.rsplit_once('-').unwrap();
            let owners: Vec<String> = serde_json::from_slice(&std::fs::read(dir.join(format!("{prefix}-owners.json"))).unwrap()).unwrap();
            let ids: std::collections::BTreeMap<String, String> = serde_json::from_slice(&std::fs::read(dir.join(format!("{prefix}-ids.json"))).unwrap()).unwrap();
            let product = snapshot.volumes[0].topology.as_ref().unwrap().product.as_ref().unwrap();
            assert_eq!(snapshot.member_catalogs[0].members.len(), product.members.len());
            for (slot, owner) in owners.iter().enumerate() {
                assert_eq!(product.members[product.candidates[slot * product.candidate_width as usize] as usize], ids[owner]);
            }
            assert_eq!(prepared.volumes().len(), 1);
            let mut malformed = snapshot;
            malformed.volumes[0].topology.as_mut().unwrap().product.as_mut().unwrap().candidates[0] = u32::MAX;
            assert!(trust.prepare(wrap(malformed)).is_err());
        }
    }

    #[test]
    fn compiler_export_requires_complete_current_run_artifacts() {
        let dir =
            std::env::temp_dir().join(format!("racer-export-validation-{}", std::process::id()));
        std::fs::create_dir(&dir).unwrap();
        struct Cleanup(std::path::PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let _cleanup = Cleanup(dir.clone());
        assert!(validate_compiler_export(&dir, "run").is_err());
        let receipt = serde_json::json!({"producer": COMPILER_PRODUCER, "run_id": "run"});
        std::fs::write(dir.join("export-receipt.json"), receipt.to_string()).unwrap();
        assert!(validate_compiler_export(&dir, "stale").is_err());
        std::fs::write(dir.join("files.json"), b"[]").unwrap();
        assert!(validate_compiler_export(&dir, "run").is_err());
        let mut files = Vec::new();
        for nodes in [2, 3, 7] {
            for slots in [1, 8, 17, 64, 262_144] {
                let prefix = format!("p{slots}-n{nodes}-fresh");
                for node in 0..nodes {
                    let file = format!("{prefix}-{node}.pb");
                    std::fs::write(dir.join(&file), b"fixture").unwrap();
                    files.push(file);
                }
                for suffix in ["ids.json", "owners.json"] {
                    std::fs::write(dir.join(format!("{prefix}-{suffix}")), b"{}").unwrap();
                }
            }
        }
        std::fs::write(dir.join("files.json"), serde_json::to_vec(&files).unwrap()).unwrap();
        validate_compiler_export(&dir, "run").unwrap();
        // An old export that omits idle-member geometry is incomplete even when
        // all of its listed payloads and the current run receipt are present.
        let without_idle: Vec<_> = files
            .iter()
            .filter(|file| !file.starts_with("p1-"))
            .collect();
        std::fs::write(
            dir.join("files.json"),
            serde_json::to_vec(&without_idle).unwrap(),
        )
        .unwrap();
        assert!(validate_compiler_export(&dir, "run").is_err());
        std::fs::write(dir.join("files.json"), serde_json::to_vec(&files).unwrap()).unwrap();
        let wrong_producer = serde_json::json!({"producer": "fixture", "run_id": "run"});
        std::fs::write(dir.join("export-receipt.json"), wrong_producer.to_string()).unwrap();
        assert!(validate_compiler_export(&dir, "run").is_err());
        std::fs::write(dir.join("export-receipt.json"), receipt.to_string()).unwrap();
        files.push(files[0].clone());
        std::fs::write(dir.join("files.json"), serde_json::to_vec(&files).unwrap()).unwrap();
        assert!(validate_compiler_export(&dir, "run").is_err());
        files.pop();
        std::fs::write(dir.join("files.json"), serde_json::to_vec(&files).unwrap()).unwrap();
        std::fs::remove_file(dir.join("p262144-n7-fresh-6.pb")).unwrap();
        assert!(validate_compiler_export(&dir, "run").is_err());
    }
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
    fn actual_rust_compiler_snapshots_prepare_and_retain_last_good() {
        use prost::Message;
        let dir = crate::conformance::compiler_snapshots();
        {
            for name in ["cache"] {
                let s = proto::Snapshot::decode(
                    std::fs::read(dir.join("p262144-n2-fresh-0.pb")).unwrap().as_slice(),
                )
                .unwrap();
                assert_eq!(s.volumes[0].origin_socket, "/dev/racer/cache-a/origin");
                assert_eq!(s.volumes[0].cache_socket, "/dev/racer/cache-a/cache");
                let trust = Trust {
                    universe: s.universe.clone().try_into().unwrap(),
                    node: s.node.clone().try_into().unwrap(),
                };
                let prepared = trust.prepare(envelope(s.clone())).unwrap();
                assert_eq!(prepared.volumes()[0].backend().host(), "localhost");
                assert_last_good(&trust, s);
                println!("Rust compiler P2PCache snapshot prepared: {name}");
            }
        }
    }
}
