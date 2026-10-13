// Shared REST fixtures use the same imports in unit and integration builds.
#[cfg(all(test, feature = "rest"))]
extern crate self as wire_codec;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Truncated,

    Limit,

    InvalidHex,
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Copy, Debug)]
pub enum Endian {
    Little,

    Big,
}

pub struct Reader<'a> {
    remaining: &'a [u8],

    endian: Endian,
}

pub struct Writer<'a> {
    bytes: &'a mut Vec<u8>,

    maximum: usize,

    endian: Endian,
}

impl<'a> Reader<'a> {
    pub fn new(bytes: &'a [u8], endian: Endian) -> Self {
        Self {
            remaining: bytes,
            endian,
        }
    }

    pub fn remaining(&self) -> &'a [u8] {
        self.remaining
    }

    pub fn take(&mut self, length: usize) -> Result<&'a [u8]> {
        let (value, rest) = self
            .remaining
            .split_at_checked(length)
            .ok_or(Error::Truncated)?;
        self.remaining = rest;
        Ok(value)
    }

    pub fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::Truncated)
    }

    pub fn u32(&mut self) -> Result<u32> {
        Ok(match self.endian {
            Endian::Little => u32::from_le_bytes(self.array()?),
            Endian::Big => u32::from_be_bytes(self.array()?),
        })
    }

    pub fn u64(&mut self) -> Result<u64> {
        Ok(match self.endian {
            Endian::Little => u64::from_le_bytes(self.array()?),
            Endian::Big => u64::from_be_bytes(self.array()?),
        })
    }

    pub fn count(&mut self, maximum: usize, minimum_bytes: usize) -> Result<usize> {
        let count = usize::try_from(self.u32()?).map_err(|_| Error::Limit)?;
        if count > maximum || (minimum_bytes != 0 && count > self.remaining.len() / minimum_bytes) {
            return Err(Error::Limit);
        }
        Ok(count)
    }

    pub fn length_prefixed(&mut self, maximum: usize) -> Result<&'a [u8]> {
        let length = self.count(maximum, 1)?;
        self.take(length)
    }
}

impl<'a> Writer<'a> {
    pub fn new(bytes: &'a mut Vec<u8>, maximum: usize, endian: Endian) -> Self {
        Self {
            bytes,
            maximum,
            endian,
        }
    }

    pub fn bytes(&mut self, value: &[u8]) -> Result<()> {
        self.append([value])
    }

    pub fn u32(&mut self, value: u32) -> Result<()> {
        self.bytes(&match self.endian {
            Endian::Little => value.to_le_bytes(),
            Endian::Big => value.to_be_bytes(),
        })
    }

    pub fn u64(&mut self, value: u64) -> Result<()> {
        self.bytes(&match self.endian {
            Endian::Little => value.to_le_bytes(),
            Endian::Big => value.to_be_bytes(),
        })
    }

    pub fn count(&mut self, value: usize) -> Result<()> {
        self.u32(u32::try_from(value).map_err(|_| Error::Limit)?)
    }

    pub fn length_prefixed(&mut self, value: &[u8], maximum: usize) -> Result<()> {
        if value.len() > maximum {
            return Err(Error::Limit);
        }
        let length = u32::try_from(value.len()).map_err(|_| Error::Limit)?;
        let prefix = match self.endian {
            Endian::Little => length.to_le_bytes(),
            Endian::Big => length.to_be_bytes(),
        };
        self.append([&prefix, value])
    }

    fn append<const N: usize>(&mut self, parts: [&[u8]; N]) -> Result<()> {
        let length = parts.iter().try_fold(self.bytes.len(), |length, part| {
            length.checked_add(part.len())
        });
        if length.is_none_or(|length| length > self.maximum) {
            return Err(Error::Limit);
        }
        for part in parts {
            self.bytes.extend_from_slice(part);
        }
        Ok(())
    }
}

pub fn decode_hex<const N: usize>(value: &[u8]) -> Result<[u8; N]> {
    if N.checked_mul(2) != Some(value.len()) {
        return Err(Error::InvalidHex);
    }
    let nibble = |byte| match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(Error::InvalidHex),
    };
    let mut bytes = [0; N];
    for (output, pair) in bytes.iter_mut().zip(value.chunks_exact(2)) {
        *output = nibble(pair[0])? << 4 | nibble(pair[1])?;
    }
    Ok(bytes)
}

pub fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut value = String::new();
    for &byte in bytes {
        value.push(char::from(HEX[usize::from(byte >> 4)]));
        value.push(char::from(HEX[usize::from(byte & 15)]));
    }
    value
}

#[cfg(feature = "rest")]
pub mod rest {

    use std::{
        cell::RefCell,
        io::{Read, Write},
        net::{SocketAddr, TcpStream},
        os::fd::FromRawFd,
        path::{Path, PathBuf},
        rc::Rc,
        sync::Arc,
        time::{Duration, Instant, SystemTime},
    };

    pub use uring_runtime::Operation;

    use uring_runtime::{Scope as _, reactor::descriptor::Descriptor};

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Error {
        InvalidConfiguration,

        InvalidRequest,

        Unauthorized,

        Unavailable,

        Overloaded,

        Io,

        Internal,
    }

    pub type Result<T, E = Error> = std::result::Result<T, E>;

    pub trait Scope: uring_runtime::Scope {
        fn deadline(&self) -> Instant;

        fn narrowed(&self, until: Instant) -> Self;
    }

    pub trait Io: 'static {
        type Error: Copy + Send + PartialEq + From<Error> + From<uring_runtime::Error> + 'static;

        type Scope: Scope<Error = Self::Error>;

        type Lease: 'static;

        type FileBytes: AsRef<[u8]>;

        fn lease(&self) -> Result<Option<Rc<Self::Lease>>, Self::Error>;

