//! Binary-only, opt-in heap profiling. No reactor work or runtime activation API.

const ADDRESS_ENV: &str = "RACER_HEAP_PROFILE_ADDR";

#[cfg(feature = "heap-profiling")]
pub use enabled::Server;

#[cfg(not(feature = "heap-profiling"))]
pub struct Server;

pub fn start_from_env() -> Result<Option<Server>, &'static str> {
    let Some(address) = std::env::var_os(ADDRESS_ENV) else {
        return Ok(None);
    };
    #[cfg(not(feature = "heap-profiling"))]
    {
        let _ = address;
        Err("RACER_HEAP_PROFILE_ADDR requires a build with the heap-profiling Cargo feature")
    }
    #[cfg(feature = "heap-profiling")]
    {
        let address = address
            .to_str()
            .and_then(|value| value.parse::<std::net::SocketAddr>().ok())
            .filter(|address| address.port() != 0)
            .ok_or("RACER_HEAP_PROFILE_ADDR must be a numeric IP:port with a nonzero port")?;
        enabled::check_profiler()?;
        let server = Server::bind(address)?;
        eprintln!(
            "racer-dataplane: heap profiling listener configured on {address}; profiles expose process memory allocation details; restrict network access"
        );
        Ok(Some(server))
    }
}

#[cfg(feature = "heap-profiling")]
mod enabled {
    use std::{
        io::{self, Read, Write},
        net::{SocketAddr, TcpListener, TcpStream},
        sync::{
            Arc,
            atomic::{AtomicBool, Ordering},
        },
        thread::{self, JoinHandle},
        time::{Duration, Instant},
    };

    const MAX_REQUEST: usize = 8192;
    const MAX_RESPONSE: usize = 32 * 1024 * 1024;
    const IO_TIMEOUT: Duration = Duration::from_secs(2);
    const ACCEPT_POLL: Duration = Duration::from_millis(100);
    const DISABLED: &str = "heap profiler is disabled; launch with _RJEM_MALLOC_CONF=prof:true,prof_active:true,lg_prof_sample:19";

