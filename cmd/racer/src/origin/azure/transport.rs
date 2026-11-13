//! [`AzureTransport`] over the worker's pooled HTTP/1.1 client.
//!
//! One [`RemoteEndpoint`] per container URL scheme, host, and port is cached per
//! worker, so containers in one storage account share connections. Names are
//! resolved before every checkout (cached for [`crate::remote::DNS_TTL`]).
//! HTTPS endpoints complete TLS inside the pool's connect path, using kernel TLS
//! when available, so page bodies are received straight into the admitted
//! plaintext buffer.
//!
//! A connection returns to the pool only after a response body framed by
//! `Content-Length` is fully consumed. Error bodies up to [`MAX_DISCARD`] bytes
//! are read and dropped; larger ones abandon the connection.

use super::{AzureExchange, AzureTransport};
use crate::admission::AdmissionPolicy;
use crate::error::{Error, Operation, Result};
use crate::http::{ConnectionLease, Endpoint, HttpIo, HttpPool, exchange_head};
use crate::memory::PlaintextBuffer;
use crate::model::parse_decimal;
use crate::remote::{RemoteEndpoint, Resolver};
use crate::runtime::RequestScope;
use http1::tls::rustls::ClientConfig;
use http1::{MessageHead, StartLine};
use racer_wire::control::{AzureContainerUrl, OriginDefinition};
use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;

/// Largest response head accepted from Azure.
pub const MAX_RESPONSE_HEAD: usize = 32 * 1024;

/// Largest error body read to keep a connection reusable.
pub const MAX_DISCARD: u64 = 64 * 1024;

/// Longest a non-final connect attempt may take before failing over.
const ATTEMPT_CAP: Duration = Duration::from_secs(5);

/// Shortest non-final connect attempt, so a tight request budget still gives
/// a slow but healthy address a fair chance to answer.
const ATTEMPT_FLOOR: Duration = Duration::from_millis(250);

/// Scope for one checkout attempt with `remaining` attempts left, including
/// this one. The final attempt keeps the full request scope. Earlier attempts
/// get `min(budget / remaining, ATTEMPT_CAP)`, at least `ATTEMPT_FLOOR` (never
/// past the request deadline), so an address that blackholes the TCP connect
/// or stalls the TLS handshake cannot consume the whole request before the
/// next resolved address is tried. The narrowed clone shares the request's
/// cancellation, so canceling the request still releases every attempt.
fn attempt_scope(scope: &RequestScope, remaining: usize) -> RequestScope {
    if remaining <= 1 {
        return scope.clone();
    }
    let now = scope.clock().now();
    let budget = scope.deadline.0.saturating_duration_since(now);
    let share = (budget / remaining as u32)
        .min(ATTEMPT_CAP)
        .max(ATTEMPT_FLOOR);
    let mut attempt = scope.clone();
    attempt.deadline.0 = scope.deadline.0.min(now + share);
    attempt
}

/// Process-wide TLS client configuration for HTTPS origins, loaded once when
/// the node is assembled, before any worker serves, so no worker reactor ever
/// blocks on the file. A bundle that fails to load is logged then and fails
/// each HTTPS request with `Unavailable` until restart; plain-HTTP origins
/// never need it. A successful load was never reread before either.
pub struct TrustRoots {
    path: PathBuf,
    config: Option<Arc<ClientConfig>>,
}

impl TrustRoots {
    /// Load `path` now. Call off the worker reactors (node assembly).
    pub fn load(path: PathBuf) -> Self {
        let config = match http1::tls::load_trust_bundle(&path) {
            Ok(roots) => Some(http1::tls::client_config(roots)),
            Err(error) => {
                eprintln!(
                    "racer: stage=azure-origin-trust path={} error={error} https_origins=unavailable",
                    path.display()
                );
                None
            }
        };
        Self { path, config }
    }

    /// No trust roots: every HTTPS request fails with `Unavailable`.
    pub fn unavailable(path: PathBuf) -> Self {
        Self { path, config: None }
    }

    /// Use a fixed configuration.
    pub fn fixed(config: Arc<ClientConfig>) -> Self {
        Self {
            path: PathBuf::new(),
            config: Some(config),
        }
    }

