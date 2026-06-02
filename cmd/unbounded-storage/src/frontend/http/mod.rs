// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The HTTP frontend: a thin factory plus its [`ServePolicy`].
//!
//! This module owns two concerns:
//!
//! 1. **The factory** ([`HttpFrontend`]): validates a
//!    [`FrontendSpec`](crate::config::FrontendSpec) and binds a
//!    `SO_REUSEPORT` listening socket so every shard can accept on the
//!    same port. It does not implement the `Frontend` trait: the
//!    concrete bufferpool type is only nameable in the binary, so the
//!    binary calls this directly.
//! 2. **The policy** ([`HttpPolicy`], in [`policy`]): the
//!    protocol-specific behavior - routing a request target to an
//!    origin object, resolving its length from an origin `HEAD`
//!    (cached per-shard), and shaping HTTP response heads / errors.
//!
//! The serving engine itself (accept loop, head parsing, zero-copy
//! body streaming) lives in [`super::conn`] and is shared with the S3
//! frontend; the per-shard driver is the generic
//! [`ServeDriver`](super::conn::ServeDriver) specialized to
//! [`HttpPolicy`], re-exported here as [`HttpDriver`].
//!
//! Linux-gated because serving depends on the io_uring
//! [`NetHandle`](crate::ring::NetHandle).

mod policy;

use std::net::SocketAddr;
use std::os::fd::RawFd;

use crate::config::{FrontendKind, FrontendSpec};
use crate::frontend::FrontendError;
use crate::frontend::conn::{ServeDriver, bind_listener};

pub use policy::HttpPolicy;

/// Default TTL (milliseconds) for the per-shard object-length cache.
const DEFAULT_META_TTL_MS: u64 = 30_000;

/// Per-shard HTTP serving driver: the shared [`ServeDriver`] engine
/// specialized to [`HttpPolicy`]. The concrete bufferpool type `P` is
/// only nameable in the binary, which constructs this directly with a
/// `Rc<HttpPolicy>`.
pub type HttpDriver<P> = ServeDriver<P, HttpPolicy>;

/// HTTP serving frontend factory. Built once per [`FrontendSpec`];
/// holds only the immutable configuration distilled from the spec.
///
/// Unlike a `Frontend` trait impl this is called directly by the
/// binary: the per-shard [`HttpDriver`] is generic over the concrete
/// bufferpool type, which is only nameable there, so the binary binds
/// the listener via [`HttpFrontend::bind_listener`] and constructs the
/// driver from an [`HttpPolicy`].
pub struct HttpFrontend {
    id: String,
    bind: SocketAddr,
    backend_id: String,
    meta_ttl_ms: u64,
}

