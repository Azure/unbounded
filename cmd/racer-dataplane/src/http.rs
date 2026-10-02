//! Racer policy adapters for the standalone fixed-length HTTP implementation.
pub use http1::{Header, MessageHead, StartLine};
pub const MAX_HEAD_BYTES: usize = 32 * 1024;
pub struct RacerOpaque;
impl http1::Opaque for RacerOpaque {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}
pub type Codec = http1::Codec<RacerOpaque>;
pub(crate) fn is_token(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b)
}
pub(crate) fn trim_ows(mut value: &[u8]) -> &[u8] {
    while value.first().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[1..];
    }
    while value.last().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[..value.len() - 1];
    }
    value
}
pub mod connection {
    #[cfg(test)]
    use super::StartLine;
    use super::{Codec, MessageHead};
    use crate::{
        error::{Error, Operation, Result},
        model::ResourceClass,
        runtime::{
            admission::{AdmissionExt, AdmissionPolicy, ConnectionReservation},
            deadline::RequestScope,
            reactor::{Descriptor, IoBuffer, Reactor, SocketAddress},
        },
    };
    #[cfg(test)]
    use flow_control::pipe::MAX_PIPE_BYTES;
    pub use http1::connection::BufferRange;
    use std::{cell::RefCell, ops::Deref, path::PathBuf, rc::Rc, task::Poll, time::Duration};
    #[cfg(test)]
    use std::{task::Waker, time::Instant};
    pub type ConnectionLease = http1::connection::ConnectionLease<HttpContext>;
    pub type OwnedBuffer = http1::connection::OwnedBuffer<HttpContext>;
    pub type HeadCompletion<T> = http1::connection::HeadCompletion<HttpContext, T>;