    /// The shared configuration, or `Unavailable` if the bundle failed to load.
    pub fn config(&self) -> Result<Arc<ClientConfig>> {
        self.config.clone().ok_or(Error::Unavailable)
    }
}

impl std::fmt::Debug for TrustRoots {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("TrustRoots")
            .field("path", &self.path)
            .finish_non_exhaustive()
    }
}

/// Worker-local Azure transport on the shared HTTP pool.
pub struct PoolTransport {
    pool: Rc<HttpPool>,
    io: Rc<HttpIo>,
    resolver: Rc<dyn Resolver>,
    trust: Arc<TrustRoots>,
    endpoints: RefCell<HashMap<(bool, String, u16), Endpoint>>,
}

impl PoolTransport {
    pub fn new(
        pool: Rc<HttpPool>,
        io: Rc<HttpIo>,
        resolver: Rc<dyn Resolver>,
        trust: Arc<TrustRoots>,
    ) -> Self {
        Self {
            pool,
            io,
            resolver,
            trust,
            endpoints: RefCell::default(),
        }
    }

    fn key(url: &AzureContainerUrl) -> (bool, String, u16) {
        (url.scheme_is_https(), url.host().to_owned(), url.port())
    }

    /// Cached endpoints (tests).
    #[cfg(test)]
    pub(crate) fn endpoint_count(&self) -> usize {
        self.endpoints.borrow().len()
    }

    /// Check out a connection, failing over across resolved addresses.
    async fn checkout(
        &self,
        remote: &RemoteEndpoint,
        sequence: &RemoteEndpoint,
        endpoint: &Endpoint,
        is_head: bool,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        let attempts = remote.address_count().max(1);
        loop {
            let before = sequence.addresses_taken();
            let attempt_scope = attempt_scope(scope, attempts.saturating_sub(before));
            let checkout = if is_head {
                self.pool.checkout_metadata(endpoint, &attempt_scope).await
            } else {
                self.pool.checkout_wait(endpoint, &attempt_scope).await
            };
            let taken = sequence.addresses_taken();
            match checkout {
                Ok(lease) => return Ok(lease),
                // An admission wait that hit the narrowed deadline took no
                // address; wait again under a fresh share of the request.
                Err(Error::DeadlineExceeded) if taken == before && scope.check().is_ok() => {}
                // A narrowed attempt's deadline is a stalled connect;
                // the request scope check rules out the request's own.
                Err(Error::Io | Error::Os(_) | Error::Unavailable | Error::DeadlineExceeded)
                    if taken > before && taken < attempts && scope.check().is_ok() => {}
                Err(error) => {
                    if matches!(error, Error::Io | Error::Os(_) | Error::Unavailable) {
                        remote.expire_addresses();
                    }
                    return Err(error);
                }
            }
        }
    }

    fn endpoint(&self, url: &AzureContainerUrl) -> Result<Endpoint> {
        let key = Self::key(url);
        if let Some(endpoint) = self.endpoints.borrow().get(&key) {
            return Ok(endpoint.clone());
        }
        let remote = if key.0 {
            RemoteEndpoint::https(url.host(), url.port(), self.trust.config()?)?
        } else {
            RemoteEndpoint::http(url.host(), url.port())?
        };
        let endpoint = Endpoint::Remote(remote);
        self.endpoints.borrow_mut().insert(key, endpoint.clone());
        Ok(endpoint)
    }
}

impl AzureTransport for PoolTransport {
    type Exchange = PoolExchange;

    /// Forget resolved addresses, rotation, and TLS state for endpoints no
    /// published origin uses. Exchanges in flight hold their own clone, and
    /// the pool's idle connections are bounded by its endpoint limit.
    fn retain(&self, live: &[&OriginDefinition]) {
        let live: HashSet<_> = live
            .iter()
            .map(|origin| Self::key(&origin.azure.container_url))
            .collect();
        // Dropped after the borrow is released.
        let mut endpoints = self.endpoints.borrow_mut();
        let stale: Vec<_> = endpoints.extract_if(|key, _| !live.contains(key)).collect();
        drop(endpoints);
        drop(stale);
    }