impl HttpFrontend {
    /// Construct from a [`FrontendSpec`], validating the kind and
    /// parsing the bind address. A configured `tls` block is accepted
    /// but ignored (loopback-only in v1).
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        if spec.kind != FrontendKind::Http {
            return Err(FrontendError::UnsupportedKind("non-http frontend kind"));
        }
        let bind = spec
            .bind
            .parse::<SocketAddr>()
            .map_err(|_| FrontendError::BadBind(spec.bind.clone()))?;
        Ok(Self {
            id: spec.id.clone(),
            bind,
            backend_id: spec.backend.clone(),
            meta_ttl_ms: DEFAULT_META_TTL_MS,
        })
    }

    /// Stable identifier, matching [`FrontendSpec::id`].
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The backend id this frontend resolves origin metadata and stripe
    /// fetches against.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// The configured listen address.
    pub fn bind(&self) -> SocketAddr {
        self.bind
    }

    /// Object-length cache TTL in milliseconds.
    pub fn meta_ttl_ms(&self) -> u64 {
        self.meta_ttl_ms
    }

    /// Create, bind, and listen the per-shard accept socket with
    /// `SO_REUSEPORT` (and `SO_REUSEADDR`) so every shard accepts on the
    /// same port, returning the listening [`RawFd`]. Linux-only.
    ///
    /// The caller owns the returned fd and is responsible for closing
    /// it; [`HttpDriver`] only reads it for `accept`.
    pub fn bind_listener(&self) -> Result<RawFd, FrontendError> {
        bind_listener(self.bind)
    }
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::rc::Rc;

    use super::*;
    use crate::bufferpool::{BufferPool, Error, ReadStream};
    use crate::config::TlsCfg;
    use crate::ring::{NetHandle, SockAddr};
    use crate::storage::StripeReq;

    fn spec(id: &str, bind: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::Http,
            bind: bind.to_string(),
            backend: "primary".to_string(),
            catalog: None,
            tls: None,
        }
    }

    #[test]
    fn from_spec_validates_kind_and_bind() {
        let f = HttpFrontend::from_spec(&spec("workload", "0.0.0.0:9000")).unwrap();
        assert_eq!(f.id(), "workload");
        assert_eq!(f.backend_id(), "primary");
        assert_eq!(f.bind(), "0.0.0.0:9000".parse().unwrap());
        assert_eq!(f.meta_ttl_ms(), DEFAULT_META_TTL_MS);

        let bad = HttpFrontend::from_spec(&spec("f", "not-an-addr"));
        assert!(matches!(bad, Err(FrontendError::BadBind(_))));
    }

    #[test]
    fn from_spec_rejects_non_http_kind() {
        let mut s = spec("f", "127.0.0.1:9000");
        s.kind = FrontendKind::S3;
        assert!(matches!(
            HttpFrontend::from_spec(&s),
            Err(FrontendError::UnsupportedKind(_))
        ));
    }

    #[test]
    fn from_spec_accepts_tls_block_but_ignores_it() {
        let mut s = spec("f", "127.0.0.1:9000");
        s.tls = Some(TlsCfg {
            cert_path: None,
            key_path: None,
            secret_ref: Some("k8s://ns/name".into()),
        });
        assert!(HttpFrontend::from_spec(&s).is_ok());
    }

    /// A mock pool whose `read` never constructs a `ReadStream` (that
    /// constructor is crate-internal to bufferpool): it always errors.
    /// Sufficient to wire an [`HttpDriver`] for accept-loop tests, where
    /// the serve path is not exercised against a real pool.
    struct MockPool;

    impl BufferPool for MockPool {
        type Req = StripeReq;

        async fn read<'p>(
            &'p self,
            _req: &'p StripeReq,
            _offset: u64,
            _len: u64,
        ) -> Result<ReadStream<'p>, Error> {
            Err(Error::from("mock pool has no data"))
        }
    }

    #[test]
    fn driver_idle_progress_returns_false_without_clients() {
        // Needs a real socket ring; skip gracefully when unavailable.
        let ring = match crate::ring::NetworkRing::new(16) {
            Ok(r) => Rc::new(RefCell::new(r)),
            Err(e) => {
                eprintln!("driver_idle_progress: ring unavailable: {e}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(ring);
        let listen_fd = match HttpFrontend::from_spec(&spec("f", "127.0.0.1:0"))
            .unwrap()
            .bind_listener()
        {
            Ok(fd) => fd,
            Err(e) => {
                eprintln!("driver_idle_progress: bind failed: {e}; skipping");
                return;
            }
        };
        let origin = {
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: 8080u16.to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(std::net::Ipv4Addr::new(127, 0, 0, 1)).to_be(),
                },
                sin_zero: [0; 8],
            };
            SockAddr::from_sockaddr_in(sin)
        };
        let policy = Rc::new(HttpPolicy::new(
            "primary".to_string(),
            4 * 1024 * 1024,
            origin,
            DEFAULT_META_TTL_MS,
        ));
        let mut driver: HttpDriver<MockPool> = ServeDriver::new(
            Rc::new(MockPool),
            policy,
            handle,
            listen_fd,
            2 * 1024 * 1024,
        );
        // No client has connected: accept is pending, no conns, so the
        // engine reports no work and stays idle.
        assert!(!driver.progress());
        assert_eq!(driver.conn_count(), 0);
        assert!(driver.is_idle());

        unsafe {
            libc::close(listen_fd);
        }
    }
}