    /// HTTP's application context, separate from the generic quota authority.
    pub struct HttpContext(pub(crate) Rc<flow_control::Quotas<AdmissionPolicy>>);
    impl http1::connection::Context for HttpContext {
        type Error = Error;
        type Scope = RequestScope;
        type Budget = crate::runtime::reactor::AdmissionBudget;
        type Reactor = Reactor;
        type Charge = flow_control::Charge<AdmissionPolicy>;
        type Slot = ConnectionReservation;
        type Opaque = super::RacerOpaque;
        type State = State;
        type Endpoint = Endpoint;
        fn charge(&self, bytes: usize) -> Result<flow_control::Charge<AdmissionPolicy>> {
            self.0
                .reserve(None, ResourceClass::RequestContext, bytes)
                .map_err(Into::into)
        }
        fn outbound_slot(&self) -> Result<ConnectionReservation> {
            self.0.reserve_connection(ResourceClass::OutboundConnection)
        }
        fn stopped(&self) -> bool {
            self.0.is_stopped()
        }
    }
    #[derive(Default)]
    pub struct State {
        pub(crate) peer_admission: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
        pub(crate) peer_response_verified: bool,
        pub(crate) connect_failure: Option<Rc<std::cell::Cell<bool>>>,
        #[cfg(test)]
        pub(crate) relay_fallback: bool,
        #[cfg(test)]
        pub(crate) relay_fallback_at: Option<usize>,
        pub(crate) relay_peer: Option<Box<ConnectionLease>>,
        pub(crate) relay_pipe: Option<flow_control::pipe::PipeLease<AdmissionPolicy>>,
        pub(crate) relay_context: Option<flow_control::Charge<AdmissionPolicy>>,
        pub(crate) relay_reservation: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        pub(crate) session: Option<crate::security::connection::Session>,
        pub(crate) control_reservation: Option<flow_control::Charge<AdmissionPolicy>>,
    }
    impl http1::connection::State<Error> for State {
        fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
            match &mut self.session {
                Some(session) => session.admit(head),
                None => Ok(head),
            }
        }
        fn sign(&mut self, head: MessageHead) -> Result<MessageHead> {
            match &mut self.session {
                Some(session) => session.sign(head),
                None => Ok(head),
            }
        }
        fn finished(&mut self) {
            if self.peer_response_verified {
                if let Some(permit) = &self.peer_admission {
                    permit.observe(crate::peer::adaptive::Outcome::Verified);
                }
                self.peer_response_verified = false;
            }
        }
        fn connect_failed(&self, errno: Option<i32>) {
            if peer_connect_failure(errno) {
                if let Some(failure) = &self.connect_failure {
                    failure.set(true);
                }
                if let Some(peer) = &self.peer_admission {
                    peer.observe(crate::peer::adaptive::Outcome::PeerFailure);
                }
            }
        }
        fn attach(&mut self, checkout: Self) {
            self.relay_reservation = checkout.relay_reservation;
            self.peer_admission = checkout.peer_admission;
            self.connect_failure = checkout.connect_failure;
        }
        fn idle(mut self) -> Self {
            Self {
                session: self.session.take(),
                ..Self::default()
            }
        }
    }
    pub fn from_accepted(
        fd: Descriptor,
        admission: &flow_control::Quotas<AdmissionPolicy>,
    ) -> Result<ConnectionLease> {
        from_reserved(
            fd,
            admission.reserve_connection(ResourceClass::IngressConnection)?,
        )
    }
    pub(crate) fn from_reserved(
        fd: Descriptor,
        reservation: ConnectionReservation,
    ) -> Result<ConnectionLease> {
        ConnectionLease::from_reserved(fd, reservation, State::default())
    }
    pub(crate) fn install_session(
        connection: &mut ConnectionLease,
        session: crate::security::connection::Session,
    ) -> Result<()> {
        if connection.state().session.is_some() || connection.closing() {
            return Err(Error::Unauthorized);
        }
        connection.state_mut().session = Some(session);
        connection.begin_io();
        Ok(())
    }
    pub struct HttpIo(http1::connection::HttpIo<HttpContext>);
    impl Deref for HttpIo {
        type Target = http1::connection::HttpIo<HttpContext>;
        fn deref(&self) -> &Self::Target {
            &self.0
        }
    }
    impl HttpIo {
        pub fn with_admission(
            reactor: Rc<Reactor>,
            codec: Codec,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            body_limit: u64,
        ) -> Self {
            Self(http1::connection::HttpIo::new(
                reactor,
                codec,
                Rc::new(HttpContext(admission)),
                body_limit,
                body_limit,
            ))
        }
        pub fn for_clients(
            reactor: Rc<Reactor>,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        ) -> Self {
            Self(http1::connection::HttpIo::new(
                reactor,
                Codec::new(
                    admission
                        .limits()
                        .header_bytes
                        .get()
                        .min(super::MAX_HEAD_BYTES),
                ),
                Rc::new(HttpContext(admission)),
                crate::model::PAGE_BYTES + 16,
                i64::MAX as u64,
            ))
        }
        pub fn capped(&self, limit: usize) -> Self {
            Self(self.0.capped(limit))
        }
    }
    #[derive(Clone, Debug, Eq, Hash, PartialEq, Ord, PartialOrd)]
    pub enum Endpoint {
        Unix(PathBuf),
        Origin {
            cache: crate::model::CacheId,
            path: PathBuf,
        },
        Peer(String),
    }
    impl http1::connection::Endpoint<Error> for Endpoint {
        fn address(&self) -> Result<SocketAddress> {
            Ok(match self {
                Self::Unix(path) | Self::Origin { path, .. } => SocketAddress::Unix(path.clone()),
                Self::Peer(value) => {
                    SocketAddress::Inet(value.parse().map_err(|_| Error::InvalidConfiguration)?)
                }
            })
        }
        fn capacity(&self, config: &http1::connection::PoolConfig) -> usize {
            match self {
                Self::Peer(_) => config.per_endpoint,
                _ => config.secondary_cap,
            }
        }
        fn priority_headroom(&self) -> usize {
            usize::from(matches!(self, Self::Origin { .. }))
        }
        fn allocation(&self) -> usize {
            match self {
                Self::Unix(path) => path.as_os_str().len(),
                Self::Origin { cache, path } => cache.0.len() + path.as_os_str().len(),
                Self::Peer(value) => value.len(),
            }
        }
    }
    pub struct HttpPool {
        core: http1::connection::HttpPool<HttpContext>,
        #[cfg(test)]
        reactor: Rc<Reactor>,
        #[cfg(test)]
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    }
    impl Deref for HttpPool {
        type Target = http1::connection::HttpPool<HttpContext>;
        fn deref(&self) -> &Self::Target {
            &self.core
        }
    }
    impl HttpPool {
        pub fn new(
            reactor: Rc<Reactor>,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            per_endpoint: usize,
        ) -> Self {
            Self::with_limits(
                reactor,
                admission,
                per_endpoint,
                256,
                Duration::from_secs(30),
            )
        }
        pub fn with_limits(
            reactor: Rc<Reactor>,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            per_endpoint: usize,
            max_endpoints: usize,
            idle_timeout: Duration,
        ) -> Self {
            Self {
                core: http1::connection::HttpPool::new(
                    reactor.clone(),
                    Rc::new(HttpContext(admission.clone())),
                    http1::connection::PoolConfig {
                        per_endpoint,
                        secondary_cap: per_endpoint,
                        max_endpoints,
                        idle_timeout,
                        waiter_cap: admission.limits().queue_entries.get(),
                        tcp_nodelay: false,
                    },
                ),
                #[cfg(test)]
                reactor,
                #[cfg(test)]
                admission,
            }
        }
        pub fn with_origin_limit(mut self, limit: usize) -> Self {
            self.core.config_mut().secondary_cap = limit;
            self
        }
        pub fn with_peer_tcp_nodelay(mut self, enabled: bool) -> Self {
            self.core.config_mut().tcp_nodelay = enabled;
            self
        }
        pub(crate) fn checkout_peer<'a>(
            &'a self,
            endpoint: &'a Endpoint,
            relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
            peer: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
            failure: Option<Rc<std::cell::Cell<bool>>>,
            scope: &'a RequestScope,
        ) -> Operation<'a, ConnectionLease> {
            self.core.checkout_with_state(
                endpoint,
                State {
                    relay_reservation: relay,
                    peer_admission: peer,
                    connect_failure: failure,
                    ..State::default()
                },
                scope,
            )
        }
    }
    fn peer_connect_failure(errno: Option<i32>) -> bool {
        matches!(
            errno,
            Some(libc::ECONNREFUSED | libc::ECONNRESET | libc::EPIPE)
        )
    }
    #[cfg(test)]
    mod adaptive_tests;
    #[cfg(test)]
    pub(crate) mod io_tests;
    #[cfg(test)]
    mod pool_tests;
    #[path = "relay.rs"]
    mod relay;
    #[cfg(test)]
    mod relay_tests;
}