        fn ready<'a>(
            &'a self,
            fd: Rc<Descriptor>,
            read: bool,
            write: bool,
            lease: Option<Rc<Self::Lease>>,
            scope: &'a Self::Scope,
        ) -> Operation<'a, (), Self::Error>;

        fn resolve<'a>(
            &'a self,
            host: &'a str,
            port: u16,
            scope: &'a Self::Scope,
        ) -> Operation<'a, Vec<SocketAddr>, Self::Error>;

        fn read_file<'a>(
            &'a self,
            path: &'a Path,
            limit: usize,
            scope: &'a Self::Scope,
        ) -> Operation<'a, Self::FileBytes, Self::Error>;
    }

    pub struct Config {
        pub url: String,

        pub trust_bundle: PathBuf,

        pub max_trust_bundle: usize,

        pub max_error_body: usize,
    }

    #[derive(Clone, Copy)]
    pub struct Identity<'a> {
        pub certificate_chain: &'a [Vec<u8>],

        pub private_key: &'a [u8],

        pub expires: SystemTime,
    }

    #[derive(Clone, Copy)]
    pub enum Method {
        Get,

        Post,
    }

    pub struct Request<'a> {
        pub method: Method,

        pub path: &'a str,

        pub bearer: Option<&'a str>,

        pub header: Option<(&'a str, &'a str)>,

        pub body: &'a [u8],

        pub limit: usize,
    }

    pub struct Transport<I: Io + ?Sized> {
        config: Config,

        io: RefCell<Option<Rc<I>>>,

        idle: Rc<RefCell<Option<Connection<I>>>>,

        owner: RefCell<Rc<()>>,
    }

    pub struct Connection<I: Io + ?Sized> {
        socket: Connected<I>,

        tls: rustls::ClientConnection,

        io: Rc<I>,

        host: String,

        authentication: Authentication<I>,

        max_error_body: usize,
    }

    pub struct Response {
        pub status: u16,

        pub body: Vec<u8>,

        pub retry_after: Option<Duration>,
    }

    enum Authentication<I: Io + ?Sized> {
        Bootstrap,

        Authenticated(Authenticated<I>),
    }

    struct Authenticated<I: Io + ?Sized> {
        expires: SystemTime,

        epoch: [u8; 32],

        idle: std::rc::Weak<RefCell<Option<Connection<I>>>>,

        owner: std::rc::Weak<()>,

        idle_since: Instant,

        retire_at: Instant,
    }

    const AUTHENTICATED_AGE_MIN: Duration = Duration::from_secs(240);

    const AUTHENTICATED_AGE_JITTER_MS: u64 = 60_000;

    fn authenticated_age(random: u64) -> Duration {
        AUTHENTICATED_AGE_MIN + Duration::from_millis(random % (AUTHENTICATED_AGE_JITTER_MS + 1))
    }

    enum Stream {
        Real(TcpStream),

        #[cfg(feature = "simulation")]
        Sim(Rc<Descriptor>),
    }

    impl Read for Stream {
        fn read(&mut self, bytes: &mut [u8]) -> std::io::Result<usize> {
            match self {
                Self::Real(stream) => stream.read(bytes),
                #[cfg(feature = "simulation")]
                Self::Sim(fd) => fd.try_recv(bytes),
            }
        }
    }

    impl Write for Stream {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            match self {
                Self::Real(stream) => stream.write(bytes),
                #[cfg(feature = "simulation")]
                Self::Sim(fd) => fd.try_send(bytes),
            }
        }

        fn flush(&mut self) -> std::io::Result<()> {
            match self {
                Self::Real(stream) => stream.flush(),
                #[cfg(feature = "simulation")]
                Self::Sim(_) => Ok(()),
            }
        }
    }

    impl Drop for Response {
        fn drop(&mut self) {
            use zeroize::Zeroize;
            self.body.zeroize();
        }
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
                Some((host, port)) => {
                    (host, port.parse().map_err(|_| Error::InvalidConfiguration)?)
                }
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

    impl<I: Io + ?Sized> Transport<I> {
        pub fn new(config: Config) -> Self {
            Self {
                config,
                io: RefCell::new(None),
                idle: Rc::new(RefCell::new(None)),
                owner: RefCell::new(Rc::new(())),
            }
        }

        pub fn attach_io(&self, io: Rc<I>) {
            *self.owner.borrow_mut() = Rc::new(());
            self.idle.borrow_mut().take();
            *self.io.borrow_mut() = Some(io);
        }

        pub fn io(&self) -> Result<Rc<I>, I::Error> {
            self.io
                .borrow()
                .clone()
                .ok_or_else(|| Error::InvalidConfiguration.into())
        }

        pub fn close_idle(&self) {
            self.idle.borrow_mut().take();
        }

        pub fn connect<'a>(
            &'a self,
            identity: Option<Identity<'a>>,
            scope: &'a I::Scope,
        ) -> Operation<'a, Connection<I>, I::Error> {
            Box::pin(async move {
                scope.check()?;
                if identity.is_some_and(|i| uring_runtime::environment::wall_now() >= i.expires) {
                    return Err(Error::Unauthorized.into());
                }
                let endpoint = endpoint(&self.config.url)?;
                let io = self.io()?;
                let owner = Rc::downgrade(&self.owner.borrow());
                let trust = io
                    .read_file(
                        &self.config.trust_bundle,
                        self.config.max_trust_bundle,
                        scope,
                    )
                    .await?;
                let epoch = trust_epoch(trust.as_ref(), identity);
                if let Some(identity) = identity {
                    validate_client_key(identity)?;
                }
                if owner.upgrade().is_some()
                    && let Some(connection) = self.checkout(identity, epoch, scope)
                {
                    return Ok(connection);
                }
                let config = tls_config(trust.as_ref(), identity)?;
                let addresses = if let Ok(ip) = endpoint.host.parse() {
                    vec![SocketAddr::new(ip, endpoint.port)]
                } else {
                    io.resolve(&endpoint.host, endpoint.port, scope).await?
                };
                if addresses.is_empty() || addresses.len() > 64 {
                    return Err(Error::Unavailable.into());
                }
                let socket = connect_addresses(io.as_ref(), &addresses, scope).await?;
                let server = rustls::pki_types::ServerName::try_from(endpoint.host)
                    .map_err(|_| Error::InvalidConfiguration)?;
                let mut tls = rustls::ClientConnection::new(Arc::new(config), server)
                    .map_err(|_| Error::Unauthorized)?;
                tls.set_buffer_limit(Some(64 * 1024));
                let authentication = self.authentication(identity, epoch, owner)?;
                let mut connection = Connection {
                    socket,
                    tls,
                    io,
                    host: endpoint.authority,
                    authentication,
                    max_error_body: self.config.max_error_body,
                };
                while connection.tls.is_handshaking() {
                    connection.step(scope).await?;
                }
                Ok(connection)
            })
        }

        fn checkout(
            &self,
            identity: Option<Identity<'_>>,
            epoch: [u8; 32],
            scope: &I::Scope,
        ) -> Option<Connection<I>> {
            let identity = identity?;
            scope.check().ok()?;
            let mut idle = self.idle.borrow_mut();
            let mut connection = idle.take()?;
            if let Authentication::Authenticated(auth) = &connection.authentication
                && auth.epoch == epoch
                && auth.expires == identity.expires
                && auth.owner.upgrade().is_some()
                && connection.within_max_age()
                && uring_runtime::environment::now().saturating_duration_since(auth.idle_since)
                    < Duration::from_secs(20)
                && connection.check(scope).is_ok()
                && connection.idle_healthy()
            {
                Some(connection)
            } else {
                None
            }
        }

        fn authentication(
            &self,
            identity: Option<Identity<'_>>,
            epoch: [u8; 32],
            owner: std::rc::Weak<()>,
        ) -> Result<Authentication<I>> {
            let Some(identity) = identity else {
                return Ok(Authentication::Bootstrap);
            };
            let mut random = [0; 8];
            uring_runtime::environment::fill_random(&mut random).map_err(|_| Error::Io)?;
            let retire_at =
                uring_runtime::environment::now() + authenticated_age(u64::from_ne_bytes(random));
            Ok(Authentication::Authenticated(Authenticated {
                expires: identity.expires,
                epoch,
                idle: Rc::downgrade(&self.idle),
                owner,
                idle_since: uring_runtime::environment::now(),
                retire_at,
            }))
        }
    }

    struct Connected<I: Io + ?Sized> {
        charge: Option<Rc<I::Lease>>,

        stream: Stream,

        fd: Rc<Descriptor>,
    }

    async fn connect_addresses<I: Io + ?Sized>(
        io: &I,
        addresses: &[SocketAddr],
        scope: &I::Scope,
    ) -> Result<Connected<I>, I::Error> {
        for (index, address) in addresses.iter().enumerate() {
            scope.check()?;
            // Reserve a share for every remaining address and one for TLS. A local
            // blackhole must not consume the parent's entire connection deadline.
            let now = uring_runtime::environment::now();
            let share = scope.deadline().saturating_duration_since(now)
                / (addresses.len() - index + 1) as u32;
            let attempt = scope.narrowed(now + share.min(Duration::from_secs(5)));
            let charge = io.lease()?;
            #[cfg(feature = "simulation")]
            if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
                if let Ok(fd) = sim.connect(uring_runtime::reactor::SocketAddress::Inet(*address)) {
                    let fd = Rc::new(fd);
                    return Ok(Connected {
                        charge,
                        stream: Stream::Sim(fd.clone()),
                        fd,
                    });
                }
                continue;
            }
            if let Ok(stream) = connect_socket(*address) {
                let fd = Rc::new(Descriptor::from(stream.try_clone().map_err(|_| Error::Io)?));
                let ready = io
                    .ready(fd.clone(), false, true, charge.clone(), &attempt)
                    .await;
                // Cancellation and overall expiry always win over local retry.
                scope.check()?;
                match ready {
                    Ok(()) => {}
                    Err(error)
                        if error == uring_runtime::Error::DeadlineExceeded.into()
                            || error == Error::Io.into()
                            || error == Error::Unavailable.into() =>
                    {
                        continue;
                    }
                    Err(error) => return Err(error),
                }
                if stream.take_error().map_err(|_| Error::Io)?.is_none() {
                    return Ok(Connected {
                        charge,
                        stream: Stream::Real(stream),
                        fd,
                    });
                }
            }
        }
        scope.check()?;
        Err(Error::Unavailable.into())
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

    impl<I: Io + ?Sized> Connection<I> {
        fn idle_healthy(&mut self) -> bool {
            // Read only what is ready. Bound work even if the peer keeps sending tickets.
            for _ in 0..16 {
                let Ok(state) = self.tls.process_new_packets() else {
                    return false;
                };
                if state.peer_has_closed() || state.plaintext_bytes_to_read() != 0 {
                    return false;
                }
                match self.tls.read_tls(&mut self.socket.stream) {
                    Ok(0) => return false,
                    Ok(_) => (),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => return true,
                    Err(_) => return false,
                }
            }
            false
        }

        pub fn request<'a>(
            mut self,
            request: Request<'a>,
            scope: &'a I::Scope,
        ) -> Operation<'a, Response, I::Error> {
            Box::pin(async move {
                let Request {
                    method,
                    path,
                    bearer: token,
                    header,
                    body,
                    limit,
                } = request;
                self.validate_request(method, path, token)?;
                let request = request_head(&self.host, method, path, token, body.len(), header)?;
                self.send(request.as_bytes(), body, scope).await?;
                let completed = self.receive_response(limit, scope).await?;
                self.recycle(&completed);
                Ok(Response {
                    status: completed.head.status,
                    body: completed.body.to_vec(),
                    retry_after: completed.head.retry_after,
                })
            })
        }

        fn validate_request(&self, method: Method, path: &str, bearer: Option<&str>) -> Result<()> {
            // Bearer credentials belong only to enrollment, never an established route.
            let allowed = match self.authentication {
                Authentication::Bootstrap => {
                    matches!(method, Method::Post)
                        && path == "/v1/bootstrap"
                        && bearer.is_some_and(|token| !token.is_empty())
                }
                Authentication::Authenticated(_) => {
                    matches!(method, Method::Get)
                        && matches!(
                            path.split_once('?').map_or(path, |(route, _)| route),
                            "/v1/snapshot" | "/v1/keyring"
                        )
                        && bearer.is_none()
                }
            };
            if allowed {
                Ok(())
            } else {
                Err(Error::InvalidRequest)
            }
        }

        fn within_max_age(&self) -> bool {
            match &self.authentication {
                Authentication::Bootstrap => true,
                Authentication::Authenticated(auth) => {
                    uring_runtime::environment::now() < auth.retire_at
                }
            }
        }

        fn check(&self, scope: &I::Scope) -> Result<(), I::Error> {
            scope.check()?;
            if let Authentication::Authenticated(auth) = &self.authentication
                && uring_runtime::environment::wall_now() >= auth.expires
            {
                return Err(Error::Unauthorized.into());
            }
            Ok(())
        }

        async fn step(&mut self, scope: &I::Scope) -> Result<(), I::Error> {
            self.check(scope)?;
            // Even a continuously readable peer must yield to other owner work.
            uring_runtime::drivers::yield_now().await;
            self.check(scope)?;
            let mut progress = false;
            if self.tls.wants_write() {
                match self.tls.write_tls(&mut self.socket.stream) {
                    Ok(0) => return Err(Error::Io.into()),
                    Ok(_) => progress = true,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(_) => return Err(Error::Io.into()),
                }
            }
            if self.tls.wants_read() {
                match self.tls.read_tls(&mut self.socket.stream) {
                    Ok(0) => return Err(Error::Io.into()),
                    Ok(_) => {
                        self.tls.process_new_packets().map_err(tls_failure)?;
                        progress = true;
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(_) => return Err(Error::Io.into()),
                }
            }
            if !progress {
                let mut bounded = scope.clone();
                if let Authentication::Authenticated(auth) = &self.authentication {
                    let remaining = auth
                        .expires
                        .duration_since(uring_runtime::environment::wall_now())
                        .map_err(|_| Error::Unauthorized)?;
                    bounded = scope.narrowed(uring_runtime::environment::now() + remaining);
                }
                self.io
                    .ready(
                        self.socket.fd.clone(),
                        self.tls.wants_read(),
                        self.tls.wants_write(),
                        self.socket.charge.clone(),
                        &bounded,
                    )
                    .await?;
            }
            Ok(())
        }

        async fn send(
            &mut self,
            head: &[u8],
            body: &[u8],
            scope: &I::Scope,
        ) -> Result<(), I::Error> {
            for bytes in [head, body] {
                let mut offset = 0;
                while offset < bytes.len() {
                    self.check(scope)?;
                    match self.tls.writer().write(&bytes[offset..]) {
                        Ok(n) if n != 0 => offset += n,
                        Ok(_) => (),
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(_) => return Err(Error::Io.into()),
                    }
                    self.step(scope).await?;
                }
            }
            while self.tls.wants_write() {
                self.step(scope).await?;
            }
            Ok(())
        }

        async fn receive_response(
            &mut self,
            limit: usize,
            scope: &I::Scope,
        ) -> Result<CompletedResponse, I::Error> {
            let mut received = zeroize::Zeroizing::new(Vec::new());
            let mut scratch = zeroize::Zeroizing::new([0; 16384]);
            let head = loop {
                if let Some(head) = parse_head(&received, limit, self.max_error_body)? {
                    break head;
                }
                if received.len() >= 16384 {
                    return Err(Error::Overloaded.into());
                }
                self.receive(&mut received, &mut scratch[..], scope).await?;
            };
            let body = match head.framing {
                Framing::Length(length) => {
                    let end = length_frame_end(head.header_len, length)?;
                    loop {
                        if received.len() > end {
                            return Err(Error::InvalidRequest.into());
                        }
                        if received.len() == end {
                            break;
                        }
                        self.receive(&mut received, &mut scratch[..], scope).await?;
                    }
                    zeroize::Zeroizing::new(received[head.header_len..].to_vec())
                }
                Framing::Chunked { bound } => {
                    self.receive_chunked(
                        &received[head.header_len..],
                        bound,
                        &mut scratch[..],
                        scope,
                    )
                    .await?
                }
            };
            self.check(scope)?;
            // A TLS record can contain more plaintext than the last bounded receive.
            let reusable = match self.tls.reader().read(&mut scratch[..1]) {
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => true,
                Ok(0) => false,
                Ok(_) => return Err(Error::InvalidRequest.into()),
                Err(_) => false,
            };
            Ok(CompletedResponse {
                head,
                body,
                reusable,
            })
        }

        fn recycle(mut self, completed: &CompletedResponse) {
            // An aged connection finishes its in-flight response, then retires.
            if completed.reusable
                && !completed.head.close
                && matches!(completed.head.status, 200 | 204)
                && self.within_max_age()
                && let Authentication::Authenticated(auth) = &mut self.authentication
                && auth.owner.upgrade().is_some()
                && let Some(idle) = auth.idle.upgrade()
            {
                auth.idle_since = uring_runtime::environment::now();
                *idle.borrow_mut() = Some(self);
            }
        }

        async fn receive_chunked(
            &mut self,
            initial: &[u8],
            bound: usize,
            scratch: &mut [u8],
            scope: &I::Scope,
        ) -> Result<zeroize::Zeroizing<Vec<u8>>, I::Error> {
            let mut raw = zeroize::Zeroizing::new(initial.to_vec());
            let mut body = zeroize::Zeroizing::new(Vec::new());
            loop {
                let line_end = loop {
                    if let Some(n) = raw.windows(2).position(|w| w == b"\r\n") {
                        break n;
                    }
                    if raw.len() > 128 {
                        return Err(Error::InvalidRequest.into());
                    }
                    self.receive(&mut raw, scratch, scope).await?;
                };
                if line_end == 0
                    || line_end > 16
                    || !raw[..line_end].iter().all(u8::is_ascii_hexdigit)
                {
                    return Err(Error::InvalidRequest.into());
                }
                let length = usize::from_str_radix(
                    std::str::from_utf8(&raw[..line_end]).map_err(|_| Error::InvalidRequest)?,
                    16,
                )
                .map_err(|_| Error::Overloaded)?;
                raw.drain(..line_end + 2);
                if length > bound - body.len() {
                    return Err(Error::Overloaded.into());
                }
                let mut remaining = length;
                while remaining != 0 {
                    if raw.is_empty() {
                        self.receive(&mut raw, scratch, scope).await?;
                    }
                    let n = remaining.min(raw.len());
                    append_sensitive(&mut body, &raw[..n]);
                    raw.drain(..n);
                    remaining -= n;
                }
                while raw.len() < 2 {
                    self.receive(&mut raw, scratch, scope).await?;
                }
                if &raw[..2] != b"\r\n" {
                    return Err(Error::InvalidRequest.into());
                }
                raw.drain(..2);
                if length == 0 {
                    if !raw.is_empty() {
                        return Err(Error::InvalidRequest.into());
                    }
                    return Ok(body);
                }
            }
        }

        async fn receive(
            &mut self,
            into: &mut zeroize::Zeroizing<Vec<u8>>,
            scratch: &mut [u8],
            scope: &I::Scope,
        ) -> Result<(), I::Error> {
            loop {
                self.check(scope)?;
                match self.tls.reader().read(scratch) {
                    Ok(0) => return Err(Error::Io.into()),
                    Ok(n) => {
                        append_sensitive(into, &scratch[..n]);
                        return Ok(());
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        self.step(scope).await?
                    }
                    Err(_) => return Err(Error::Io.into()),
                }
            }
        }
    }

    fn request_head(
        host: &str,
        method: Method,
        path: &str,
        token: Option<&str>,
        body_length: usize,
        header: Option<(&str, &str)>,
    ) -> Result<zeroize::Zeroizing<String>> {
        const MAX_HEAD: usize = 16384;

        if !path.starts_with('/')
            || path.bytes().any(|b| b <= 32 || b >= 127)
            || host.bytes().any(|b| b <= 32 || b >= 127)
            || token.is_some_and(|t| t.bytes().any(|b| b <= 32 || b >= 127))
            || header.is_some_and(|(name, value)| !valid_header(name, value))
        {
            return Err(Error::InvalidRequest);
        }
        // Bound allocation before copying bearer credentials. Reserve once, so a
        // String growth cannot free an allocation still containing a token.
        let capacity = [
            host.len(),
            path.len(),
            token.map_or(0, str::len),
            header.map_or(0, |(name, _)| name.len()),
            header.map_or(0, |(_, value)| value.len()),
        ]
        .into_iter()
        .try_fold(256usize, |total, len| total.checked_add(len))
        .filter(|size| *size <= MAX_HEAD)
        .ok_or(Error::Overloaded)?;
        let mut request = zeroize::Zeroizing::new(String::with_capacity(capacity));
        use std::fmt::Write;
        let method = match method {
            Method::Get => "GET",
            Method::Post => "POST",
        };
        write!(request, "{method} {path} HTTP/1.1\r\nHost: {host}\r\nAccept: application/json\r\nContent-Type: application/json\r\nContent-Length: {body_length}\r\nConnection: keep-alive\r\n")
        .map_err(|_| Error::Internal)?;
        if let Some(token) = token {
            request.push_str("Authorization: Bearer ");
            request.push_str(token);
            request.push_str("\r\n");
        }
        if let Some((name, value)) = header {
            request.push_str(name);
            request.push_str(": ");
            request.push_str(value);
            request.push_str("\r\n");
        }
        request.push_str("\r\n");
        Ok(request)
    }

    fn append_sensitive(into: &mut zeroize::Zeroizing<Vec<u8>>, bytes: &[u8]) {
        if into.capacity() - into.len() < bytes.len() {
            let capacity = (into.len() + bytes.len()).max(into.capacity().saturating_mul(2));
            let mut next = zeroize::Zeroizing::new(Vec::with_capacity(capacity));
            next.extend_from_slice(into);
            std::mem::swap(into, &mut next);
        }
        into.extend_from_slice(bytes);
    }

    fn tls_failure(error: rustls::Error) -> Error {
        match error {
            rustls::Error::AlertReceived(rustls::AlertDescription::InternalError) => {
                Error::Unavailable
            }
            _ => Error::Unauthorized,
        }
    }

    fn trust_epoch(trust: &[u8], identity: Option<Identity<'_>>) -> [u8; 32] {
        use sha2::Digest;
        let mut epoch = sha2::Sha256::new();
        epoch.update(trust);
        if let Some(identity) = identity {
            for certificate in identity.certificate_chain {
                epoch.update((certificate.len() as u64).to_be_bytes());
                epoch.update(certificate);
            }
        }
        epoch.finalize().into()
    }

    fn validate_client_key(identity: Identity<'_>) -> Result<()> {
        let private = rustls::pki_types::PrivatePkcs8KeyDer::from(identity.private_key).into();
        let key = rustls::crypto::ring::sign::any_supported_type(&private)
            .map_err(|_| Error::Unauthorized)?;
        let leaf = identity
            .certificate_chain
            .first()
            .ok_or(Error::Unauthorized)?;
        rustls::sign::CertifiedKey::new(
            vec![rustls::pki_types::CertificateDer::from(leaf.clone())],
            key,
        )
        .keys_match()
        .map_err(|_| Error::Unauthorized)
    }

    fn tls_config(
        mut trust: &[u8],
        identity: Option<Identity<'_>>,
    ) -> Result<rustls::ClientConfig> {
        let mut roots = rustls::RootCertStore::empty();
        for cert in rustls_pemfile::certs(&mut trust) {
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
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| Error::Unauthorized)?
        .with_root_certificates(roots);
        let mut config = if let Some(identity) = identity {
            builder
                .with_client_auth_cert(
                    identity
                        .certificate_chain
                        .iter()
                        .cloned()
                        .map(rustls::pki_types::CertificateDer::from)
                        .collect(),
                    rustls::pki_types::PrivatePkcs8KeyDer::from(identity.private_key.to_vec())
                        .into(),
                )
                .map_err(|_| Error::Unauthorized)?
        } else {
            builder.with_no_client_auth()
        };
        config.alpn_protocols = vec![b"http/1.1".to_vec()];
        config.resumption = rustls::client::Resumption::disabled();
        Ok(config)
    }

    #[derive(Debug, Eq, PartialEq)]
    enum Framing {
        Length(usize),

        Chunked { bound: usize },
    }

    #[derive(Debug, Eq, PartialEq)]
    struct ResponseHead {
        status: u16,

        header_len: usize,

        framing: Framing,

        retry_after: Option<Duration>,

        close: bool,
    }

    struct CompletedResponse {
        head: ResponseHead,

        body: zeroize::Zeroizing<Vec<u8>>,

        reusable: bool,
    }

    fn length_frame_end(header_len: usize, body_len: usize) -> Result<usize> {
        header_len.checked_add(body_len).ok_or(Error::Overloaded)
    }

    fn parse_head(
        bytes: &[u8],
        limit: usize,
        max_error_body: usize,
    ) -> Result<Option<ResponseHead>> {
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
        let mut close = false;
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
            if h.name.eq_ignore_ascii_case("connection") {
                // Defer invalid UTF-8 until after framing validation, as before.
                if let Ok(value) = std::str::from_utf8(h.value) {
                    close |= value
                        .split(',')
                        .any(|token| token.trim().eq_ignore_ascii_case("close"));
                }
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
        let bound = if status == 200 { limit } else { max_error_body };
        if size > bound {
            return Err(Error::Overloaded);
        }
        // The old request path required UTF-8 even in ignored headers. Keep that
        // acceptance rule after size/framing checks, without reparsing Connection.
        std::str::from_utf8(&bytes[..length]).map_err(|_| Error::InvalidRequest)?;
        Ok(Some(ResponseHead {
            status,
            header_len: length,
            framing: if chunked {
                Framing::Chunked { bound }
            } else {
                Framing::Length(size)
            },
            retry_after: retry,
            close,
        }))
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
        if time.len() != 3 || !(1970..=9999).contains(&year) {
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
        Ok(target
            .duration_since(uring_runtime::environment::wall_now())
            .unwrap_or_default())
    }

    fn valid_header(name: &str, value: &str) -> bool {
        !name.is_empty()
            && name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b))
            && value.bytes().all(|b| (32..127).contains(&b))
            && ![
                "host",
                "authorization",
                "proxy-authorization",
                "content-length",
                "transfer-encoding",
                "content-type",
                "content-encoding",
                "accept",
                "accept-encoding",
                "connection",
                "te",
                "trailer",
                "upgrade",
                "expect",
            ]
            .iter()
            .any(|reserved| name.eq_ignore_ascii_case(reserved))
    }

    pub mod dns {
        use super::{Error, Io, Result, Scope};
        use std::{
            net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, UdpSocket},
            rc::Rc,
            time::Duration,
        };
        use uring_runtime::{Scope as _, reactor::descriptor::Descriptor};

        pub async fn resolve<I: Io + ?Sized>(
            io: &I,
            host: &str,
            port: u16,
            scope: &I::Scope,
        ) -> Result<Vec<SocketAddr>, I::Error> {
            scope.check()?;
            // Configuration and network I/O both use the owner-local reactor.
            let config = io
                .read_file(std::path::Path::new("/etc/resolv.conf"), 64 * 1024, scope)
                .await?;
            let text =
                std::str::from_utf8(config.as_ref()).map_err(|_| Error::InvalidConfiguration)?;
            let mut servers = Vec::new();
            let mut search = Vec::new();
            let mut ndots = 1;
            for line in text.lines() {
                let mut fields = line
                    .split(['#', ';'])
                    .next()
                    .unwrap_or("")
                    .split_whitespace();
                match fields.next() {
                    Some("nameserver") => {
                        if let Some(ip) = fields.next().and_then(|s| s.parse::<IpAddr>().ok()) {
                            servers.push(SocketAddr::new(ip, 53));
                        }
                    }
                    Some("search" | "domain") => {
                        search = fields.take(6).map(str::to_owned).collect()
                    }
                    Some("options") => {
                        for option in fields {
                            if let Some(n) = option
                                .strip_prefix("ndots:")
                                .and_then(|s| s.parse::<usize>().ok())
                            {
                                ndots = n.min(15);
                            }
                        }
                    }
                    _ => (),
                }
            }
            servers.truncate(3);
            if servers.is_empty() {
                return Err(Error::Unavailable.into());
            }
            let mut names = Vec::new();
            let absolute_first =
                host.ends_with('.') || host.bytes().filter(|b| *b == b'.').count() >= ndots;
            if absolute_first {
                names.push(host.trim_end_matches('.').to_owned());
            }
            if !host.ends_with('.') {
                for suffix in search {
                    names.push(format!("{host}.{suffix}"));
                }
            }
            if !absolute_first {
                names.push(host.to_owned());
            }
            for name in names {
                for server in &servers {
                    let mut addresses = Vec::new();
                    for kind in [1u16, 28] {
                        let attempt = scope
                            .narrowed(uring_runtime::environment::now() + Duration::from_secs(2));
                        match query(io, *server, &name, kind, &attempt).await {
                            Ok(ips) => addresses
                                .extend(ips.into_iter().map(|ip| SocketAddr::new(ip, port))),
                            Err(error) if error == uring_runtime::Error::Cancelled.into() => {
                                return Err(error);
                            }
                            Err(_) => {
                                scope.check()?;
                            }
                        }
                    }
                    if !addresses.is_empty() {
                        addresses.truncate(64);
                        return Ok(addresses);
                    }
                }
            }
            Err(Error::Unavailable.into())
        }

        enum Datagram {
            Real(UdpSocket),

            #[cfg(feature = "simulation")]
            Sim(Rc<Descriptor>),
        }

        impl Datagram {
            fn connect(server: SocketAddr) -> Result<(Self, Rc<Descriptor>)> {
                #[cfg(feature = "simulation")]
                if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
                    let fd = sim
                        .bind_datagram(
                            if server.is_ipv4() {
                                "127.0.0.1:0"
                            } else {
                                "[::1]:0"
                            }
                            .parse()
                            .unwrap(),
                        )
                        .map_err(|_| Error::Io)?;
                    let Some(handle) = fd.as_sim() else {
                        unreachable!()
                    };
                    handle.connect_datagram(server).map_err(|_| Error::Io)?;
                    let fd = Rc::new(fd);
                    return Ok((Self::Sim(fd.clone()), fd));
                }
                let socket = UdpSocket::bind(if server.is_ipv4() {
                    "0.0.0.0:0"
                } else {
                    "[::]:0"
                })
                .map_err(|_| Error::Io)?;
                socket.set_nonblocking(true).map_err(|_| Error::Io)?;
                socket.connect(server).map_err(|_| Error::Io)?;
                let fd = Rc::new(Descriptor::from(socket.try_clone().map_err(|_| Error::Io)?));
                Ok((Self::Real(socket), fd))
            }

            fn send(&self, bytes: &[u8]) -> std::io::Result<usize> {
                match self {
                    Self::Real(socket) => socket.send(bytes),
                    #[cfg(feature = "simulation")]
                    Self::Sim(fd) => {
                        let Some(h) = fd.as_sim() else { unreachable!() };
                        h.send_datagram(bytes)
                    }
                }
            }

            fn recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
                match self {
                    Self::Real(socket) => socket.recv(bytes),
                    #[cfg(feature = "simulation")]
                    Self::Sim(fd) => {
                        let Some(h) = fd.as_sim() else { unreachable!() };
                        h.recv_from(bytes).map(|(n, _)| n)
                    }
                }
            }
        }

        async fn query<I: Io + ?Sized>(
            io: &I,
            server: SocketAddr,
            name: &str,
            kind: u16,
            scope: &I::Scope,
        ) -> Result<Vec<IpAddr>, I::Error> {
            let mut id = [0; 2];
            uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
            let mut request = vec![id[0], id[1], 1, 0, 0, 1, 0, 0, 0, 0, 0, 0];
            if name.len() > 253 {
                return Err(Error::InvalidConfiguration.into());
            }
            for label in name.split('.') {
                if label.is_empty() || label.len() > 63 || !label.is_ascii() {
                    return Err(Error::InvalidConfiguration.into());
                }
                request.push(label.len() as u8);
                request.extend_from_slice(label.as_bytes());
            }
            request.push(0);
            request.extend_from_slice(&kind.to_be_bytes());
            request.extend_from_slice(&[0, 1]);
            let charge = io.lease()?;
            let (socket, fd) = Datagram::connect(server)?;
            loop {
                scope.check()?;
                match socket.send(&request) {
                    Ok(n) if n == request.len() => break,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        io.ready(fd.clone(), false, true, charge.clone(), scope)
                            .await?
                    }
                    _ => return Err(Error::Io.into()),
                }
            }
            let mut response = [0; 4096];
            loop {
                scope.check()?;
                match socket.recv(&mut response) {
                    Ok(n) => return parse(&response[..n], &request, kind).map_err(Into::into),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        io.ready(fd.clone(), true, false, charge.clone(), scope)
                            .await?
                    }
                    _ => return Err(Error::Io.into()),
                }
            }
        }

        fn u16_at(b: &[u8], p: usize) -> Result<u16> {
            Ok(u16::from_be_bytes(
                b.get(p..p + 2)
                    .ok_or(Error::InvalidRequest)?
                    .try_into()
                    .unwrap(),
            ))
        }

        fn skip_name(b: &[u8], p: &mut usize) -> Result<()> {
            for _ in 0..128 {
                let n = *b.get(*p).ok_or(Error::InvalidRequest)?;
                *p += 1;
                if n == 0 {
                    return Ok(());
                }
                if n & 0xc0 == 0xc0 {
                    b.get(*p).ok_or(Error::InvalidRequest)?;
                    *p += 1;
                    return Ok(());
                }
                if n > 63 {
                    return Err(Error::InvalidRequest);
                }
                *p += n as usize;
                if *p > b.len() {
                    return Err(Error::InvalidRequest);
                }
            }
            Err(Error::InvalidRequest)
        }

        fn parse(b: &[u8], request: &[u8], kind: u16) -> Result<Vec<IpAddr>> {
            if b.len() < request.len()
                || b[..2] != request[..2]
                || b[2] & 0xfa != 0x80
                || b[3] & 0x0f != 0
                || u16_at(b, 4)? != 1
                || b[12..request.len()] != request[12..]
            {
                return Err(Error::Unavailable);
            }
            let mut p = request.len();
            let count = u16_at(b, 6)? as usize;
            if count > 128 {
                return Err(Error::Overloaded);
            }
            let mut addresses = Vec::new();
            for _ in 0..count {
                skip_name(b, &mut p)?;
                let rr = u16_at(b, p)?;
                let class = u16_at(b, p + 2)?;
                let len = u16_at(b, p + 8)? as usize;
                p += 10;
                let data = b.get(p..p + len).ok_or(Error::InvalidRequest)?;
                p += len;
                if class == 1 && rr == kind {
                    match (rr, len) {
                        (1, 4) => addresses.push(IpAddr::V4(Ipv4Addr::from(
                            <[u8; 4]>::try_from(data).unwrap(),
                        ))),
                        (28, 16) => addresses.push(IpAddr::V6(Ipv6Addr::from(
                            <[u8; 16]>::try_from(data).unwrap(),
                        ))),
                        _ => return Err(Error::InvalidRequest),
                    }
                }
            }
            Ok(addresses)
        }

        #[cfg(test)]
        mod tests {
            use super::*;
            use crate::{rest::Operation, testing};
            use std::{
                cell::{Cell, RefCell},
                rc::Weak,
                task::{Context, Poll},
            };

            #[derive(Default)]
            struct DnsIo {
                deny: bool,
                attempts: Cell<usize>,
                lease: RefCell<Weak<()>>,
                retained: RefCell<Option<(Rc<Descriptor>, Rc<()>)>>,
                ready: Cell<bool>,
            }

            impl Io for DnsIo {
                type Error = testing::Error;
                type Scope = testing::TestScope;
                type Lease = ();
                type FileBytes = Vec<u8>;

                fn lease(&self) -> Result<Option<Rc<()>>, Self::Error> {
                    self.attempts.set(self.attempts.get() + 1);
                    if self.deny {
                        return Err(Error::Overloaded.into());
                    }
                    let lease = Rc::new(());
                    *self.lease.borrow_mut() = Rc::downgrade(&lease);
                    Ok(Some(lease))
                }

                fn ready<'a>(
                    &'a self,
                    fd: Rc<Descriptor>,
                    read: bool,
                    write: bool,
                    lease: Option<Rc<()>>,
                    _: &'a Self::Scope,
                ) -> Operation<'a, (), Self::Error> {
                    assert!(read);
                    assert!(!write);
                    let lease = lease.expect("DNS readiness must inherit admission");
                    assert_eq!(Rc::strong_count(&lease), 2, "query retains its lease");
                    *self.retained.borrow_mut() = Some((fd, lease));
                    Box::pin(std::future::poll_fn(move |_| {
                        if self.ready.get() {
                            self.retained.borrow_mut().take();
                            Poll::Ready(Ok(()))
                        } else {
                            Poll::Pending
                        }
                    }))
                }

                fn resolve<'a>(
                    &'a self,
                    _: &'a str,
                    _: u16,
                    _: &'a Self::Scope,
                ) -> Operation<'a, Vec<SocketAddr>, Self::Error> {
                    unreachable!("test calls the actual DNS query")
                }

                fn read_file<'a>(
                    &'a self,
                    _: &'a std::path::Path,
                    _: usize,
                    _: &'a Self::Scope,
                ) -> Operation<'a, Vec<u8>, Self::Error> {
                    unreachable!("query does not read resolver configuration")
                }
            }

            #[test]
            fn dns_query_denied_admission_sends_nothing() {
                let server = UdpSocket::bind("127.0.0.1:0").unwrap();
                server.set_nonblocking(true).unwrap();
                let io = DnsIo {
                    deny: true,
                    ..Default::default()
                };
                assert_eq!(
                    futures::executor::block_on(query(
                        &io,
                        server.local_addr().unwrap(),
                        "example.test",
                        1,
                        &testing::scope(),
                    )),
                    Err(Error::Overloaded.into())
                );
                assert_eq!(io.attempts.get(), 1);
                assert!(io.retained.borrow().is_none());
                assert_eq!(
                    server.recv_from(&mut [0; 512]).unwrap_err().kind(),
                    std::io::ErrorKind::WouldBlock
                );
            }

            #[test]
            fn dns_query_lease_lives_through_response_or_cancel_cleanup() {
                for cancel in [false, true] {
                    let server = UdpSocket::bind("127.0.0.1:0").unwrap();
                    server.set_nonblocking(true).unwrap();
                    let io = DnsIo::default();
                    let scope = testing::scope();
                    let mut query = Box::pin(query(
                        &io,
                        server.local_addr().unwrap(),
                        "example.test",
                        1,
                        &scope,
                    ));
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    assert!(query.as_mut().poll(&mut cx).is_pending());
                    assert_eq!(io.attempts.get(), 1);
                    assert_eq!(io.lease.borrow().strong_count(), 2);
                    let mut request = [0; 512];
                    let (n, peer) = server.recv_from(&mut request).unwrap();
                    if cancel {
                        drop(query);
                        assert_eq!(io.lease.borrow().strong_count(), 1);
                        io.retained.borrow_mut().take();
                    } else {
                        let mut response = request[..n].to_vec();
                        response[2] = 0x81;
                        response[3] = 0x80;
                        response[7] = 1;
                        response.extend_from_slice(&[
                            0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 192, 0, 2, 1,
                        ]);
                        server.send_to(&response, peer).unwrap();
                        io.ready.set(true);
                        assert_eq!(
                            query.as_mut().poll(&mut cx),
                            Poll::Ready(Ok(vec!["192.0.2.1".parse::<IpAddr>().unwrap()]))
                        );
                    }
                    assert!(io.lease.borrow().upgrade().is_none());
                }
            }

            #[test]
            fn rejects_truncation_wrong_question_and_malformed_answers() {
                let request = [1, 2, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, b'a', 0, 0, 1, 0, 1];
                let mut response = request.to_vec();
                response[2] = 0x81;
                response[3] = 0x80;
                response[7] = 1;
                response
                    .extend_from_slice(&[0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 192, 0, 2, 1]);
                assert_eq!(
                    parse(&response, &request, 1).unwrap(),
                    vec!["192.0.2.1".parse::<IpAddr>().unwrap()]
                );
                response[2] |= 2;
                assert!(parse(&response, &request, 1).is_err());
                response[2] &= !2;
                response[13] = b'b';
                assert!(parse(&response, &request, 1).is_err());
                response[13] = b'a';
                response.pop();
                assert!(parse(&response, &request, 1).is_err());
            }
        }
    }

    #[cfg(test)]
    use crate::testing;

    #[cfg(test)]
    mod tests {
        use super::*;
        use testing::{FixtureIo, TestIdentity, TestTransport as _};

        #[derive(Clone, Copy, Debug, PartialEq)]
        enum CancelError {
            Runtime(uring_runtime::Error),
            Fixture(testing::Error),
        }

        impl From<uring_runtime::Error> for CancelError {
            fn from(error: uring_runtime::Error) -> Self {
                Self::Runtime(error)
            }
        }

        impl From<Error> for CancelError {
            fn from(error: Error) -> Self {
                Self::Fixture(error.into())
            }
        }

        #[derive(Clone)]
        struct CancelScope {
            cancelled: Rc<std::cell::Cell<bool>>,
            inner: testing::TestScope,
        }

        impl uring_runtime::Scope for CancelScope {
            type Error = CancelError;

            fn check(&self) -> Result<(), Self::Error> {
                if self.cancelled.get() {
                    return Err(uring_runtime::Error::Cancelled.into());
                }
                self.inner.check().map_err(CancelError::Fixture)
            }
        }

        impl Scope for CancelScope {
            fn deadline(&self) -> Instant {
                self.inner.deadline()
            }

            fn narrowed(&self, until: Instant) -> Self {
                Self {
                    cancelled: self.cancelled.clone(),
                    inner: self.inner.narrowed(until),
                }
            }
        }

        struct CancelIo;

        impl Io for CancelIo {
            type Error = CancelError;
            type Scope = CancelScope;
            type Lease = ();
            type FileBytes = <FixtureIo as Io>::FileBytes;

            fn lease(&self) -> Result<Option<Rc<()>>, Self::Error> {
                FixtureIo.lease().map_err(CancelError::Fixture)
            }

            fn ready<'a>(
                &'a self,
                fd: Rc<Descriptor>,
                read: bool,
                write: bool,
                lease: Option<Rc<()>>,
                scope: &'a CancelScope,
            ) -> Operation<'a, (), Self::Error> {
                Box::pin(async move {
                    FixtureIo
                        .ready(fd, read, write, lease, &scope.inner)
                        .await
                        .map_err(CancelError::Fixture)
                })
            }

            fn resolve<'a>(
                &'a self,
                host: &'a str,
                port: u16,
                scope: &'a CancelScope,
            ) -> Operation<'a, Vec<SocketAddr>, Self::Error> {
                Box::pin(async move {
                    FixtureIo
                        .resolve(host, port, &scope.inner)
                        .await
                        .map_err(CancelError::Fixture)
                })
            }

            fn read_file<'a>(
                &'a self,
                path: &'a Path,
                limit: usize,
                scope: &'a CancelScope,
            ) -> Operation<'a, Self::FileBytes, Self::Error> {
                Box::pin(async move {
                    FixtureIo
                        .read_file(path, limit, &scope.inner)
                        .await
                        .map_err(CancelError::Fixture)
                })
            }
        }

        fn connected_pair() -> (
            Transport<CancelIo>,
            Connection<CancelIo>,
            TcpStream,
            rustls::ServerConnection,
            CancelScope,
            testing::Directory,
        ) {
            let (ca, ca_key) = testing::ca();
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
            let cert = rcgen::CertificateParams::new(vec!["127.0.0.1".into()])
                .unwrap()
                .signed_by(&key, &ca, &ca_key)
                .unwrap();
            let mut config = rustls::ServerConfig::builder_with_provider(Arc::new(
                rustls::crypto::ring::default_provider(),
            ))
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(
                vec![cert.der().clone()],
                rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
            )
            .unwrap();
            config.send_tls13_tickets = 0;
            let directory = testing::Directory::new();
            let trust = directory.0.join("ca.crt");
            std::fs::write(&trust, ca.pem()).unwrap();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            listener.set_nonblocking(true).unwrap();
            let server = std::thread::spawn(move || {
                let until = Instant::now() + Duration::from_secs(5);
                let mut socket = loop {
                    match listener.accept() {
                        Ok((socket, _)) => break socket,
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            assert!(Instant::now() < until, "client did not connect");
                            std::thread::yield_now();
                        }
                        Err(error) => panic!("accept: {error}"),
                    }
                };
                socket
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                socket
                    .set_write_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let mut tls = rustls::ServerConnection::new(Arc::new(config)).unwrap();
                while tls.is_handshaking() {
                    tls.complete_io(&mut socket).unwrap();
                }
                (socket, tls)
            });
            let scope = CancelScope {
                cancelled: Rc::new(std::cell::Cell::new(false)),
                inner: testing::scope().narrowed(Instant::now() + Duration::from_secs(5)),
            };
            let transport = Transport::new(testing::config(format!("https://{address}"), trust));
            transport.attach_io(Rc::new(CancelIo));
            let mut connection =
                futures::executor::block_on(transport.connect(None, &scope)).unwrap();
            while connection.tls.wants_write() {
                futures::executor::block_on(connection.step(&scope)).unwrap();
            }
            let (socket, mut tls) = server.join().unwrap();
            socket.set_nonblocking(true).unwrap();
            assert_eq!(
                socket.peek(&mut [0; 1]).unwrap_err().kind(),
                std::io::ErrorKind::WouldBlock
            );
            assert_eq!(
                tls.reader().read(&mut [0; 1]).unwrap_err().kind(),
                std::io::ErrorKind::WouldBlock
            );
            socket.set_nonblocking(false).unwrap();
            (transport, connection, socket, tls, scope, directory)
        }

        fn request_at_yield(cancel: bool) {
            use std::task::Context;
            let (_transport, connection, mut socket, mut tls, scope, _directory) = connected_pair();
            let mut request = connection.request(
                testing::request(Method::Post, "/v1/bootstrap", Some("test-token"), 1024),
                &scope,
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(request.as_mut().poll(&mut cx).is_pending());
            scope.cancelled.set(cancel);
            let result = request.as_mut().poll(&mut cx);
            if cancel {
                drop(request);
                // EOF must follow the handshake directly, with no request records.
                assert_eq!(socket.read(&mut [0; 4096]).unwrap(), 0, "request leaked");
                assert!(matches!(
                    result,
                    std::task::Poll::Ready(Err(CancelError::Runtime(
                        uring_runtime::Error::Cancelled
                    )))
                ));
            } else {
                assert!(result.is_pending());
                let mut head = Vec::new();
                while !head.ends_with(b"\r\n\r\n") {
                    assert_ne!(tls.read_tls(&mut socket).unwrap(), 0);
                    tls.process_new_packets().unwrap();
                    let mut bytes = [0; 4096];
                    loop {
                        match tls.reader().read(&mut bytes) {
                            Ok(0) => panic!("unexpected TLS EOF"),
                            Ok(n) => head.extend_from_slice(&bytes[..n]),
                            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => break,
                            Err(error) => panic!("read request: {error}"),
                        }
                    }
                }
                let head = String::from_utf8(head).unwrap();
                assert!(head.starts_with("POST /v1/bootstrap HTTP/1.1\r\n"));
                assert!(head.contains("\r\nAuthorization: Bearer test-token\r\n"));
                tls.writer()
                    .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                    .unwrap();
                while tls.wants_write() {
                    tls.write_tls(&mut socket).unwrap();
                }
                assert_eq!(futures::executor::block_on(request).unwrap().status, 204);
            }
        }

        #[test]
        fn cancellation_during_tls_yield_sends_no_request_bytes() {
            request_at_yield(true);
        }

        #[test]
        fn active_scope_after_tls_yield_sends_request() {
            request_at_yield(false);
        }

        #[test]
        fn checkout_probes_idle_tls_before_reuse() {
            for event in [
                "healthy",
                "key-update",
                "close-notify",
                "eof",
                "plaintext",
                "invalid-tls",
            ] {
                let (transport, mut connection, mut socket, mut tls, scope, _directory) =
                    connected_pair();
                let (ca, key) = testing::ca();
                let identity = TestIdentity::new(&ca, &key);
                let epoch = [1; 32];
                connection.authentication = transport
                    .authentication(
                        Some(identity.borrowed()),
                        epoch,
                        Rc::downgrade(&transport.owner.borrow()),
                    )
                    .unwrap();
                let fd = connection.socket.fd.clone();
                *transport.idle.borrow_mut() = Some(connection);
                match event {
                    "healthy" | "eof" => (),
                    "key-update" => tls.refresh_traffic_keys().unwrap(),
                    "close-notify" => tls.send_close_notify(),
                    "plaintext" => tls.writer().write_all(b"unsolicited response").unwrap(),
                    "invalid-tls" => socket.write_all(b"not a TLS record").unwrap(),
                    _ => unreachable!(),
                }
                while tls.wants_write() {
                    tls.write_tls(&mut socket).unwrap();
                }
                if event == "eof" {
                    socket.shutdown(std::net::Shutdown::Both).unwrap();
                }
                if event != "healthy" {
                    futures::executor::block_on(transport.io().unwrap().ready(
                        fd.clone(),
                        true,
                        false,
                        None,
                        &scope,
                    ))
                    .unwrap();
                }
                let checked = transport.checkout(Some(identity.borrowed()), epoch, &scope);
                assert_eq!(
                    checked.is_some(),
                    matches!(event, "healthy" | "key-update"),
                    "{event}"
                );
                if let Some(connection) = checked {
                    assert!(Rc::ptr_eq(&connection.socket.fd, &fd));
                }
                assert!(transport.idle.borrow().is_none());
            }
        }

        #[test]
        fn bootstrap_and_canceled_checkout_preserve_healthy_idle_connection() {
            let (transport, mut connection, _socket, _tls, scope, _directory) = connected_pair();
            let (ca, key) = testing::ca();
            let identity = TestIdentity::new(&ca, &key);
            let epoch = [1; 32];
            connection.authentication = transport
                .authentication(
                    Some(identity.borrowed()),
                    epoch,
                    Rc::downgrade(&transport.owner.borrow()),
                )
                .unwrap();
            let fd = connection.socket.fd.clone();
            *transport.idle.borrow_mut() = Some(connection);
            assert!(transport.checkout(None, [0; 32], &scope).is_none());
            assert!(transport.idle.borrow().is_some());
            scope.cancelled.set(true);
            assert!(
                transport
                    .checkout(Some(identity.borrowed()), epoch, &scope)
                    .is_none()
            );
            assert!(transport.idle.borrow().is_some());
            scope.cancelled.set(false);
            let connection = transport
                .checkout(Some(identity.borrowed()), epoch, &scope)
                .unwrap();
            assert!(Rc::ptr_eq(&connection.socket.fd, &fd));
        }

        fn authenticated(connection: &mut Connection<FixtureIo>) -> &mut Authenticated<FixtureIo> {
            let Authentication::Authenticated(auth) = &mut connection.authentication else {
                panic!("expected authenticated connection");
            };
            auth
        }

        #[test]
        fn weekly_projected_trust_reload_invalidates_idle_transport() {
            use testing::{RotationServer, WeeklyCertificates, rotation_request};
            let certs = WeeklyCertificates::new();
            assert_eq!(certs.crosses.len(), 3);
            assert_eq!(certs.leaf_only_servers.len(), 4);
            let d = testing::Directory::new();
            let trust = d.0.join("ca.crt");
            std::os::unix::fs::symlink("..data/ca.crt", &trust).unwrap();
            let project = |revision: usize, bundle: String| {
                let directory = format!("revision-{revision}");
                std::fs::create_dir(d.0.join(&directory)).unwrap();
                std::fs::write(d.0.join(&directory).join("ca.crt"), bundle).unwrap();
                std::os::unix::fs::symlink(directory, d.0.join("..data-next")).unwrap();
                std::fs::rename(d.0.join("..data-next"), d.0.join("..data")).unwrap();
            };
            let (peer_ca, peer_key) = testing::ca();
            let identity = TestIdentity::new(&peer_ca, &peer_key);
            let server = RotationServer::new(certs.servers[1].clone());
            let transport = server.transport(trust);
            project(0, certs.roots[0].pem());
            let old = rotation_request(&transport, Some(&identity)).unwrap();
            assert!(transport.idle.borrow().is_some());
            assert_eq!(rotation_request(&transport, Some(&identity)).unwrap(), old);
            // Projected overlap accepts the live old server but discards cached TLS.
            project(
                1,
                certs.roots[2].pem() + &certs.roots[1].pem() + &certs.roots[0].pem(),
            );
            let overlap = rotation_request(&transport, Some(&identity)).unwrap();
            assert_ne!(overlap, old);
            assert_eq!(
                rotation_request(&transport, Some(&identity)).unwrap(),
                overlap
            );
            // Removed trust cannot be bypassed by recycling an idle connection.
            project(2, certs.roots[2].pem());
            assert!(matches!(
                rotation_request(&transport, Some(&identity)),
                Err(testing::Error::Transport(Error::Unauthorized))
            ));
            assert!(transport.idle.borrow().is_none());
            server.rotate(certs.servers[2].clone());
            let current = rotation_request(&transport, Some(&identity)).unwrap();
            assert_ne!(current, overlap);
            assert_eq!(
                rotation_request(&transport, Some(&identity)).unwrap(),
                current
            );
            project(3, peer_ca.pem());
            assert!(matches!(
                rotation_request(&transport, Some(&identity)),
                Err(testing::Error::Transport(Error::Unauthorized))
            ));
            assert!(transport.idle.borrow().is_none());
            // Recover through both cross certificates without reattaching transport.
            project(4, certs.roots[0].pem());
            assert!(rotation_request(&transport, Some(&identity)).is_ok());
            transport.close_idle();
        }

        #[test]
        fn requests_enforce_connection_credentials_and_routes() {
            use testing::{RotationServer, WeeklyCertificates};
            let certs = WeeklyCertificates::new();
            let d = testing::Directory::new();
            let trust = d.0.join("trust.pem");
            std::fs::write(&trust, certs.roots[0].pem()).unwrap();
            let server = RotationServer::new(certs.servers[0].clone());
            let transport = server.transport(trust);
            let (ca, key) = testing::ca();
            let identity = TestIdentity::new(&ca, &key);
            for mutual in [false, true] {
                for (method, path, bearer, bootstrap_ok, authenticated_ok) in [
                    (Method::Post, "/v1/bootstrap", Some("token"), true, false),
                    (Method::Post, "/v1/bootstrap", None, false, false),
                    (Method::Post, "/v1/bootstrap", Some(""), false, false),
                    (Method::Get, "/v1/bootstrap", Some("token"), false, false),
                    (
                        Method::Post,
                        "/v1/bootstrap?x=1",
                        Some("token"),
                        false,
                        false,
                    ),
                    (Method::Post, "/v1/bootstrap/", Some("token"), false, false),
                    (Method::Get, "/v1/snapshot", None, false, true),
                    (Method::Get, "/v1/snapshot?after=1", None, false, true),
                    (Method::Get, "/v1/keyring", None, false, true),
                    (Method::Get, "/v1/keyring?after=1", None, false, true),
                    (Method::Get, "/v1/snapshot", Some("token"), false, false),
                    (Method::Get, "/v1/keyring", Some("token"), false, false),
                    (Method::Get, "/v1/keyring", Some(""), false, false),
                    (Method::Post, "/v1/snapshot", None, false, false),
                    (Method::Get, "/v1/snapshot/", None, false, false),
                    (Method::Get, "/v1/%73napshot", None, false, false),
                    (Method::Get, "/unknown", None, false, false),
                ] {
                    let scope = testing::scope();
                    let connection = futures::executor::block_on(
                        transport.connect(mutual.then(|| identity.borrowed()), &scope),
                    )
                    .unwrap();
                    let result = futures::executor::block_on(
                        connection.request(testing::request(method, path, bearer, 1024), &scope),
                    );
                    if if mutual {
                        authenticated_ok
                    } else {
                        bootstrap_ok
                    } {
                        assert_eq!(result.unwrap().status, 200, "{mutual}: {path}");
                    } else {
                        assert!(
                            matches!(
                                result,
                                Err(testing::Error::Transport(Error::InvalidRequest))
                            ),
                            "{mutual}: {path}"
                        );
                    }
                    transport.close_idle();
                }
            }
        }

        #[test]
        fn bootstrap_never_carries_authenticated_pool_state() {
            use testing::{RotationServer, WeeklyCertificates, rotation_request};
            let certs = WeeklyCertificates::new();
            let d = testing::Directory::new();
            let trust = d.0.join("trust.pem");
            std::fs::write(&trust, certs.roots[0].pem()).unwrap();
            let server = RotationServer::new(certs.servers[0].clone());
            let transport = server.transport(trust);
            let scope = testing::scope();
            let connection = futures::executor::block_on(transport.bootstrap(&scope)).unwrap();
            assert!(matches!(
                connection.authentication,
                Authentication::Bootstrap
            ));
            let first = futures::executor::block_on(connection.request(
                testing::request(Method::Post, "/v1/bootstrap", Some("test-token"), 1024),
                &scope,
            ))
            .unwrap();
            assert!(transport.idle.borrow().is_none());
            let second = rotation_request(&transport, None).unwrap();
            assert_ne!(first.body, second);
            assert!(transport.idle.borrow().is_none());
        }

        #[test]
        fn replacing_owner_invalidates_checked_out_connection_recycling() {
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = TestIdentity::new(&ca, &key);
            let response = || ("/v1/snapshot".to_owned(), 200, b"{}".to_vec());
            let (endpoint, server) = scripted_server(
                &d,
                &ca,
                &key,
                vec![vec![response(), response()], vec![response(), response()]],
            );
            let transport = Transport::new(endpoint);
            let owner_a = Rc::new(FixtureIo);
            let owner_b = Rc::new(FixtureIo);
            transport.attach_io(owner_a.clone());
            let scope = testing::scope();
            let complete = |connection: Connection<FixtureIo>| {
                futures::executor::block_on(connection.request(
                    testing::request(Method::Get, "/v1/snapshot", None, 1024),
                    &scope,
                ))
                .unwrap()
            };
            let connect =
                || futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
            complete(connect());
            let old = connect();
            assert!(Rc::ptr_eq(&old.io, &owner_a));
            transport.attach_io(owner_b.clone());
            assert_eq!(complete(old).status, 200);
            assert!(transport.idle.borrow().is_none());
            for _ in 0..2 {
                let current = connect();
                assert!(Rc::ptr_eq(&current.io, &owner_b));
                assert_eq!(complete(current).status, 200);
                assert!(transport.idle.borrow().is_some());
            }
            transport.close_idle();
            server.join().unwrap();
        }

        #[test]
        fn pooled_connections_retire_between_requests_and_discard_failures() {
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = TestIdentity::new(&ca, &key);
            let response = |status| {
                (
                    "/v1/snapshot".to_owned(),
                    status,
                    if status == 204 {
                        vec![]
                    } else {
                        b"{}".to_vec()
                    },
                )
            };
            let (endpoint, server) = scripted_server(
                &d,
                &ca,
                &key,
                vec![
                    vec![response(200), response(204)], // Reuse before age boundary.
                    vec![response(204)],                // In-flight response survives retirement.
                    vec![response(200)],                // Idle timeout still applies.
                    vec![response(200)],                // Identity/trust epoch still applies.
                    vec![response(200), response(429)],
                    vec![response(200), response(503)],
                    vec![response(200)], // Invalid local request discards the pool lease.
                    vec![response(200), response(0)], // Backend disconnect during request.
                    vec![response(204)],
                ],
            );
            let transport = Transport::new(endpoint);
            transport.attach_io(Rc::new(FixtureIo));
            let scope = testing::scope();
            let get = || {
                futures::executor::block_on(async {
                    transport
                        .authenticated(&identity, &scope)
                        .await
                        .unwrap()
                        .request(
                            testing::request(Method::Get, "/v1/snapshot", None, 65536),
                            &scope,
                        )
                        .await
                        .unwrap()
                })
            };
            assert_eq!(get().status, 200);
            let retire_at = authenticated(transport.idle.borrow_mut().as_mut().unwrap()).retire_at;
            assert!(retire_at > Instant::now() + Duration::from_secs(239));
            assert!(retire_at <= Instant::now() + Duration::from_secs(300));
            assert_eq!(get().status, 204);
            assert_eq!(
                authenticated(transport.idle.borrow_mut().as_mut().unwrap()).retire_at,
                retire_at
            );
            authenticated(transport.idle.borrow_mut().as_mut().unwrap()).retire_at = Instant::now();
            // Checkout retires the first connection. Active I/O ignores its age.
            let mut connection =
                futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
            authenticated(&mut connection).retire_at = Instant::now();
            assert_eq!(connection.check(&scope), Ok(()));
            let expiry = authenticated(&mut connection).expires;
            authenticated(&mut connection).expires = SystemTime::now() - Duration::from_secs(1);
            assert_eq!(
                connection.check(&scope),
                Err(testing::Error::Transport(Error::Unauthorized))
            );
            authenticated(&mut connection).expires = expiry;
            let result = futures::executor::block_on(connection.request(
                testing::request(Method::Get, "/v1/snapshot", None, 65536),
                &scope,
            ))
            .unwrap();
            assert_eq!(result.status, 204);
            assert!(transport.idle.borrow().is_none());
            assert_eq!(get().status, 200);
            authenticated(transport.idle.borrow_mut().as_mut().unwrap()).idle_since =
                Instant::now() - Duration::from_secs(20);
            assert_eq!(get().status, 200);
            authenticated(transport.idle.borrow_mut().as_mut().unwrap()).epoch = [0; 32];
            for status in [429, 503] {
                assert_eq!(get().status, 200);
                assert_eq!(get().status, status);
                assert!(transport.idle.borrow().is_none());
            }
            assert_eq!(get().status, 200);
            let connection =
                futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
            assert!(matches!(
                futures::executor::block_on(connection.request(
                    testing::request(Method::Get, "invalid", None, 65536),
                    &scope
                )),
                Err(testing::Error::Transport(Error::InvalidRequest))
            ));
            assert!(transport.idle.borrow().is_none());
            assert_eq!(get().status, 200);
            let connection =
                futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
            assert!(matches!(
                futures::executor::block_on(connection.request(
                    testing::request(Method::Get, "/v1/snapshot", None, 65536),
                    &scope
                )),
                Err(testing::Error::Transport(Error::Io))
            ));
            assert!(transport.idle.borrow().is_none());
            assert_eq!(get().status, 204);
            transport.close_idle();
            server.join().unwrap();
        }

        #[test]
        fn pooled_connections_validate_client_key_before_reuse() {
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = TestIdentity::new(&ca, &key);
            let unrelated = TestIdentity::new(&ca, &key);
            let response = || ("/v1/snapshot".to_owned(), 200, b"{}".to_vec());
            let (endpoint, server) =
                scripted_server(&d, &ca, &key, vec![vec![response(), response()]]);
            let transport = Transport::new(endpoint);
            transport.attach_io(Rc::new(FixtureIo));
            let scope = testing::scope();
            let invalid_keys = [unrelated.borrowed().private_key, b"corrupt PKCS#8"];
            let connect_error = |private_key| {
                futures::executor::block_on(transport.connect(
                    Some(Identity {
                        private_key,
                        ..identity.borrowed()
                    }),
                    &scope,
                ))
                .err()
            };
            let cold_errors = invalid_keys.map(connect_error);
            assert_eq!(
                cold_errors,
                [Some(testing::Error::Transport(Error::Unauthorized)); 2]
            );
            let get = || {
                futures::executor::block_on(async {
                    transport
                        .authenticated(&identity, &scope)
                        .await
                        .unwrap()
                        .request(
                            testing::request(Method::Get, "/v1/snapshot", None, 1024),
                            &scope,
                        )
                        .await
                        .unwrap()
                })
            };
            assert_eq!(get().status, 200);
            assert!(transport.idle.borrow().is_some());
            for (private_key, cold_error) in invalid_keys.into_iter().zip(cold_errors) {
                assert_eq!(connect_error(private_key), cold_error);
                assert!(transport.idle.borrow().is_some());
            }
            assert_eq!(get().status, 200);
            assert!(transport.idle.borrow().is_some());
            transport.close_idle();
            server.join().unwrap();
        }

        #[test]
        fn pooled_connections_require_matching_identity_expiration() {
            let d = testing::Directory::new();
            let (ca, key) = testing::ca();
            let identity = TestIdentity::new(&ca, &key);
            let (endpoint, server) = scripted_server(
                &d,
                &ca,
                &key,
                vec![vec![("/v1/snapshot".to_owned(), 200, b"{}".to_vec())]],
            );
            let transport = Transport::new(endpoint);
            transport.attach_io(Rc::new(FixtureIo));
            let scope = testing::scope();
            let connection =
                futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
            futures::executor::block_on(connection.request(
                testing::request(Method::Get, "/v1/snapshot", None, 1024),
                &scope,
            ))
            .unwrap();
            let (epoch, expires) = {
                let mut idle = transport.idle.borrow_mut();
                let auth = authenticated(idle.as_mut().unwrap());
                (auth.epoch, auth.expires)
            };
            let mut shorter_identity = identity.borrowed();
            shorter_identity.expires = expires - Duration::from_secs(1);
            assert!(
                transport
                    .checkout(Some(shorter_identity), epoch, &scope)
                    .is_none()
            );
            assert!(transport.idle.borrow().is_none());
            server.join().unwrap();
        }

        fn scripted_server(
            d: &testing::Directory,
            ca: &rcgen::Certificate,
            ca_key: &rcgen::KeyPair,
            groups: Vec<Vec<(String, u16, Vec<u8>)>>,
        ) -> (Config, std::thread::JoinHandle<()>) {
            let mut params = rcgen::CertificateParams::new(vec!["127.0.0.1".into()]).unwrap();
            params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
            let cert = params.signed_by(&key, ca, ca_key).unwrap();
            let trust = d.0.join("trust");
            std::fs::write(&trust, ca.pem()).unwrap();
            let mut roots = rustls::RootCertStore::empty();
            roots.add(ca.der().clone()).unwrap();
            let provider = Arc::new(rustls::crypto::ring::default_provider());
            let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
                Arc::new(roots),
                provider.clone(),
            )
            .build()
            .unwrap();
            let config = Arc::new(
                rustls::ServerConfig::builder_with_provider(provider)
                    .with_safe_default_protocol_versions()
                    .unwrap()
                    .with_client_cert_verifier(verifier)
                    .with_single_cert(
                        vec![cert.der().clone()],
                        rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
                    )
                    .unwrap(),
            );
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            let endpoint =
                testing::config(format!("https://{}", listener.local_addr().unwrap()), trust);
            let server = std::thread::spawn(move || {
                for group in groups {
                    let deadline = Instant::now() + Duration::from_secs(10);
                    let socket = loop {
                        match listener.accept() {
                            Ok((socket, _)) => break socket,
                            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                                assert!(Instant::now() < deadline, "missing backend connection");
                                std::thread::sleep(Duration::from_millis(1));
                            }
                            Err(e) => panic!("accept: {e}"),
                        }
                    };
                    socket
                        .set_read_timeout(Some(Duration::from_secs(10)))
                        .unwrap();
                    socket
                        .set_write_timeout(Some(Duration::from_secs(10)))
                        .unwrap();
                    let mut stream = rustls::StreamOwned::new(
                        rustls::ServerConnection::new(config.clone()).unwrap(),
                        socket,
                    );
                    let mut disconnected = false;
                    for (path, status, body) in group {
                        let mut request = Vec::new();
                        while !request.ends_with(b"\r\n\r\n") {
                            assert!(request.len() < 16384);
                            let mut byte = [0];
                            stream.read_exact(&mut byte).unwrap();
                            request.push(byte[0]);
                        }
                        assert!(
                            String::from_utf8(request)
                                .unwrap()
                                .starts_with(&format!("GET {path} HTTP/1.1\r\n"))
                        );
                        if status == 0 {
                            disconnected = true;
                            break;
                        }
                        let head = format!(
                            "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nRetry-After: 1\r\n\r\n",
                            body.len()
                        );
                        stream.write_all(head.as_bytes()).unwrap();
                        stream.write_all(&body).unwrap();
                        stream.flush().unwrap();
                    }
                    if disconnected {
                        continue;
                    }
                    match stream.read(&mut [0]) {
                        Ok(0) => (),
                        Err(e)
                            if matches!(
                                e.kind(),
                                std::io::ErrorKind::UnexpectedEof
                                    | std::io::ErrorKind::ConnectionReset
                            ) => {}
                        result => panic!("connection not discarded: {result:?}"),
                    }
                }
            });
            (endpoint, server)
        }

        #[test]
        fn request_heads_validate_and_bound_credentials_before_copying() {
            let base = "a".repeat(64);
            let head = request_head(
                "controller:443",
                Method::Post,
                "/enroll",
                Some("secret"),
                42,
                Some(("X-Delta-Base", &base)),
            )
            .unwrap();
            assert!(head.starts_with("POST /enroll HTTP/1.1\r\nHost: controller:443\r\n"));
            assert!(head.contains("Content-Length: 42\r\n"));
            assert!(head.contains("Authorization: Bearer secret\r\n"));
            assert!(head.ends_with(&format!("X-Delta-Base: {base}\r\n\r\n")));
            for (method, path, token, base) in [
                (Method::Get, "enroll", None, None),
                (Method::Get, "/bad\r\n", None, None),
                (Method::Get, "/enroll", Some("bad\r\n"), None),
                (
                    Method::Get,
                    "/enroll",
                    None,
                    Some(("X-Delta-Base", "bad\r\n")),
                ),
            ] {
                assert_eq!(
                    request_head("controller", method, path, token, 0, base),
                    Err(Error::InvalidRequest)
                );
            }
            assert_eq!(
                request_head("controller\r\n", Method::Get, "/", None, 0, None),
                Err(Error::InvalidRequest)
            );
            assert_eq!(
                request_head(
                    "controller",
                    Method::Get,
                    "/",
                    Some(&"a".repeat(16384)),
                    0,
                    None
                ),
                Err(Error::Overloaded)
            );
            assert_eq!(
                request_head(
                    "controller",
                    Method::Get,
                    &format!("/{}", "a".repeat(16384)),
                    None,
                    0,
                    None
                ),
                Err(Error::Overloaded)
            );
        }

        #[test]
        fn authenticated_age_jitter_is_bounded() {
            assert_eq!(authenticated_age(0), Duration::from_secs(240));
            assert_eq!(authenticated_age(60_000), Duration::from_secs(300));
            for random in [1, 30_000, 60_001, u64::MAX] {
                let age = authenticated_age(random);
                assert!((Duration::from_secs(240)..=Duration::from_secs(300)).contains(&age));
            }
            assert_ne!(authenticated_age(1), authenticated_age(30_000));
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
        ] { assert!(parse_head(bytes, 10, 65536).is_err()); }
        }

        #[test]
        fn maximum_content_length_rejects_frame_overflow_without_body_allocation() {
            for status in [200, 429] {
                // Only a small header is allocated, never the attacker-specified body.
                let bytes = format!(
                    "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",
                    usize::MAX
                );
                let head = parse_head(bytes.as_bytes(), usize::MAX, usize::MAX)
                    .unwrap()
                    .unwrap();
                let Framing::Length(length) = head.framing else {
                    panic!("expected content length framing");
                };
                assert_eq!(length, usize::MAX);
                assert_eq!(
                    length_frame_end(head.header_len, length),
                    Err(Error::Overloaded)
                );
                assert_eq!(
                    length_frame_end(head.header_len, usize::MAX - head.header_len),
                    Ok(usize::MAX)
                );
                assert_eq!(length_frame_end(head.header_len, 0), Ok(head.header_len));
                assert_eq!(
                    length_frame_end(head.header_len, 2),
                    Ok(head.header_len + 2)
                );
            }
        }

        #[test]
        fn response_head_collects_connection_close_once() {
            for (headers, close) in [
                ("", false),
                ("Connection: keep-alive\r\n", false),
                ("Connection: xclose, closex\r\n", false),
                ("cOnNeCtIoN: keep-alive,\tClOsE \r\n", true),
                ("Connection: close\r\nConnection: keep-alive\r\n", true),
                ("Connection: keep-alive\r\nConnection: close\r\n", true),
                ("Connection: \u{a0}close\u{a0}\r\n", true),
            ] {
                let bytes = format!("HTTP/1.1 204 No Content\r\n{headers}Retry-After: 2\r\n\r\n");
                assert_eq!(
                    parse_head(bytes.as_bytes(), 10, 20).unwrap(),
                    Some(ResponseHead {
                        status: 204,
                        header_len: bytes.len(),
                        framing: Framing::Length(0),
                        retry_after: Some(Duration::from_secs(2)),
                        close,
                    })
                );
            }
            assert_eq!(parse_head(b"HTTP/1.1 204 No Content\r\n", 10, 20), Ok(None));
        }

        #[test]
        fn response_head_preserves_utf8_acceptance_and_error_precedence() {
            for name in ["X-Ignored", "Connection"] {
                let mut bytes = format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 12\r\n{name}: ").into_bytes();
                bytes.extend_from_slice(b"\xff\r\n\r\n");
                assert_eq!(parse_head(&bytes, 10, 20), Err(Error::Overloaded));
                assert_eq!(parse_head(&bytes, 12, 20), Err(Error::InvalidRequest));
            }
            let bytes = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n\xff\xff";
            assert!(parse_head(bytes, 2, 20).is_ok());
            assert_eq!(parse_head(b"HTTP/1.1 429 Busy\r\nContent-Type: application/json\r\nContent-Length: 12\r\n\r\n", 10, 20).unwrap().unwrap().framing, Framing::Length(12));
        }

        #[test]
        fn chunked_framing_carries_the_validated_body_limit() {
            for (status, limit, max_error_body, bound) in [
                (200, 10, 20, 10),
                (429, 10, 20, 20),
                (503, 10, 0, 0),
                (200, 0, 20, 0),
            ] {
                let bytes = format!(
                    "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
                );
                assert_eq!(
                    parse_head(bytes.as_bytes(), limit, max_error_body)
                        .unwrap()
                        .unwrap()
                        .framing,
                    Framing::Chunked { bound }
                );
            }
            assert_eq!(
                parse_head(
                    b"HTTP/1.1 204 Empty\r\nTransfer-Encoding: chunked\r\n\r\n",
                    10,
                    20
                ),
                Err(Error::InvalidRequest)
            );
        }

        #[test]
        fn trust_epochs_preserve_certificate_boundaries() {
            let chain = vec![b"ab".to_vec(), b"c".to_vec()];
            let regrouped = vec![b"a".to_vec(), b"bc".to_vec()];
            let identity = Identity {
                certificate_chain: &chain,
                private_key: b"key",
                expires: std::time::UNIX_EPOCH,
            };
            let epoch = trust_epoch(b"trust", Some(identity));
            assert_eq!(epoch, trust_epoch(b"trust", Some(identity)));
            assert_ne!(epoch, trust_epoch(b"changed", Some(identity)));
            assert_ne!(epoch, trust_epoch(b"trust", None));
            assert_ne!(
                epoch,
                trust_epoch(
                    b"trust",
                    Some(Identity {
                        certificate_chain: &regrouped,
                        ..identity
                    })
                )
            );
            assert_eq!(
                epoch,
                trust_epoch(
                    b"trust",
                    Some(Identity {
                        private_key: b"different key",
                        ..identity
                    })
                )
            );
        }

        #[test]
        fn generic_header_accepts_tokens_and_printable_values() {
            let head = request_head(
                "example.test",
                Method::Post,
                "/json",
                Some("secret"),
                4,
                Some(("X-Client-Version", "v1 with spaces")),
            )
            .unwrap();
            assert!(head.starts_with("POST /json HTTP/1.1\r\n"));
            assert!(head.contains("Authorization: Bearer secret\r\n"));
            assert!(head.contains("Content-Length: 4\r\n"));
            assert!(head.ends_with("X-Client-Version: v1 with spaces\r\n\r\n"));
            assert!(valid_header("!#$%&'*+-.^_`|~09AZaz", ""));
            assert!(valid_header("X-Test", " ~"));
        }

        #[test]
        fn generic_header_rejects_injection_and_reserved_overrides() {
            for (name, value) in [
                ("", "ok"),
                ("X Bad", "ok"),
                ("X:Bad", "ok"),
                ("X-Test", "bad\r\nInjected: yes"),
                ("X-Test", "\t"),
                ("X-Test", "\x7f"),
                ("X-Test", "non-ASCII: é"),
                ("é", "ok"),
            ] {
                assert_eq!(
                    request_head(
                        "example.test",
                        Method::Get,
                        "/",
                        None,
                        0,
                        Some((name, value))
                    ),
                    Err(Error::InvalidRequest)
                );
            }
            for name in [
                "Host",
                "AUTHORIZATION",
                "Proxy-Authorization",
                "Content-Length",
                "Transfer-Encoding",
                "Content-Type",
                "Content-Encoding",
                "Accept",
                "Accept-Encoding",
                "Connection",
                "TE",
                "Trailer",
                "Upgrade",
                "Expect",
            ] {
                assert_eq!(
                    request_head(
                        "example.test",
                        Method::Get,
                        "/",
                        None,
                        0,
                        Some((name, "override"))
                    ),
                    Err(Error::InvalidRequest)
                );
            }
        }

        #[test]
        fn generic_header_bounds_name_and_value_before_copying() {
            let large = "a".repeat(16384);
            for header in [Some((large.as_str(), "")), Some(("X-Test", large.as_str()))] {
                assert_eq!(
                    request_head("example.test", Method::Get, "/", None, 0, header),
                    Err(Error::Overloaded)
                );
            }
        }
    }
}