    fn exchange<'a>(
        &'a self,
        url: &'a AzureContainerUrl,
        head: MessageHead,
        scope: &'a RequestScope,
    ) -> Operation<'a, PoolExchange> {
        Box::pin(async move {
            scope.check()?;
            let Endpoint::Remote(remote) = self.endpoint(url)? else {
                return Err(Error::Internal);
            };
            remote.resolve(&*self.resolver, scope).await?;
            // This exchange's own walk over the addresses, so concurrent
            // checkouts rotating the shared cursor cannot make a retry repeat
            // an address it already tried while skipping a healthy one.
            let sequence = remote.address_sequence();
            let endpoint = Endpoint::Remote(sequence.clone());
            let is_head =
                matches!(&head.start, StartLine::Request { method, .. } if method == "HEAD");
            // HEAD may use the endpoint's priority headroom, so metadata checks
            // are not starved by page reads.
            // The first fresh connect takes the endpoint's next rotated address
            // and each retry the following resolved address, so a connect or
            // TLS failure retries on the remaining resolved addresses (nothing
            // has been sent yet) within the request scope.
            // Each checkout that still has untried addresses after it runs
            // under a narrowed scope (see [`attempt_scope`]), so a stalled
            // connect or handshake counts as a connect failure. Attempts are
            // counted by addresses actually taken, so a checkout that times out
            // while waiting for pool admission retries without spending an
            // address. Only checkout is narrowed: a reused idle connection
            // returns at once, and the send and response read below use the
            // full request scope.
            // A GET or HEAD that fails on a reused idle connection before any
            // response byte retries once after dropping the endpoint's idle
            // connections, so it runs on a fresh one: the peer most likely
            // closed the idle socket and never saw the request.
            let mut retry = Some(crate::http::copy_head(&head));
            let mut head = head;
            let response = loop {
                let lease = self
                    .checkout(&remote, &sequence, &endpoint, is_head, scope)
                    .await?;
                match exchange_head(&self.io, lease, head, scope, MAX_RESPONSE_HEAD).await {
                    Ok(response) => break response,
                    Err(failure) if failure.stale && scope.check().is_ok() => {
                        let Some(again) = retry.take() else {
                            return Err(failure.error);
                        };
                        self.pool.discard_idle(&endpoint);
                        head = again;
                    }
                    Err(failure) => return Err(failure.error),
                }
            };
            Ok(PoolExchange {
                head: response.value,
                lease: response.connection,
                _decoded: response._decoded,
                io: self.io.clone(),
                is_head,
            })
        })
    }
}

/// One pooled response.
pub struct PoolExchange {
    head: MessageHead,
    lease: ConnectionLease,
    /// Admission charge for the decoded header storage owned by `head`. It
    /// lives exactly as long as the exchange holds the head, so headers stay
    /// inside the RequestContext budget through `read_body` and `discard`.
    _decoded: Option<flow_control::Charge<AdmissionPolicy>>,
    io: Rc<HttpIo>,
    is_head: bool,
}

impl AzureExchange for PoolExchange {
    fn head(&self) -> &MessageHead {
        &self.head
    }

    fn finish(mut self) -> Result<()> {
        self.lease.finish_exchange().map_err(|_| Error::BadGateway)
    }

    fn read_body<'a>(
        self,
        buffer: PlaintextBuffer,
        scope: &'a RequestScope,
    ) -> Operation<'a, PlaintextBuffer>
    where
        Self: 'a,
    {
        Box::pin(async move {
            let mut body = self.io.read_body_exact(self.lease, buffer, scope).await?;
            body.lease
                .finish_exchange()
                .map_err(|_| Error::BadGateway)?;
            Ok(body.buffer)
        })
    }

    fn discard<'a>(self, scope: &'a RequestScope) -> Operation<'a, ()>
    where
        Self: 'a,
    {
        Box::pin(async move {
            let length = if self.is_head {
                0
            } else {
                self.head
                    .unique("Content-Length")
                    .ok()
                    .flatten()
                    .and_then(|value| parse_decimal(value).ok())
                    .filter(|length| *length <= MAX_DISCARD)
                    .ok_or(Error::BadGateway)?
            };
            if length == 0 {
                return self.finish();
            }
            let buffer = self.io.buffer(length as usize)?;
            let mut body = self.io.read_body_exact(self.lease, buffer, scope).await?;
            body.lease.finish_exchange().map_err(|_| Error::BadGateway)
        })
    }
}

#[cfg(test)]
mod tests;