    pub(super) fn check_profiler() -> Result<(), &'static str> {
        let ctl = jemalloc_pprof::PROF_CTL.as_ref().ok_or(DISABLED)?;
        let ctl = ctl.try_lock().map_err(|_| "heap profiler is busy")?;
        if !ctl.activated() {
            return Err(DISABLED);
        }
        Ok(())
    }

    #[derive(Debug)]
    enum DumpError {
        Busy,
        Disabled,
        Failed,
    }

    fn dump() -> Result<Vec<u8>, DumpError> {
        let ctl = jemalloc_pprof::PROF_CTL
            .as_ref()
            .ok_or(DumpError::Disabled)?;
        let mut ctl = ctl.try_lock().map_err(|_| DumpError::Busy)?;
        if !ctl.activated() {
            return Err(DumpError::Disabled);
        }
        // Upstream materializes the native dump and encoded profile before returning.
        // MAX_RESPONSE bounds transmission, not native dump time or peak memory.
        ctl.dump_pprof().map_err(|_| DumpError::Failed)
    }

    pub struct Server {
        stop: Arc<AtomicBool>,
        worker: Option<JoinHandle<()>>,
    }

    impl Server {
        pub(super) fn bind(address: SocketAddr) -> Result<Self, &'static str> {
            let listener =
                TcpListener::bind(address).map_err(|_| "cannot bind RACER_HEAP_PROFILE_ADDR")?;
            Self::spawn(listener, dump)
        }

        fn spawn(
            listener: TcpListener,
            mut dumper: impl FnMut() -> Result<Vec<u8>, DumpError> + Send + 'static,
        ) -> Result<Self, &'static str> {
            listener
                .set_nonblocking(true)
                .map_err(|_| "cannot configure heap profiling listener")?;
            let stop = Arc::new(AtomicBool::new(false));
            let worker_stop = Arc::clone(&stop);
            let worker = thread::Builder::new()
                .name("racer-heap-profile".into())
                .spawn(move || {
                    // One connection and one dump at a time. No userspace request queue,
                    // per-connection threads, async executor, or data-worker involvement.
                    while !worker_stop.load(Ordering::Acquire) {
                        match listener.accept() {
                            Ok((mut stream, _)) => {
                                let _ =
                                    handle(&mut stream, &mut dumper, MAX_RESPONSE, &worker_stop);
                            }
                            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                                thread::park_timeout(ACCEPT_POLL);
                            }
                            Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
                            Err(_) => {
                                eprintln!("racer-dataplane: heap profiling listener accept failed");
                                break;
                            }
                        }
                    }
                })
                .map_err(|_| "cannot start heap profiling listener thread")?;
            Ok(Self {
                stop,
                worker: Some(worker),
            })
        }
    }

    impl Drop for Server {
        fn drop(&mut self) {
            self.stop.store(true, Ordering::Release);
            if let Some(worker) = self.worker.take() {
                worker.thread().unpark();
                // Do not join: native dumping/symbolization has no cancellation or
                // deadline guarantee. This thread owns no application resources and
                // exits after the current operation; process exit terminates it. Socket
                // I/O is deadline-bounded, but shutdown does not await a native dump.
                drop(worker);
            }
        }
    }

    fn remaining(deadline: Instant) -> io::Result<Duration> {
        deadline
            .checked_duration_since(Instant::now())
            .filter(|duration| !duration.is_zero())
            .ok_or_else(|| io::Error::from(io::ErrorKind::TimedOut))
    }

    fn read_request(stream: &mut TcpStream, deadline: Instant) -> Result<(), u16> {
        let mut buffer = [0u8; MAX_REQUEST];
        let mut used = 0;
        loop {
            stream
                .set_read_timeout(Some(remaining(deadline).map_err(|_| 408u16)?))
                .map_err(|_| 400u16)?;
            let count = match stream.read(&mut buffer[used..]) {
                Ok(0) => return Err(400),
                Ok(count) => count,
                Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
                Err(error)
                    if matches!(
                        error.kind(),
                        io::ErrorKind::TimedOut | io::ErrorKind::WouldBlock
                    ) =>
                {
                    return Err(408);
                }
                Err(_) => return Err(400),
            };
            used += count;
            if parse_request(&buffer[..used])? {
                return Ok(());
            }
        }
    }

    // Returns false only when more bytes may complete a bounded request.
    fn parse_request(buffer: &[u8]) -> Result<bool, u16> {
        let mut headers = [httparse::EMPTY_HEADER; 64];
        let mut request = httparse::Request::new(&mut headers);
        match request.parse(buffer) {
            Ok(httparse::Status::Complete(end)) => {
                // Reject already-buffered trailing bytes. Bytes arriving later are
                // never interpreted: this listener closes after a single response.
                if end != buffer.len() {
                    return Err(400);
                }
                validate_request(&request)?;
                Ok(true)
            }
            Ok(httparse::Status::Partial) if buffer.len() < MAX_REQUEST => Ok(false),
            Ok(httparse::Status::Partial) | Err(httparse::Error::TooManyHeaders) => Err(431),
            Err(_) => Err(400),
        }
    }

    fn validate_request(request: &httparse::Request<'_, '_>) -> Result<(), u16> {
        let mut content_length = false;
        let mut host = false;
        for header in request.headers.iter() {
            if header.name.eq_ignore_ascii_case("transfer-encoding")
                || header.name.eq_ignore_ascii_case("expect")
            {
                return Err(400);
            }
            if header.name.eq_ignore_ascii_case("content-length") {
                if content_length || header.value != b"0" {
                    return Err(400);
                }
                content_length = true;
            }
            if header.name.eq_ignore_ascii_case("host") {
                if host || header.value.is_empty() {
                    return Err(400);
                }
                host = true;
            }
        }
        if request.version == Some(1) && !host {
            return Err(400);
        }
        if request.method != Some("GET") {
            return Err(405);
        }
        if !matches!(
            request.path,
            Some("/debug/pprof/allocs" | "/debug/pprof/heap")
        ) {
            return Err(404);
        }
        Ok(())
    }

    fn handle(
        stream: &mut TcpStream,
        dumper: &mut impl FnMut() -> Result<Vec<u8>, DumpError>,
        max_response: usize,
        stop: &AtomicBool,
    ) -> io::Result<()> {
        let (status, body) = match read_request(stream, Instant::now() + IO_TIMEOUT) {
            Err(status) => (status, Vec::new()),
            Ok(()) if stop.load(Ordering::Acquire) => return Ok(()),
            Ok(()) => match dumper() {
                Ok(body) if body.len() <= max_response => (200, body),
                Ok(_) => (500, Vec::new()),
                Err(DumpError::Busy | DumpError::Disabled) => (503, Vec::new()),
                Err(DumpError::Failed) => (500, Vec::new()),
            },
        };
        if stop.load(Ordering::Acquire) {
            return Ok(());
        }
        let reason = match status {
            200 => "OK",
            400 => "Bad Request",
            404 => "Not Found",
            405 => "Method Not Allowed",
            408 => "Request Timeout",
            431 => "Request Header Fields Too Large",
            503 => "Service Unavailable",
            _ => "Internal Server Error",
        };
        let allow = if status == 405 { "Allow: GET\r\n" } else { "" };
        let header = format!(
            "HTTP/1.1 {status} {reason}\r\nContent-Type: application/octet-stream\r\nCache-Control: no-store\r\nContent-Length: {}\r\nConnection: close\r\n{allow}\r\n",
            body.len()
        );
        let deadline = Instant::now() + IO_TIMEOUT;
        write_response(stream, header.as_bytes(), deadline)?;
        write_response(stream, &body, deadline)
    }

    fn write_response(
        stream: &mut TcpStream,
        mut bytes: &[u8],
        deadline: Instant,
    ) -> io::Result<()> {
        while !bytes.is_empty() {
            stream.set_write_timeout(Some(remaining(deadline)?))?;
            match stream.write(bytes) {
                Ok(0) => return Err(io::ErrorKind::WriteZero.into()),
                Ok(count) => bytes = &bytes[count..],
                Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
                Err(error) => return Err(error),
            }
        }
        Ok(())
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        fn exchange(
            request: &[u8],
            max_response: usize,
            mut dumper: impl FnMut() -> Result<Vec<u8>, DumpError> + Send + 'static,
        ) -> Vec<u8> {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let mut client = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            client
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            client.set_write_timeout(Some(IO_TIMEOUT)).unwrap();
            let worker = thread::spawn(move || {
                let (mut stream, _) = listener.accept().unwrap();
                handle(
                    &mut stream,
                    &mut dumper,
                    max_response,
                    &AtomicBool::new(false),
                )
                .unwrap();
            });
            client.write_all(request).unwrap();
            client.shutdown(std::net::Shutdown::Write).unwrap();
            let mut response = Vec::new();
            client.read_to_end(&mut response).unwrap();
            worker.join().unwrap();
            response
        }

        #[test]
        fn routes_preserve_binary_profile_and_disable_caching() {
            for path in ["allocs", "heap"] {
                let bytes = vec![0x1f, 0x8b, 0, 255, 42];
                let expected = bytes.clone();
                let response = exchange(
                    format!("GET /debug/pprof/{path} HTTP/1.1\r\nHost: localhost\r\n\r\n")
                        .as_bytes(),
                    bytes.len(),
                    move || Ok(bytes.clone()),
                );
                let end = response
                    .windows(4)
                    .position(|part| part == b"\r\n\r\n")
                    .unwrap()
                    + 4;
                let header = std::str::from_utf8(&response[..end]).unwrap();
                assert!(header.starts_with("HTTP/1.1 200 OK\r\n"));
                assert!(header.contains("Content-Type: application/octet-stream\r\n"));
                assert!(header.contains("Cache-Control: no-store\r\n"));
                assert!(!header.contains("Content-Encoding:"));
                assert_eq!(&response[end..], expected);
            }
        }

        #[test]
        fn rejects_methods_routes_malformed_and_body_bearing_requests() {
            for (request, status) in [
                ("POST /debug/pprof/heap HTTP/1.1\r\nHost: x\r\n\r\n", 405),
                ("GET /debug/pprof/activate HTTP/1.1\r\nHost: x\r\n\r\n", 404),
                ("GET /debug/pprof/heap?x=y HTTP/1.1\r\nHost: x\r\n\r\n", 404),
                ("not a request\r\n\r\n", 400),
                ("GET /debug/pprof/heap HTTP/1.1\r\n\r\n", 400),
                (
                    "GET /debug/pprof/heap HTTP/1.0\r\nContent-Length: 1\r\n\r\nx",
                    400,
                ),
                (
                    "GET /debug/pprof/heap HTTP/1.0\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n",
                    400,
                ),
                (
                    "GET /debug/pprof/heap HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n",
                    400,
                ),
                (
                    "GET /debug/pprof/heap HTTP/1.0\r\nExpect: 100-continue\r\n\r\n",
                    400,
                ),
                (
                    "GET /debug/pprof/heap HTTP/1.0\r\n\r\nuntrusted-secret",
                    400,
                ),
                ("GET /debug/pprof/heap HTTP/1.0\r\n", 400),
            ] {
                if request.ends_with("untrusted-secret") {
                    // TCP can split headers and undeclared trailing bytes across
                    // reads. Exercise the already-buffered case deterministically.
                    assert_eq!(parse_request(request.as_bytes()), Err(status));
                    continue;
                }
                let response =
                    exchange(request.as_bytes(), MAX_RESPONSE, || panic!("must not dump"));
                let response = std::str::from_utf8(&response).unwrap();
                assert!(
                    response.starts_with(&format!("HTTP/1.1 {status} ")),
                    "{response}"
                );
                assert!(!response.contains("untrusted-secret"));
                assert!(response.ends_with("\r\n\r\n"));
            }
        }

        #[test]
        fn request_and_response_limits_and_dump_errors() {
            let prefix = "GET /debug/pprof/heap HTTP/1.0\r\nX: ";
            let request = format!("{prefix}{}", "x".repeat(MAX_REQUEST - prefix.len()));
            let response = exchange(request.as_bytes(), MAX_RESPONSE, || panic!("must not dump"));
            assert!(response.starts_with(b"HTTP/1.1 431 "));
            for (result, status) in [
                (Ok(vec![42; 5]), 500),
                (Err(DumpError::Busy), 503),
                (Err(DumpError::Disabled), 503),
                (Err(DumpError::Failed), 500),
            ] {
                let mut result = Some(result);
                let response = exchange(b"GET /debug/pprof/heap HTTP/1.0\r\n\r\n", 4, move || {
                    result.take().unwrap()
                });
                assert!(response.starts_with(format!("HTTP/1.1 {status} ").as_bytes()));
                assert!(response.ends_with(b"\r\n\r\n"));
            }
        }

        #[test]
        fn io_deadlines_and_bind_failure() {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            assert!(Server::bind(listener.local_addr().unwrap()).is_err());
            let _client = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (mut stream, _) = listener.accept().unwrap();
            assert_eq!(
                read_request(&mut stream, Instant::now() + Duration::from_millis(20)),
                Err(408)
            );
            assert!(write_response(&mut stream, b"x", Instant::now()).is_err());
        }

        #[test]
        fn shutdown_does_not_wait_for_in_progress_dump() {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let (entered_tx, entered_rx) = std::sync::mpsc::sync_channel(1);
            let (release_tx, release_rx) = std::sync::mpsc::sync_channel(1);
            let (completed_tx, completed_rx) = std::sync::mpsc::sync_channel(1);
            struct Completed(std::sync::mpsc::SyncSender<()>);
            impl Drop for Completed {
                fn drop(&mut self) {
                    let _ = self.0.send(());
                }
            }
            let completed = Completed(completed_tx);
            let server = Server::spawn(listener, move || {
                let _keep_until_worker_exit = &completed;
                entered_tx.send(()).unwrap();
                release_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                Ok(vec![1])
            })
            .unwrap();
            let mut client = TcpStream::connect(address).unwrap();
            client.set_write_timeout(Some(IO_TIMEOUT)).unwrap();
            client
                .write_all(b"GET /debug/pprof/heap HTTP/1.0\r\n\r\n")
                .unwrap();
            entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            // Leave the JoinHandle in Server to exercise Drop's Some branch.
            drop(server);
            assert!(matches!(
                completed_rx.try_recv(),
                Err(std::sync::mpsc::TryRecvError::Empty)
            ));
            release_tx.send(()).unwrap();
            completed_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            let deadline = Instant::now() + Duration::from_secs(5);
            while TcpStream::connect_timeout(&address, Duration::from_millis(50)).is_ok() {
                assert!(
                    Instant::now() < deadline,
                    "listener must close after dump completes"
                );
                thread::sleep(Duration::from_millis(10));
            }
        }

        #[test]
        fn unavailable_tmpdir_fails_scrape_not_listener_startup() {
            if std::env::var_os("RACER_HEAP_TMPDIR_TEST_CHILD").is_none() {
                // current_exe is an existing regular file, so it cannot be used
                // as a temporary directory. No filesystem/environment mutation.
                let executable = std::env::current_exe().unwrap();
                let status = std::process::Command::new("timeout")
                    .args(["--signal=TERM", "--kill-after=10s", "30s"])
                    .arg(&executable)
                    .args(["--exact", "heap_profile::enabled::tests::unavailable_tmpdir_fails_scrape_not_listener_startup", "--nocapture"])
                    .env("RACER_HEAP_TMPDIR_TEST_CHILD", "1")
                    .env("_RJEM_MALLOC_CONF", "prof:true,prof_active:true,lg_prof_sample:19")
                    .env("TMPDIR", &executable)
                    .status().unwrap();
                assert!(status.success());
                return;
            }
            check_profiler().unwrap();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let _server = Server::spawn(listener, dump).unwrap();
            let mut client = TcpStream::connect(address).unwrap();
            client
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            client.set_write_timeout(Some(IO_TIMEOUT)).unwrap();
            client
                .write_all(b"GET /debug/pprof/heap HTTP/1.0\r\n\r\n")
                .unwrap();
            let mut response = String::new();
            client.read_to_string(&mut response).unwrap();
            assert!(response.starts_with("HTTP/1.1 500 Internal Server Error\r\n"));
            assert!(response.contains("Content-Length: 0\r\n"));
            assert!(response.ends_with("\r\n\r\n"));
            assert!(!response.contains(std::env::var("TMPDIR").unwrap().as_str()));
        }

        // Decode only the fields asserted here; prost ignores the remaining pprof
        // fields. These are field numbers from perftools.profiles.Profile.
        #[derive(prost::Message)]
        struct Profile {
            #[prost(message, repeated, tag = "1")]
            sample_type: Vec<ValueType>,
            #[prost(message, repeated, tag = "2")]
            sample: Vec<Sample>,
            #[prost(message, repeated, tag = "4")]
            location: Vec<Location>,
            #[prost(message, repeated, tag = "5")]
            function: Vec<Function>,
            #[prost(string, repeated, tag = "6")]
            string_table: Vec<String>,
        }

        #[derive(prost::Message)]
        struct ValueType {
            #[prost(int64, tag = "1")]
            r#type: i64,
            #[prost(int64, tag = "2")]
            unit: i64,
        }

        #[derive(prost::Message)]
        struct Sample {
            #[prost(uint64, repeated, tag = "1")]
            location_id: Vec<u64>,
            #[prost(int64, repeated, tag = "2")]
            value: Vec<i64>,
        }

        #[derive(prost::Message)]
        struct Location {
            #[prost(uint64, tag = "1")]
            id: u64,
            #[prost(message, repeated, tag = "4")]
            line: Vec<Line>,
        }

        #[derive(prost::Message)]
        struct Line {
            #[prost(uint64, tag = "1")]
            function_id: u64,
        }

        #[derive(prost::Message)]
        struct Function {
            #[prost(uint64, tag = "1")]
            id: u64,
            #[prost(int64, tag = "2")]
            name: i64,
        }

        #[inline(never)]
        fn retained_heap_profile_test_allocation() -> Vec<u8> {
            std::hint::black_box(vec![42; 8 * 1024 * 1024])
        }

        #[test]
        fn real_profile_contains_retained_allocation_and_symbols() {
            use prost::Message;
            if std::env::var_os("RACER_HEAP_TEST_CHILD").is_none() {
                // Allocator configuration must precede process startup. Never mutate
                // this multithreaded test process's environment.
                let status = std::process::Command::new("timeout")
                    .args(["--signal=TERM", "--kill-after=10s", "60s"])
                    .arg(std::env::current_exe().unwrap())
                    .args(["--exact", "heap_profile::enabled::tests::real_profile_contains_retained_allocation_and_symbols", "--nocapture"])
                    .env("RACER_HEAP_TEST_CHILD", "1")
                    .env("_RJEM_MALLOC_CONF", "prof:true,prof_active:true,lg_prof_sample:19")
                    .status().unwrap();
                assert!(status.success());
                return;
            }
            check_profiler().unwrap();
            let retained = retained_heap_profile_test_allocation();
            let response = exchange(
                b"GET /debug/pprof/allocs HTTP/1.0\r\n\r\n",
                MAX_RESPONSE,
                dump,
            );
            assert!(response.starts_with(b"HTTP/1.1 200 "));
            let end = response
                .windows(4)
                .position(|part| part == b"\r\n\r\n")
                .unwrap()
                + 4;
            let mut bytes = Vec::new();
            flate2::read::GzDecoder::new(&response[end..])
                .read_to_end(&mut bytes)
                .unwrap();
            let profile = Profile::decode(bytes.as_slice()).unwrap();
            assert_eq!(profile.sample_type.len(), 1);
            let kind = &profile.sample_type[0];
            assert_eq!(profile.string_table[kind.r#type as usize], "inuse_space");
            assert_eq!(profile.string_table[kind.unit as usize], "bytes");
            let function_ids: Vec<_> = profile
                .function
                .iter()
                .filter(|function| {
                    profile.string_table[function.name as usize]
                        .contains("retained_heap_profile_test_allocation")
                })
                .map(|function| function.id)
                .collect();
            assert!(
                !function_ids.is_empty(),
                "retained allocation symbol must be present"
            );
            let location_ids: Vec<_> = profile
                .location
                .iter()
                .filter(|location| {
                    location
                        .line
                        .iter()
                        .any(|line| function_ids.contains(&line.function_id))
                })
                .map(|location| location.id)
                .collect();
            let retained_bytes: i64 = profile
                .sample
                .iter()
                .filter(|sample| {
                    sample
                        .location_id
                        .iter()
                        .any(|id| location_ids.contains(id))
                })
                .map(|sample| sample.value[0])
                .sum();
            assert!(
                retained_bytes >= retained.len() as i64,
                "retained allocation must contribute inuse bytes: {retained_bytes}"
            );
            std::hint::black_box(&retained);
        }
    }
}
