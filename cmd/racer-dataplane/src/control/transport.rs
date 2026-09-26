//! Nonblocking rustls over reactor-owned readiness. No executor or helper thread.
use super::{client::ControlEndpoint, enrollment::LocalSigningIdentity, wire};
use crate::{
    error::{Error, Operation, Result},
    runtime::deadline::RequestScope,
};
use std::{
    cell::RefCell,
    io::{Read, Write},
    net::{SocketAddr, TcpStream},
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime},
};

/// Runtime adapter: readiness registration must retain the FD until deregistration.
/// Dropping a wait cancels its registration. Timers and DNS run on the sole owner.
pub trait ControlIo {
    /// Production supplies the same owner-local reactor for filesystem and TLS.
    fn reactor(&self) -> Option<Rc<crate::runtime::reactor::Reactor>> {
        None
    }
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
        Box::pin(async move {
            let reactor = self.reactor().ok_or(Error::InvalidConfiguration)?;
            super::async_files::read_path(&reactor, path, limit, scope).await
        })
    }
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>>;
    fn ready<'a>(
        &'a self,
        fd: Rc<OwnedFd>,
        read: bool,
        write: bool,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()>;
    fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()>;
}
/// Concrete owner-local io_uring readiness/timer adapter. DNS is independently
/// injectable; the default implementation uses bounded nonblocking UDP queries.
pub struct ReactorControlIo {
    reactor: Rc<crate::runtime::reactor::Reactor>,
}
impl ReactorControlIo {
    pub fn new(reactor: Rc<crate::runtime::reactor::Reactor>) -> Self {
        Self { reactor }
    }
}
impl ControlIo for ReactorControlIo {
    fn reactor(&self) -> Option<Rc<crate::runtime::reactor::Reactor>> {
        Some(self.reactor.clone())
    }
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>> {
        Box::pin(async move { super::dns::resolve(self, host, port, scope).await })
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<OwnedFd>,
        read: bool,
        write: bool,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let interest =
                if read { libc::POLLIN } else { 0 } | if write { libc::POLLOUT } else { 0 };
            self.reactor.readiness(fd, interest as u32, scope).await?;
            scope.check()
        })
    }
    fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let duration = until.saturating_duration_since(Instant::now());
            if duration.is_zero() {
                return Ok(());
            }
            let raw = unsafe {
                libc::timerfd_create(
                    libc::CLOCK_MONOTONIC,
                    libc::TFD_CLOEXEC | libc::TFD_NONBLOCK,
                )
            };
            if raw < 0 {
                return Err(Error::Io);
            }
            let fd = Rc::new(unsafe { OwnedFd::from_raw_fd(raw) });
            let interval = libc::itimerspec {
                it_interval: libc::timespec {
                    tv_sec: 0,
                    tv_nsec: 0,
                },
                it_value: libc::timespec {
                    tv_sec: duration
                        .as_secs()
                        .try_into()
                        .map_err(|_| Error::InvalidRequest)?,
                    tv_nsec: duration.subsec_nanos() as _,
                },
            };
            if unsafe { libc::timerfd_settime(fd.as_raw_fd(), 0, &interval, std::ptr::null_mut()) }
                != 0
            {
                return Err(Error::Io);
            }
            self.ready(fd, true, false, scope).await
        })
    }
}
pub struct ControlTransport {
    endpoint: ControlEndpoint,
    io: RefCell<Option<Rc<dyn ControlIo>>>,
}
pub struct ControlConnection {
    stream: TcpStream,
    fd: Rc<OwnedFd>,
    tls: rustls::ClientConnection,
    io: Rc<dyn ControlIo>,
    host: String,
    expires: Option<SystemTime>,
}
pub struct HttpResponse {
    pub status: u16,
    pub body: Vec<u8>,
    pub retry_after: Option<Duration>,
}
struct Endpoint {
    host: String,
    authority: String,
    port: u16,
}
fn endpoint(url: &str) -> Result<Endpoint> {
    let authority = url
        .strip_prefix("https://")
        .ok_or(Error::InvalidConfiguration)?
        .trim_end_matches('/');
    if authority.is_empty()
        || authority.contains(['/', '?', '#', '@', '\r', '\n', '\\'])
        || !authority.is_ascii()
    {
        return Err(Error::InvalidConfiguration);
    }
    let (host, port) = if let Some(s) = authority.strip_prefix('[') {
        let (host, rest) = s.split_once(']').ok_or(Error::InvalidConfiguration)?;
        host.parse::<std::net::Ipv6Addr>()
            .map_err(|_| Error::InvalidConfiguration)?;
        (
            host,
            if rest.is_empty() {
                443
            } else {
                rest.strip_prefix(':')
                    .ok_or(Error::InvalidConfiguration)?
                    .parse()
                    .map_err(|_| Error::InvalidConfiguration)?
            },
        )
    } else {
        match authority.split_once(':') {
            Some((host, port)) => (host, port.parse().map_err(|_| Error::InvalidConfiguration)?),
            None => (authority, 443),
        }
    };
    rustls::pki_types::ServerName::try_from(host.to_owned())
        .map_err(|_| Error::InvalidConfiguration)?;
    if port == 0 {
        return Err(Error::InvalidConfiguration);
    }
    Ok(Endpoint {
        host: host.into(),
        authority: authority.into(),
        port,
    })
}
impl ControlTransport {
    pub fn new(endpoint: ControlEndpoint) -> Self {
        Self {
            endpoint,
            io: RefCell::new(None),
        }
    }
    pub fn attach_io(&self, io: Rc<dyn ControlIo>) {
        *self.io.borrow_mut() = Some(io);
    }
    pub fn io(&self) -> Result<Rc<dyn ControlIo>> {
        self.io.borrow().clone().ok_or(Error::InvalidConfiguration)
    }
    pub fn bootstrap<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ControlConnection> {
        self.connect(None, scope)
    }
    pub fn authenticated<'a>(
        &'a self,
        identity: &'a LocalSigningIdentity,
        scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        self.connect(Some(identity), scope)
    }
    fn connect<'a>(
        &'a self,
        identity: Option<&'a LocalSigningIdentity>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        Box::pin(async move {
            scope.check()?;
            if identity.is_some_and(|i| !i.valid_now()) {
                return Err(Error::Unauthorized);
            }
            let endpoint = endpoint(&self.endpoint.url)?;
            let io = self.io()?;
            let trust = io
                .read_file(&self.endpoint.trust_bundle, wire::MAX_BUNDLE_BYTES, scope)
                .await?;
            let mut roots = rustls::RootCertStore::empty();
            for cert in rustls_pemfile::certs(&mut trust.as_slice()) {
                roots
                    .add(cert.map_err(|_| Error::Unauthorized)?)
                    .map_err(|_| Error::Unauthorized)?;
            }
            if roots.is_empty() {
                return Err(Error::Unauthorized);
            }
            let builder = rustls::ClientConfig::builder_with_provider(Arc::new(
                rustls::crypto::ring::default_provider(),
            ))
            .with_safe_default_protocol_versions()
            .map_err(|_| Error::Unauthorized)?
            .with_root_certificates(roots);
            let mut config = if let Some(i) = identity {
                builder
                    .with_client_auth_cert(
                        i.certificate_chain()
                            .iter()
                            .cloned()
                            .map(rustls::pki_types::CertificateDer::from)
                            .collect(),
                        rustls::pki_types::PrivatePkcs8KeyDer::from(i.private_key_der().to_vec())
                            .into(),
                    )
                    .map_err(|_| Error::Unauthorized)?
            } else {
                builder.with_no_client_auth()
            };
            config.alpn_protocols = vec![b"http/1.1".to_vec()];
            config.resumption = rustls::client::Resumption::disabled();
            let addresses = if let Ok(ip) = endpoint.host.parse() {
                vec![SocketAddr::new(ip, endpoint.port)]
            } else {
                io.resolve(&endpoint.host, endpoint.port, scope).await?
            };
            if addresses.is_empty() || addresses.len() > 64 {
                return Err(Error::Unavailable);
            }
            let mut connected = None;
            for address in addresses {
                scope.check()?;
                if let Ok(stream) = connect_socket(address) {
                    let fd = Rc::new(OwnedFd::from(stream.try_clone().map_err(|_| Error::Io)?));
                    io.ready(fd.clone(), false, true, scope).await?;
                    if stream.take_error().map_err(|_| Error::Io)?.is_none() {
                        connected = Some((stream, fd));
                        break;
                    }
                }
            }
            let (stream, fd) = connected.ok_or(Error::Unavailable)?;
            let server = rustls::pki_types::ServerName::try_from(endpoint.host)
                .map_err(|_| Error::InvalidConfiguration)?;
            let mut tls = rustls::ClientConnection::new(Arc::new(config), server)
                .map_err(|_| Error::Unauthorized)?;
            tls.set_buffer_limit(Some(64 * 1024));
            let mut connection = ControlConnection {
                stream,
                fd,
                tls,
                io,
                host: endpoint.authority,
                expires: identity.map(|i| i.expires_at()),
            };
            while connection.tls.is_handshaking() {
                connection.step(scope).await?;
            }
            Ok(connection)
        })
    }
}
fn connect_socket(address: SocketAddr) -> Result<TcpStream> {
    let domain = if address.is_ipv4() {
        libc::AF_INET
    } else {
        libc::AF_INET6
    };
    let raw = unsafe {
        libc::socket(
            domain,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if raw < 0 {
        return Err(Error::Io);
    }
    let stream = unsafe { TcpStream::from_raw_fd(raw) };
    let result = match address {
        SocketAddr::V4(a) => {
            let addr = libc::sockaddr_in {
                sin_family: libc::AF_INET as _,
                sin_port: a.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from_ne_bytes(a.ip().octets()),
                },
                sin_zero: [0; 8],
            };
            unsafe {
                libc::connect(
                    raw,
                    (&addr as *const libc::sockaddr_in).cast(),
                    std::mem::size_of_val(&addr) as _,
                )
            }
        }
        SocketAddr::V6(a) => {
            let addr = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as _,
                sin6_port: a.port().to_be(),
                sin6_flowinfo: a.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: a.ip().octets(),
                },
                sin6_scope_id: a.scope_id(),
            };
            unsafe {
                libc::connect(
                    raw,
                    (&addr as *const libc::sockaddr_in6).cast(),
                    std::mem::size_of_val(&addr) as _,
                )
            }
        }
    };
    if result < 0 && std::io::Error::last_os_error().raw_os_error() != Some(libc::EINPROGRESS) {
        return Err(Error::Io);
    }
    Ok(stream)
}
impl ControlConnection {
    fn check(&self, scope: &RequestScope) -> Result<()> {
        scope.check()?;
        if self.expires.is_some_and(|e| SystemTime::now() >= e) {
            return Err(Error::Unauthorized);
        }
        Ok(())
    }
    async fn step(&mut self, scope: &RequestScope) -> Result<()> {
        self.check(scope)?;
        // Even a continuously readable peer must yield to other owner work.
        let mut yielded = false;
        std::future::poll_fn(|cx| {
            if yielded {
                std::task::Poll::Ready(())
            } else {
                yielded = true;
                cx.waker().wake_by_ref();
                std::task::Poll::Pending
            }
        })
        .await;
        let mut progress = false;
        if self.tls.wants_write() {
            match self.tls.write_tls(&mut self.stream) {
                Ok(0) => return Err(Error::Io),
                Ok(_) => progress = true,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(_) => return Err(Error::Io),
            }
        }
        if self.tls.wants_read() {
            match self.tls.read_tls(&mut self.stream) {
                Ok(0) => return Err(Error::Io),
                Ok(_) => {
                    self.tls.process_new_packets().map_err(tls_error)?;
                    progress = true;
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(_) => return Err(Error::Io),
            }
        }
        if !progress {
            let mut bounded = scope.clone();
            if let Some(expiry) = self.expires {
                let remaining = expiry
                    .duration_since(SystemTime::now())
                    .map_err(|_| Error::Unauthorized)?;
                bounded.deadline.0 = bounded.deadline.0.min(Instant::now() + remaining);
            }
            self.io
                .ready(
                    self.fd.clone(),
                    self.tls.wants_read(),
                    self.tls.wants_write(),
                    &bounded,
                )
                .await?;
        }
        Ok(())
    }
    /// One request per connection; no pooled session can cross identity rotation.
    pub fn request<'a>(
        mut self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, HttpResponse> {
        Box::pin(async move {
            if !matches!(method, "GET" | "POST")
                || !path.starts_with('/')
                || path.bytes().any(|b| b <= 32 || b >= 127)
                || token.is_some_and(|t| t.bytes().any(|b| b <= 32 || b >= 127))
            {
                return Err(Error::InvalidRequest);
            }
            let mut request = zeroize::Zeroizing::new(format!(
                "{method} {path} HTTP/1.1\r\nHost: {}\r\nAccept: application/json\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n",
                self.host,
                body.len()
            ));
            if let Some(token) = token {
                request.push_str("Authorization: Bearer ");
                request.push_str(token);
                request.push_str("\r\n");
            }
            request.push_str("\r\n");
            for bytes in [request.as_bytes(), body] {
                let mut offset = 0;
                while offset < bytes.len() {
                    self.check(scope)?;
                    match self.tls.writer().write(&bytes[offset..]) {
                        Ok(n) if n != 0 => offset += n,
                        Ok(_) => (),
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(_) => return Err(Error::Io),
                    }
                    self.step(scope).await?;
                }
            }
            while self.tls.wants_write() {
                self.step(scope).await?;
            }
            let mut received = Vec::new();
            let mut scratch = [0; 16384];
            let (status, header_len, framing, retry_after) = loop {
                if let Some(head) = parse_head(&received, limit)? {
                    break head;
                }
                if received.len() >= 16384 {
                    return Err(Error::Overloaded);
                }
                self.receive(&mut received, &mut scratch, scope).await?;
            };
            let body = match framing {
                Framing::Length(length) => {
                    if received.len() > header_len + length {
                        return Err(Error::InvalidRequest);
                    }
                    while received.len() < header_len + length {
                        self.receive(&mut received, &mut scratch, scope).await?;
                        if received.len() > header_len + length {
                            return Err(Error::InvalidRequest);
                        }
                    }
                    received.split_off(header_len)
                }
                Framing::Chunked => {
                    let mut raw = received.split_off(header_len);
                    let mut body = Vec::new();
                    let bound = if status == 200 {
                        limit
                    } else {
                        wire::MAX_ENROLLMENT_BYTES
                    };
                    loop {
                        let line_end = loop {
                            if let Some(n) = raw.windows(2).position(|w| w == b"\r\n") {
                                break n;
                            }
                            if raw.len() > 128 {
                                return Err(Error::InvalidRequest);
                            }
                            self.receive(&mut raw, &mut scratch, scope).await?;
                        };
                        if line_end == 0
                            || line_end > 16
                            || !raw[..line_end].iter().all(u8::is_ascii_hexdigit)
                        {
                            return Err(Error::InvalidRequest);
                        }
                        let length = usize::from_str_radix(
                            std::str::from_utf8(&raw[..line_end])
                                .map_err(|_| Error::InvalidRequest)?,
                            16,
                        )
                        .map_err(|_| Error::Overloaded)?;
                        raw.drain(..line_end + 2);
                        if length > bound - body.len() {
                            return Err(Error::Overloaded);
                        }
                        let mut remaining = length;
                        while remaining != 0 {
                            if raw.is_empty() {
                                self.receive(&mut raw, &mut scratch, scope).await?;
                            }
                            let n = remaining.min(raw.len());
                            body.extend(raw.drain(..n));
                            remaining -= n;
                        }
                        while raw.len() < 2 {
                            self.receive(&mut raw, &mut scratch, scope).await?;
                        }
                        if &raw[..2] != b"\r\n" {
                            return Err(Error::InvalidRequest);
                        }
                        raw.drain(..2);
                        if length == 0 {
                            if !raw.is_empty() {
                                return Err(Error::InvalidRequest);
                            }
                            break;
                        }
                    }
                    body
                }
            };
            self.check(scope)?;
            Ok(HttpResponse {
                status,
                body,
                retry_after,
            })
        })
    }
    async fn receive(
        &mut self,
        into: &mut Vec<u8>,
        scratch: &mut [u8],
        scope: &RequestScope,
    ) -> Result<()> {
        loop {
            self.check(scope)?;
            match self.tls.reader().read(scratch) {
                Ok(0) => return Err(Error::Io),
                Ok(n) => {
                    into.extend_from_slice(&scratch[..n]);
                    return Ok(());
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => self.step(scope).await?,
                Err(_) => return Err(Error::Io),
            }
        }
    }
}
fn tls_error(error: rustls::Error) -> Error {
    match error {
        // Go's GetConfigForClient sends internal_error when admission is full or
        // the issuer lookup is unavailable, before HTTP can return 429/503.
        // Retry through the existing bounded control backoff, with fresh TLS
        // authentication on every attempt. Certificate and other TLS failures
        // remain fatal; an alert never authorizes a connection.
        rustls::Error::AlertReceived(rustls::AlertDescription::InternalError) => Error::Unavailable,
        _ => Error::Unauthorized,
    }
}
enum Framing {
    Length(usize),
    Chunked,
}
fn parse_head(
    bytes: &[u8],
    limit: usize,
) -> Result<Option<(u16, usize, Framing, Option<Duration>)>> {
    let mut headers = [httparse::EMPTY_HEADER; 64];
    let mut response = httparse::Response::new(&mut headers);
    let length = match response.parse(bytes).map_err(|_| Error::InvalidRequest)? {
        httparse::Status::Partial => return Ok(None),
        httparse::Status::Complete(n) => n,
    };
    if length > 16384 || response.version != Some(1) {
        return Err(Error::InvalidRequest);
    }
    let status = response.code.ok_or(Error::InvalidRequest)?;
    let mut content_length = None;
    let mut retry = None;
    let mut content_type = false;
    let mut chunked = false;
    for h in response.headers {
        if h.name.eq_ignore_ascii_case("content-encoding") {
            return Err(Error::InvalidRequest);
        }
        if h.name.eq_ignore_ascii_case("transfer-encoding") {
            if chunked || !h.value.eq_ignore_ascii_case(b"chunked") {
                return Err(Error::InvalidRequest);
            }
            chunked = true;
        }
        if h.name.eq_ignore_ascii_case("content-length") {
            if content_length.is_some() {
                return Err(Error::InvalidRequest);
            }
            let s = std::str::from_utf8(h.value).map_err(|_| Error::InvalidRequest)?;
            if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
                return Err(Error::InvalidRequest);
            }
            content_length = Some(s.parse::<usize>().map_err(|_| Error::Overloaded)?);
        }
        if h.name.eq_ignore_ascii_case("content-type") {
            if content_type
                || !std::str::from_utf8(h.value)
                    .map_err(|_| Error::InvalidRequest)?
                    .split(';')
                    .next()
                    .is_some_and(|s| s.trim().eq_ignore_ascii_case("application/json"))
            {
                return Err(Error::InvalidRequest);
            }
            content_type = true;
        }
        if h.name.eq_ignore_ascii_case("retry-after") {
            if retry.is_some() {
                return Err(Error::InvalidRequest);
            }
            retry = Some(retry_delay(h.value)?);
        }
    }
    if chunked && (content_length.is_some() || status == 204) {
        return Err(Error::InvalidRequest);
    }
    let size = if status == 204 {
        if content_length.is_some_and(|n| n != 0) {
            return Err(Error::InvalidRequest);
        }
        0
    } else {
        if !content_type {
            return Err(Error::InvalidRequest);
        }
        if chunked {
            0
        } else {
            content_length.ok_or(Error::InvalidRequest)?
        }
    };
    let bound = if status == 200 {
        limit
    } else {
        wire::MAX_ENROLLMENT_BYTES
    };
    if size > bound {
        return Err(Error::Overloaded);
    }
    Ok(Some((
        status,
        length,
        if chunked {
            Framing::Chunked
        } else {
            Framing::Length(size)
        },
        retry,
    )))
}
fn retry_delay(bytes: &[u8]) -> Result<Duration> {
    let text = std::str::from_utf8(bytes).map_err(|_| Error::InvalidRequest)?;
    if !text.is_empty() && text.bytes().all(|b| b.is_ascii_digit()) {
        return text
            .parse::<u64>()
            .map(Duration::from_secs)
            .map_err(|_| Error::InvalidRequest);
    }
    // IMF-fixdate, the preferred HTTP-date form. No locale or time-zone globals.
    let parts: Vec<_> = text.split(' ').collect();
    if parts.len() != 6
        || !["Mon,", "Tue,", "Wed,", "Thu,", "Fri,", "Sat,", "Sun,"].contains(&parts[0])
        || parts[5] != "GMT"
    {
        return Err(Error::InvalidRequest);
    }
    let number = |s: &str| -> Result<i64> {
        if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
            return Err(Error::InvalidRequest);
        }
        s.parse().map_err(|_| Error::InvalidRequest)
    };
    let day = number(parts[1])?;
    let year = number(parts[3])?;
    let month = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ]
    .iter()
    .position(|s| *s == parts[2])
    .ok_or(Error::InvalidRequest)? as i64
        + 1;
    let time: Vec<_> = parts[4].split(':').collect();
    if time.len() != 3 || year < 1970 || year > 9999 {
        return Err(Error::InvalidRequest);
    }
    let hour = number(time[0])?;
    let minute = number(time[1])?;
    let second = number(time[2])?;
    let leap = year % 4 == 0 && (year % 100 != 0 || year % 400 == 0);
    let days = [
        31,
        if leap { 29 } else { 28 },
        31,
        30,
        31,
        30,
        31,
        31,
        30,
        31,
        30,
        31,
    ];
    if day < 1 || day > days[month as usize - 1] || hour > 23 || minute > 59 || second > 59 {
        return Err(Error::InvalidRequest);
    }
    let y = year - i64::from(month <= 2);
    let era = y / 400;
    let yoe = y - era * 400;
    let doy = (153 * (month + if month > 2 { -3 } else { 9 }) + 2) / 5 + day - 1;
    let days = era * 146097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719468;
    let seconds = days * 86400 + hour * 3600 + minute * 60 + second;
    let target = std::time::UNIX_EPOCH
        + Duration::from_secs(seconds.try_into().map_err(|_| Error::InvalidRequest)?);
    Ok(target.duration_since(SystemTime::now()).unwrap_or_default())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::control::{enrollment::Enrollment, testing};
    /// Test-only synchronous driver. Production uses ReactorControlIo and never
    /// calls poll from within a future; this drives real loopback TLS fixtures.
    struct FixtureIo;
    impl ControlIo for FixtureIo {
        fn read_file<'a>(
            &'a self,
            path: &'a std::path::Path,
            limit: usize,
            _: &'a RequestScope,
        ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
            Box::pin(async move {
                super::super::files::read_path(path, limit).map(zeroize::Zeroizing::new)
            })
        }
        fn resolve<'a>(
            &'a self,
            _: &'a str,
            port: u16,
            _: &'a RequestScope,
        ) -> Operation<'a, Vec<SocketAddr>> {
            Box::pin(async move { Ok(vec![SocketAddr::from(([127, 0, 0, 1], port))]) })
        }
        fn ready<'a>(
            &'a self,
            fd: Rc<OwnedFd>,
            read: bool,
            write: bool,
            scope: &'a RequestScope,
        ) -> Operation<'a, ()> {
            Box::pin(async move {
                loop {
                    scope.check()?;
                    let mut poll = libc::pollfd {
                        fd: fd.as_raw_fd(),
                        events: (if read { libc::POLLIN } else { 0 })
                            | (if write { libc::POLLOUT } else { 0 }),
                        revents: 0,
                    };
                    let n = unsafe { libc::poll(&mut poll, 1, 10) };
                    if n > 0 {
                        return Ok(());
                    }
                    if n < 0 {
                        return Err(Error::Io);
                    }
                }
            })
        }
        fn sleep<'a>(&'a self, _: Instant, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async { Ok(()) })
        }
    }
    #[test]
    fn real_server_auth_and_mutual_tls_chunked_response() {
        tls_fixture(None);
    }
    #[test]
    fn real_ring_server_auth_and_mutual_tls() {
        let Some(r) = testing::reactor() else {
            return;
        };
        tls_fixture(Some(r));
    }
    fn tls_fixture(reactor: Option<Rc<crate::runtime::reactor::Reactor>>) {
        let d = testing::Directory::new();
        let (ca, ca_key) = testing::ca();
        let mut params =
            rcgen::CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()]).unwrap();
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let cert = params.signed_by(&server_key, &ca, &ca_key).unwrap();
        let trust = d.0.join("trust.pem");
        std::fs::write(&trust, ca.pem()).unwrap();
        let enrollment = Enrollment::new(
            crate::model::identity::ClusterId("11111111-1111-4111-8111-111111111111".into()),
            d.0.join("token"),
            d.0.join("identity"),
        );
        enrollment
            .set_peer_trust_roots(vec![ca.der().to_vec()])
            .unwrap();
        let request = enrollment.prepare_now().unwrap();
        let identity = enrollment
            .accept_response(testing::issue(
                &request,
                &ca,
                &ca_key,
                "22222222-2222-4222-8222-222222222222",
            ))
            .unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let mut roots = rustls::RootCertStore::empty();
        roots.add(ca.der().clone()).unwrap();
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots),
            Arc::new(rustls::crypto::ring::default_provider()),
        )
        .allow_unauthenticated()
        .build()
        .unwrap();
        let config = rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_client_cert_verifier(verifier)
        .with_single_cert(
            vec![cert.der().clone()],
            rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
        )
        .unwrap();
        let server = std::thread::spawn(move || {
            for mutual in [false, true] {
                let (socket, _) = listener.accept().unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(10)))
                    .unwrap();
                socket
                    .set_write_timeout(Some(Duration::from_secs(10)))
                    .unwrap();
                let tls = rustls::ServerConnection::new(Arc::new(config.clone())).unwrap();
                let mut stream = rustls::StreamOwned::new(tls, socket);
                let mut request = Vec::new();
                let mut b = [0; 1];
                while !request.ends_with(b"\r\n\r\n") {
                    stream.read_exact(&mut b).unwrap();
                    request.push(b[0]);
                }
                assert_eq!(stream.conn.peer_certificates().is_some(), mutual);
                if !mutual {
                    assert!(
                        String::from_utf8(request)
                            .unwrap()
                            .contains("Authorization: Bearer fixture.token")
                    );
                }
                stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\n{}\r\n0\r\n\r\n").unwrap();
                stream.flush().unwrap();
            }
        });
        let transport = ControlTransport::new(ControlEndpoint {
            url: format!("https://127.0.0.1:{port}"),
            trust_bundle: trust,
        });
        if let Some(r) = &reactor {
            transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
        } else {
            transport.attach_io(Rc::new(FixtureIo));
        }
        let scope = testing::scope();
        let run = |future: Operation<'_, HttpResponse>| {
            if let Some(r) = &reactor {
                testing::drive(r, future)
            } else {
                futures::executor::block_on(future)
            }
        };
        let first = run(Box::pin(async {
            transport
                .bootstrap(&scope)
                .await?
                .request(
                    "POST",
                    wire::BOOTSTRAP_PATH,
                    Some("fixture.token"),
                    &[],
                    65536,
                    &scope,
                )
                .await
        }))
        .unwrap();
        assert_eq!(first.body, b"{}");
        let second = run(Box::pin(async {
            transport
                .authenticated(&identity, &scope)
                .await?
                .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
                .await
        }))
        .unwrap();
        assert_eq!(second.status, 200);
        assert_eq!(second.body, b"{}");
        server.join().unwrap();
    }
    #[test]
    fn only_internal_error_alert_is_transient() {
        assert_eq!(
            tls_error(rustls::Error::AlertReceived(
                rustls::AlertDescription::InternalError
            )),
            Error::Unavailable
        );
        for alert in [
            rustls::AlertDescription::AccessDenied,
            rustls::AlertDescription::BadCertificate,
            rustls::AlertDescription::CertificateExpired,
            rustls::AlertDescription::UnknownCA,
            rustls::AlertDescription::HandshakeFailure,
        ] {
            assert_eq!(
                tls_error(rustls::Error::AlertReceived(alert)),
                Error::Unauthorized
            );
        }
        assert_eq!(
            tls_error(rustls::Error::InvalidCertificate(
                rustls::CertificateError::UnknownIssuer
            )),
            Error::Unauthorized
        );
    }
    #[test]
    fn rejects_ambiguous_framing_and_insecure_urls() {
        for url in [
            "http://localhost",
            "https://user@localhost",
            "https://localhost/path",
            "https://localhost:0",
        ] {
            assert!(endpoint(url).is_err());
        }
        for bytes in [
            &b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nContent-Length: 2\r\n\r\n"[..],
            &b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nTransfer-Encoding: chunked\r\n\r\n"[..],
            &b"HTTP/1.1 204 No Content\r\nContent-Length: 1\r\n\r\n"[..],
            &b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 999999\r\n\r\n"[..],
        ] { assert!(parse_head(bytes,10).is_err()); }
    }
}
