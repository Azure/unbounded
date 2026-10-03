//! Racer policy and health around the generic worker-local REST transport.
use super::{ControlEndpoint, enrollment::LocalSigningIdentity, wire};
use crate::{
    error::{Error, Operation, Result},
    runtime::deadline::RequestScope,
};
use std::{
    net::SocketAddr,
    os::fd::{AsRawFd, FromRawFd},
    rc::Rc,
    time::Instant,
};

pub use rest_client::Response as HttpResponse;

/// Racer's owner-local filesystem, timer, admission, and readiness adapter.
pub trait ControlIo {
    fn ready_charged<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        charge: Option<Rc<crate::runtime::admission::ConnectionReservation>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            if let Some(reactor) = self.reactor() {
                let interest =
                    if read { libc::POLLIN } else { 0 } | if write { libc::POLLOUT } else { 0 };
                reactor
                    .readiness_with_lease(fd, interest as u32, charge, scope)
                    .await?;
                scope.check()
            } else {
                self.ready(fd, read, write, scope).await
            }
        })
    }
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
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()>;
    fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()>;
}

// Keep the existing object-safe Racer adapter API while supplying the generic
// crate's associated types. Fixtures and production share this exact bridge.
impl rest_client::Io for dyn ControlIo {
    type Error = Error;
    type Scope = RequestScope;
    type Lease = crate::runtime::admission::ConnectionReservation;

    fn lease(&self) -> Result<Option<Rc<Self::Lease>>> {
        self.reactor()
            .map(|r| {
                r.reserve_connection(crate::model::ResourceClass::ControlConnection)
                    .map(Rc::new)
            })
            .transpose()
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        lease: Option<Rc<Self::Lease>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        self.ready_charged(fd, read, write, lease, scope)
    }
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>> {
        ControlIo::resolve(self, host, port, scope)
    }
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
        ControlIo::read_file(self, path, limit, scope)
    }
}

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
        Box::pin(async move {
            rest_client::dns::resolve(self as &dyn ControlIo, host, port, scope).await
        })
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
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
            let duration = until.saturating_duration_since(uring_runtime::environment::now());
            if duration.is_zero() {
                return Ok(());
            }
            #[cfg(test)]
            if uring_runtime::reactor::simulation::Simulation::current().is_some() {
                return std::future::poll_fn(|cx| {
                    scope.check()?;
                    if uring_runtime::environment::now() >= until {
                        std::task::Poll::Ready(Ok(()))
                    } else {
                        cx.waker().wake_by_ref();
                        std::task::Poll::Pending
                    }
                })
                .await;
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
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
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
    health: Rc<crate::topology::health::LinkHealth>,
    endpoint: crate::model::NodeId,
    inner: rest_client::Transport<dyn ControlIo>,
}
pub struct ControlConnection {
    health: Rc<crate::topology::health::LinkHealth>,
    endpoint: crate::model::NodeId,
    inner: rest_client::Connection<dyn ControlIo>,
}
impl ControlTransport {
    pub fn new(endpoint: ControlEndpoint) -> Self {
        Self {
            health: Rc::new(crate::topology::health::LinkHealth::new(1)),
            endpoint: crate::model::NodeId(endpoint.url.clone()),
            inner: rest_client::Transport::new(rest_client::Config {
                url: endpoint.url,
                trust_bundle: endpoint.trust_bundle,
                max_trust_bundle: wire::MAX_BUNDLE_BYTES,
                max_error_body: wire::MAX_ENROLLMENT_BYTES,
            }),
        }
    }
    pub fn attach_io(&self, io: Rc<dyn ControlIo>) {
        self.inner.attach_io(io);
    }
    pub fn io(&self) -> Result<Rc<dyn ControlIo>> {
        self.inner.io()
    }
    pub fn close_idle(&self) {
        self.inner.close_idle();
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
            self.health
                .run(
                    &self.endpoint,
                    Box::pin(async move {
                        scope.check()?;
                        if identity.is_some_and(|i| !i.valid_now()) {
                            return Err(Error::Unauthorized);
                        }
                        let identity = identity.map(|i| rest_client::Identity {
                            certificate_chain: i.certificate_chain(),
                            private_key: i.private_key_der(),
                            expires: i.expires_at(),
                        });
                        let inner = self.inner.connect(identity, scope).await?;
                        Ok(ControlConnection {
                            health: self.health.clone(),
                            endpoint: self.endpoint.clone(),
                            inner,
                        })
                    }),
                )
                .await
        })
    }
}
impl ControlConnection {
    pub fn request<'a>(
        self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, HttpResponse> {
        self.request_delta(method, path, token, body, limit, None, scope)
    }
    pub fn request_delta<'a>(
        self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        base: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, HttpResponse> {
        Box::pin(async move {
            self.health
                .run(
                    &self.endpoint,
                    Box::pin(async move {
                        let method = match method {
                            "GET" => rest_client::Method::Get,
                            "POST" => rest_client::Method::Post,
                            _ => return Err(Error::InvalidRequest),
                        };
                        if base.is_some_and(|b| {
                            b.len() != 64 || !b.bytes().all(|b| b.is_ascii_hexdigit())
                        }) {
                            return Err(Error::InvalidRequest);
                        }
                        self.inner
                            .request(
                                rest_client::Request {
                                    method,
                                    path,
                                    bearer: token,
                                    header: base.map(|base| ("X-Racer-Delta-Base", base)),
                                    body,
                                    limit,
                                },
                                scope,
                            )
                            .await
                    }),
                )
                .await
        })
    }
}

#[cfg(test)]
pub(super) mod scenarios;
use uring_runtime::reactor::Descriptor;