#[cfg(all(test, feature = "rest"))]
#[path = "../tests/support/mod.rs"]
mod testing;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_hex() {
        let bytes = std::array::from_fn::<_, 256, _>(|i| i as u8);
        let hex = encode_hex(&bytes);
        assert_eq!(hex.len(), 512);
        assert!(
            hex.bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        );
        assert_eq!(decode_hex::<256>(hex.as_bytes()), Ok(bytes));
        assert_eq!(decode_hex::<0>(b""), Ok([]));
        assert_eq!(encode_hex(&[]), "");
        for value in [
            b"0".as_slice(),
            b"000",
            b"AA",
            b"aF",
            b"gg",
            b" 0",
            b"0\n",
            &[0xff, 0xff],
        ] {
            assert_eq!(decode_hex::<1>(value), Err(Error::InvalidHex));
        }
    }

    #[test]
    fn endian_vectors_and_every_truncation() {
        for (endian, expected) in [
            (
                Endian::Little,
                vec![4, 3, 2, 1, 8, 7, 6, 5, 4, 3, 2, 1, 2, 0, 0, 0, b'a', b'b'],
            ),
            (
                Endian::Big,
                vec![1, 2, 3, 4, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 2, b'a', b'b'],
            ),
        ] {
            let mut bytes = Vec::new();
            let mut writer = Writer::new(&mut bytes, 18, endian);
            writer.u32(0x01020304).unwrap();
            writer.u64(0x0102030405060708).unwrap();
            writer.length_prefixed(b"ab", 2).unwrap();
            assert_eq!(bytes, expected);
            let parse = |bytes: &[u8]| -> Result<()> {
                let mut reader = Reader::new(bytes, endian);
                assert_eq!(reader.u32()?, 0x01020304);
                assert_eq!(reader.u64()?, 0x0102030405060708);
                assert_eq!(reader.length_prefixed(2)?, b"ab");
                assert!(reader.remaining().is_empty());
                Ok(())
            };
            parse(&bytes).unwrap();
            for length in 0..bytes.len() {
                assert!(parse(&bytes[..length]).is_err(), "{length}");
            }
        }
    }

    #[test]
    fn limits_empty_fields_and_overflow() {
        let mut bytes = Vec::new();
        let mut writer = Writer::new(&mut bytes, 4, Endian::Little);
        assert_eq!(writer.length_prefixed(b"x", 0), Err(Error::Limit));
        assert_eq!(writer.length_prefixed(b"x", 1), Err(Error::Limit));
        writer.length_prefixed(b"", 0).unwrap();
        assert_eq!(writer.bytes(b"x"), Err(Error::Limit));
        assert_eq!(bytes, [0; 4]);
        assert_eq!(
            Reader::new(&bytes, Endian::Little).length_prefixed(0),
            Ok(b"".as_slice())
        );
        let mut reader = Reader::new(&[1, 2], Endian::Big);
        assert_eq!(reader.take(usize::MAX), Err(Error::Truncated));
        assert_eq!(reader.remaining(), &[1, 2]);
        assert_eq!(
            Reader::new(&[2, 0, 0, 0, 0], Endian::Little).count(2, 1),
            Err(Error::Limit)
        );
        assert_eq!(
            Reader::new(&[2, 0, 0, 0], Endian::Little).count(1, 0),
            Err(Error::Limit)
        );
        assert_eq!(
            Reader::new(&[2, 0, 0, 0], Endian::Little).count(2, 0),
            Ok(2)
        );
        let mut oversized = vec![0; 5];
        assert_eq!(
            Writer::new(&mut oversized, 4, Endian::Big).bytes(&[]),
            Err(Error::Limit)
        );
        if usize::BITS > 32 {
            assert_eq!(
                Writer::new(&mut bytes, usize::MAX, Endian::Little).count(usize::MAX),
                Err(Error::Limit)
            );
        }
    }

    #[test]
    fn length_prefixed_append_is_atomic_at_every_bound() {
        for endian in [Endian::Little, Endian::Big] {
            for length in 0..=8 {
                let value = vec![0x7f; length];
                for existing in 0..=4 {
                    for capacity in 0..=20 {
                        for maximum in 0..=8 {
                            let mut bytes = vec![0xa5; existing];
                            let result = Writer::new(&mut bytes, capacity, endian)
                                .length_prefixed(&value, maximum);
                            if length <= maximum && existing + 4 + length <= capacity {
                                assert_eq!(result, Ok(()));
                                assert_eq!(&bytes[..existing], vec![0xa5; existing]);
                                let mut reader = Reader::new(&bytes[existing..], endian);
                                assert_eq!(reader.length_prefixed(maximum), Ok(value.as_slice()));
                                assert!(reader.remaining().is_empty());
                            } else {
                                assert_eq!(result, Err(Error::Limit));
                                assert_eq!(bytes, vec![0xa5; existing]);
                            }
                        }
                    }
                }
            }
        }
    }

    #[test]
    fn count_failures_preserve_cursor_contract() {
        for endian in [Endian::Little, Endian::Big] {
            for count in 0..=8 {
                let mut bytes = Vec::new();
                Writer::new(&mut bytes, 4, endian).count(count).unwrap();
                for prefix in 0..4 {
                    let mut reader = Reader::new(&bytes[..prefix], endian);
                    assert_eq!(reader.count(8, 0), Err(Error::Truncated));
                    assert_eq!(reader.remaining(), &bytes[..prefix]);
                }
                bytes.extend_from_slice(&[0xa5; 8]);
                for available in 0..=8 {
                    for maximum in 0..=8 {
                        for minimum in [0, 1, 2, 3, usize::MAX] {
                            let input = &bytes[..4 + available];
                            let mut reader = Reader::new(input, endian);
                            let expected = if count <= maximum
                                && (minimum == 0 || count <= available / minimum)
                            {
                                Ok(count)
                            } else {
                                Err(Error::Limit)
                            };
                            assert_eq!(reader.count(maximum, minimum), expected);
                            assert_eq!(reader.remaining(), &input[4..]);
                        }
                        let input = &bytes[..4 + available];
                        let mut reader = Reader::new(input, endian);
                        if count <= maximum && count <= available {
                            assert_eq!(reader.length_prefixed(maximum), Ok(&input[4..4 + count]));
                            assert_eq!(reader.remaining(), &input[4 + count..]);
                        } else {
                            assert_eq!(reader.length_prefixed(maximum), Err(Error::Limit));
                            assert_eq!(reader.remaining(), &input[4..]);
                        }
                    }
                }
            }
        }
    }
}
