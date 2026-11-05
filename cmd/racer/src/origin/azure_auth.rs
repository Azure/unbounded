//! Azure Storage bearer tokens for the native Azure Blob origin.
//!
//! Two credential sources are supported:
//!
//! - Managed identity through the instance metadata service (IMDS):
//!   `GET http://169.254.169.254/metadata/identity/oauth2/token` with
//!   `Metadata: true`, optionally selecting a user-assigned identity by client ID.
//! - Workload identity federation: the projected service account token named by
//!   `RACER_AZURE_FEDERATED_TOKEN_FILE` (reread on every exchange) is exchanged at
//!   `<authority><tenant>/oauth2/v2.0/token`, where the authority is
//!   `RACER_AZURE_AUTHORITY_HOST` (default `https://login.microsoftonline.com/`),
//!   with the client credentials grant and a JWT bearer client assertion. Without a
//!   configured tenant, the tenant comes from a storage `WWW-Authenticate` challenge
//!   (see [`parse_bearer_challenge`] and [`AzureTokenCache::learn_tenant`]).
//!
//! [`AzureTokenCache`] is node-wide (`Send + Sync`, share it in an `Arc`).
//! [`AzureTokenSource`] is the worker-local handle: it fetches on the worker's
//! reactor and allows one refresh at a time per worker. Tokens refresh ahead of
//! expiry by the smaller of five minutes and half their lifetime; an expired
//! cached token is never returned. Tokens are never logged or formatted.

use crate::error::Error;
use crate::remote::DnsResolver;
use crate::remote::MAX_SMALL_BODY;
use crate::remote::RemoteEndpoint;
use crate::remote::Resolver;
use crate::remote::SmallRequest;
use crate::remote::exchange_small;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use http1::tls::rustls;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::SystemTime;
use zeroize::Zeroizing;

/// Token audience for Azure Storage (IMDS `resource`).
pub const STORAGE_RESOURCE: &str = "https://storage.azure.com/";

/// Token scope for Azure Storage (Entra v2 `scope`).
pub const STORAGE_SCOPE: &str = "https://storage.azure.com/.default";

/// Default Entra authority host.
pub const DEFAULT_AUTHORITY_HOST: &str = "https://login.microsoftonline.com/";

/// IMDS address.
pub const IMDS_ADDRESS: &str = "169.254.169.254";

/// Largest refresh lead before expiry.
pub const REFRESH_LEAD: Duration = Duration::from_secs(300);

/// Largest federated token file accepted.
const MAX_ASSERTION: u64 = 64 * 1024;

/// Credential selection, mirroring the published origin credential.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AzureCredential {
    /// IMDS managed identity; `client_id` selects a user-assigned identity.
    ManagedIdentity {
        /// User-assigned identity client ID, or the system-assigned identity.
        client_id: Option<String>,
    },

    /// Workload identity federation with an Entra application.
    WorkloadIdentity {
        /// Entra application (client) ID.
        client_id: String,

        /// Tenant ID; discovered from a storage challenge when absent.
        tenant_id: Option<String>,
    },
}

/// Token acquisition failure. Never carries token or assertion bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AuthError {
    /// Transport failure or a retryable token service response (408, 429, 5xx,
    /// and IMDS 404/410 while the identity is provisioned).
    Unavailable,

    /// The token service rejected the request with this status.
    Rejected(u16),

    /// The token service response could not be parsed or is already expired.
    Malformed,

    /// Workload identity has no tenant yet; supply one from a storage challenge.
    TenantUnknown,

    /// Invalid credential settings or an unreadable federated token file.
    Configuration,

    /// The caller's scope was canceled.
    Cancelled,

    /// The caller's deadline expired.
    DeadlineExceeded,
}

impl std::fmt::Display for AuthError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Unavailable => f.write_str("token service unavailable"),
            Self::Rejected(status) => write!(f, "token request rejected with status {status}"),
            Self::Malformed => f.write_str("malformed token response"),
            Self::TenantUnknown => f.write_str("workload identity tenant unknown"),
            Self::Configuration => f.write_str("invalid Azure credential configuration"),
            Self::Cancelled => f.write_str("canceled"),
            Self::DeadlineExceeded => f.write_str("deadline exceeded"),
        }
    }
}

impl std::error::Error for AuthError {}

impl From<Error> for AuthError {
    fn from(error: Error) -> Self {
        match error {
            Error::Cancelled => Self::Cancelled,
            Error::DeadlineExceeded => Self::DeadlineExceeded,
            Error::InvalidConfiguration => Self::Configuration,
            _ => Self::Unavailable,
        }
    }
}

impl From<AuthError> for Error {
    fn from(error: AuthError) -> Self {
        match error {
            AuthError::Cancelled => Self::Cancelled,
            AuthError::DeadlineExceeded => Self::DeadlineExceeded,
            AuthError::Configuration => Self::InvalidConfiguration,
            AuthError::Rejected(_) | AuthError::TenantUnknown => Self::OriginForbidden,
            AuthError::Unavailable | AuthError::Malformed => Self::Unavailable,
        }
    }
}

/// Token service locations. [`AuthEndpoints::new`] gives production values.
#[derive(Clone)]
pub struct AuthEndpoints {
    /// IMDS endpoint (plain HTTP).
    pub imds: RemoteEndpoint,

    /// Entra authority base URL ending in `/`, for example
    /// `https://login.microsoftonline.com/`.
    pub authority: String,

    /// Federated token file for workload identity.
    pub federated_token_file: Option<PathBuf>,

    /// TLS configuration for an `https` authority: the node's origin trust
    /// roots, loaded at assembly. `None` (no usable bundle) makes workload
    /// identity over `https` a configuration error.
    pub tls: Option<Arc<rustls::ClientConfig>>,
}

impl AuthEndpoints {
    /// IMDS at 169.254.169.254:80, the configured authority and federated
    /// token file, and the node's loaded trust roots.
    pub fn new(
        settings: &crate::config::OriginSettings,
        tls: Option<Arc<rustls::ClientConfig>>,
    ) -> Self {
        Self {
            imds: RemoteEndpoint::http(IMDS_ADDRESS, 80).expect("IMDS literal"),
            authority: settings.azure_authority_host.clone(),
            federated_token_file: settings.azure_federated_token_file.clone(),
            tls,
        }
    }
}

/// A cached token with its absolute expiry and refresh times.
#[derive(Clone)]
struct CachedToken {
    token: Arc<str>,

    expires: SystemTime,

    refresh: SystemTime,
}

/// Node-wide credential configuration and token cache.
pub struct AzureTokenCache {
    credential: AzureCredential,

    endpoints: AuthEndpoints,

    authority: Option<(RemoteEndpoint, String)>,

    tenant: Mutex<Option<String>>,

    token: Mutex<Option<CachedToken>>,

    /// Node-wide single-flight refresh shared by every worker's source.
    flight: Mutex<Flight>,
}

/// State of the node-wide token refresh. At most one worker fetches at a
/// time; the others park their wakers here and, when the fetch completes,
/// adopt its token or its failure instead of fetching again.
#[derive(Default)]
struct Flight {
    /// A fetch is running.
    running: bool,

    /// Completed fetches; a waiter compares it to detect completion.
    completed: u64,

    /// Failure of the latest completed fetch.
    failure: Option<AuthError>,

    /// Waiters parked on the running fetch, from any worker thread.
    waiters: Vec<std::task::Waker>,
}

/// Leadership of the node-wide refresh. Dropping it without
/// [`FlightGuard::finish`] (a cancelled fetch) releases leadership without
/// a result, so a waiter takes over.
struct FlightGuard<'a> {
    cache: &'a AzureTokenCache,
}

impl FlightGuard<'_> {
    fn finish(self, failure: Option<AuthError>) {
        let mut flight = self.cache.flight();
        flight.completed += 1;
        flight.failure = failure;
    }
}

impl Drop for FlightGuard<'_> {
    fn drop(&mut self) {
        let waiters = {
            let mut flight = self.cache.flight();
            flight.running = false;
            std::mem::take(&mut flight.waiters)
        };
        for waker in waiters {
            waker.wake();
        }
    }
}

/// Resolves once the fetch running at creation ends (or none is running).
struct FlightWait<'a> {
    cache: &'a AzureTokenCache,

    seen: u64,
}

impl std::future::Future for FlightWait<'_> {
    type Output = ();

    fn poll(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<()> {
        let mut flight = self.cache.flight();
        if !flight.running || flight.completed != self.seen {
            return std::task::Poll::Ready(());
        }
        if !flight.waiters.iter().any(|w| w.will_wake(cx.waker())) {
            flight.waiters.push(cx.waker().clone());
        }
        std::task::Poll::Pending
    }
}

impl std::fmt::Debug for AzureTokenCache {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AzureTokenCache")
            .field("credential", &self.credential)
            .finish_non_exhaustive()
    }
}

impl AzureTokenCache {
    /// Validate settings and create an empty cache.
    pub fn new(credential: AzureCredential, endpoints: AuthEndpoints) -> Result<Self, AuthError> {
        let mut tenant = None;
        match &credential {
            AzureCredential::ManagedIdentity { client_id } => {
                if client_id.as_deref().is_some_and(|id| !valid_id(id)) {
                    return Err(AuthError::Configuration);
                }
            }
            AzureCredential::WorkloadIdentity {
                client_id,
                tenant_id,
            } => {
                if !valid_id(client_id)
                    || tenant_id.as_deref().is_some_and(|id| !valid_id(id))
                    || endpoints.federated_token_file.is_none()
                {
                    return Err(AuthError::Configuration);
                }
                tenant = tenant_id.clone();
            }
        }
        let authority = match &credential {
            AzureCredential::WorkloadIdentity { .. } => Some(parse_authority(
                &endpoints.authority,
                endpoints.tls.clone(),
            )?),
            AzureCredential::ManagedIdentity { .. } => None,
        };
        Ok(Self {
            credential,
            endpoints,
            authority,
            tenant: Mutex::new(tenant),
            token: Mutex::new(None),
            flight: Mutex::default(),
        })
    }

    /// The configured credential.
    pub fn credential(&self) -> &AzureCredential {
        &self.credential
    }

    /// The tenant used for workload identity exchanges, if known.
    pub fn tenant(&self) -> Option<String> {
        self.tenant
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Adopt the tenant from a storage `WWW-Authenticate` challenge when none is
    /// configured. Returns whether the challenge named a tenant now in use.
    pub fn learn_tenant(&self, www_authenticate: &[u8]) -> bool {
        let AzureCredential::WorkloadIdentity { tenant_id, .. } = &self.credential else {
            return false;
        };
        let Some(challenge) = parse_bearer_challenge(www_authenticate) else {
            return false;
        };
        if tenant_id.is_some() {
            return tenant_id.as_deref() == Some(challenge.tenant_id.as_str());
        }
        let mut tenant = self.tenant.lock().unwrap_or_else(|e| e.into_inner());
        if tenant.as_deref() != Some(challenge.tenant_id.as_str()) {
            *tenant = Some(challenge.tenant_id);
            // A token from another tenant must not be reused.
            self.invalidate();
        }
        true
    }

    /// Drop the cached token (for example after a 401 from storage).
    pub fn invalidate(&self) {
        *self.token.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }

    /// Drop the cached token only if it is `sent`, so a stale rejection does
    /// not evict a newer token another request or worker already fetched.
    /// Returns whether it was dropped.
    pub fn invalidate_sent(&self, sent: &[u8]) -> bool {
        let mut token = self.token.lock().unwrap_or_else(|e| e.into_inner());
        let held = token.as_ref().is_some_and(|t| t.token.as_bytes() == sent);
        if held {
            *token = None;
        }
        held
    }

    /// Whether the cached token is `sent`.
    pub fn holds(&self, sent: &[u8]) -> bool {
        self.token
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .as_ref()
            .is_some_and(|t| t.token.as_bytes() == sent)
    }

    /// The cached token if it is not yet due for refresh.
    fn fresh(&self, now: SystemTime) -> Option<Arc<str>> {
        let token = self.token.lock().unwrap_or_else(|e| e.into_inner());
        token
            .as_ref()
            .filter(|t| now < t.refresh)
            .map(|t| t.token.clone())
    }

    /// The cached token if it has not expired (used when a refresh fails).
    fn unexpired(&self, now: SystemTime) -> Option<Arc<str>> {
        let token = self.token.lock().unwrap_or_else(|e| e.into_inner());
        token
            .as_ref()
            .filter(|t| now + Duration::from_secs(10) < t.expires)
            .map(|t| t.token.clone())
    }

    fn flight(&self) -> std::sync::MutexGuard<'_, Flight> {
        self.flight.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Lead the refresh, or learn the completion count of the running one.
    fn begin(&self) -> Result<FlightGuard<'_>, u64> {
        let mut flight = self.flight();
        if flight.running {
            return Err(flight.completed);
        }
        flight.running = true;
        Ok(FlightGuard { cache: self })
    }

    /// The failure of the fetch that completed after `seen`, if one did.
    fn completed_since(&self, seen: u64) -> Option<Option<AuthError>> {
        let flight = self.flight();
        (flight.completed != seen).then_some(flight.failure)
    }

    fn store(&self, token: CachedToken) {
        *self.token.lock().unwrap_or_else(|e| e.into_inner()) = Some(token);
    }
}

/// Worker-local token handle over a shared [`AzureTokenCache`].
pub struct AzureTokenSource {
    cache: Arc<AzureTokenCache>,

    reactor: Rc<Reactor>,

    resolver: Rc<dyn Resolver>,
}

impl AzureTokenSource {
    /// Fetch on `reactor`, resolving names through bounded DNS.
    pub fn new(cache: Arc<AzureTokenCache>, reactor: Rc<Reactor>) -> Self {
        let resolver = Rc::new(DnsResolver::new(reactor.clone()));
        Self::with_resolver(cache, reactor, resolver)
    }

    /// Fetch on `reactor` with an explicit resolver.
    pub fn with_resolver(
        cache: Arc<AzureTokenCache>,
        reactor: Rc<Reactor>,
        resolver: Rc<dyn Resolver>,
    ) -> Self {
        Self {
            cache,
            reactor,
            resolver,
        }
    }

    /// The shared cache.
    pub fn cache(&self) -> &Arc<AzureTokenCache> {
        &self.cache
    }

    /// Drop the cached token after storage rejects it (401).
    pub fn invalidate(&self) {
        self.cache.invalidate();
    }

    /// A storage bearer token (without the `Bearer ` prefix). Uses the cached
    /// token until its refresh time; if a refresh fails, an unexpired cached
    /// token is still returned. One refresh runs per node: requests on any
    /// worker that find the token due wait for it and share its result.
    pub async fn bearer(&self, scope: &RequestScope) -> Result<Arc<str>, AuthError> {
        loop {
            scope.check()?;
            if let Some(token) = self.cache.fresh(self.wall_now()) {
                return Ok(token);
            }
            let seen = match self.cache.begin() {
                Ok(flight) => return self.refresh(flight, scope).await,
                Err(seen) => seen,
            };
            self.wait(seen, scope).await?;
            // A finished fetch answers every waiter; a cancelled one does not,
            // and the next pass leads a new fetch.
            if let Some(failure) = self.cache.completed_since(seen) {
                let now = self.wall_now();
                if let Some(token) = self.cache.fresh(now) {
                    return Ok(token);
                }
                if let Some(error) = failure {
                    return self.cache.unexpired(now).ok_or(error);
                }
            }
        }
    }

    fn wall_now(&self) -> SystemTime {
        self.reactor.ctx().clock.wall_now()
    }

    /// Run the node-wide refresh as its leader.
    async fn refresh(
        &self,
        flight: FlightGuard<'_>,
        scope: &RequestScope,
    ) -> Result<Arc<str>, AuthError> {
        // Another worker may have refreshed between the check and `begin`.
        if let Some(token) = self.cache.fresh(self.wall_now()) {
            flight.finish(None);
            return Ok(token);
        }
        match self.fetch(scope).await {
            Ok(token) => {
                let value = token.token.clone();
                self.cache.store(token);
                flight.finish(None);
                Ok(value)
            }
            Err(error) => {
                if !matches!(error, AuthError::Cancelled | AuthError::DeadlineExceeded) {
                    flight.finish(Some(error));
                    if let Some(token) = self.cache.unexpired(self.wall_now()) {
                        return Ok(token);
                    }
                }
                Err(error)
            }
        }
    }

    /// Wait for the running refresh under the waiter's own deadline and
    /// cancellation. Giving up only drops this waiter; the fetch runs on under
    /// its leader's scope and still fills the cache.
    async fn wait(&self, seen: u64, scope: &RequestScope) -> Result<(), AuthError> {
        use futures::future::{Either, select};
        let expiry = self.reactor.sleep_until(scope.deadline.0, scope);
        let wait = FlightWait {
            cache: &self.cache,
            seen,
        };
        match select(wait, expiry).await {
            Either::Left(((), _)) => Ok(()),
            Either::Right((slept, _)) => {
                slept?;
                scope.check()?;
                // The timer fired at the deadline; report it even if the
                // clock reads a hair early.
                Err(AuthError::DeadlineExceeded)
            }
        }
    }

    async fn fetch(&self, scope: &RequestScope) -> Result<CachedToken, AuthError> {
        let issued = self.wall_now();
        let response = match &self.cache.credential {
            AzureCredential::ManagedIdentity { client_id } => {
                let mut target = format!(
                    "/metadata/identity/oauth2/token?api-version=2018-02-01&resource={}",
                    form_encode(STORAGE_RESOURCE)
                );
                if let Some(id) = client_id {
                    target.push_str("&client_id=");
                    target.push_str(id);
                }
                exchange_small(
                    &self.reactor,
                    self.resolver.as_ref(),
                    &self.cache.endpoints.imds,
                    &SmallRequest {
                        method: "GET",
                        target: &target,
                        headers: &[("Metadata", "true"), ("Accept", "application/json")],
                        body: &[],
                    },
                    MAX_SMALL_BODY,
                    scope,
                )
                .await?
            }
            AzureCredential::WorkloadIdentity { client_id, .. } => {
                let tenant = self.cache.tenant().ok_or(AuthError::TenantUnknown)?;
                let (endpoint, prefix) = self
                    .cache
                    .authority
                    .as_ref()
                    .ok_or(AuthError::Configuration)?;
                let assertion = read_assertion(
                    &self.reactor,
                    self.cache
                        .endpoints
                        .federated_token_file
                        .as_deref()
                        .ok_or(AuthError::Configuration)?,
                    scope,
                )
                .await?;
                let mut body = Zeroizing::new(String::with_capacity(assertion.len() + 256));
                body.push_str("client_assertion=");
                body.push_str(&form_encode(&assertion));
                body.push_str("&client_assertion_type=");
                body.push_str(&form_encode(
                    "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
                ));
                body.push_str("&client_id=");
                body.push_str(&form_encode(client_id));
                body.push_str("&grant_type=client_credentials&scope=");
                body.push_str(&form_encode(STORAGE_SCOPE));
                let target = format!("{prefix}{tenant}/oauth2/v2.0/token");
                exchange_small(
                    &self.reactor,
                    self.resolver.as_ref(),
                    endpoint,
                    &SmallRequest {
                        method: "POST",
                        target: &target,
                        headers: &[
                            ("Content-Type", "application/x-www-form-urlencoded"),
                            ("Accept", "application/json"),
                        ],
                        body: body.as_bytes(),
                    },
                    MAX_SMALL_BODY,
                    scope,
                )
                .await?
            }
        };
        match response.status {
            200 => parse_token(&response.body, issued, self.wall_now()),
            408 | 429 | 500..=599 => Err(AuthError::Unavailable),
            // IMDS returns 404 and 410 while an identity is being provisioned.
            404 | 410
                if matches!(
                    self.cache.credential,
                    AzureCredential::ManagedIdentity { .. }
                ) =>
            {
                Err(AuthError::Unavailable)
            }
            status => Err(AuthError::Rejected(status)),
        }
    }
}

/// Parse `scheme://host[:port]/path/` into an endpoint and a path prefix ending in `/`.
fn parse_authority(
    url: &str,
    tls: Option<Arc<rustls::ClientConfig>>,
) -> Result<(RemoteEndpoint, String), AuthError> {
    let (https, rest) = if let Some(rest) = url.strip_prefix("https://") {
        (true, rest)
    } else if let Some(rest) = url.strip_prefix("http://") {
        (false, rest)
    } else {
        return Err(AuthError::Configuration);
    };
    let (authority, path) = rest.split_at(rest.find('/').unwrap_or(rest.len()));
    if authority.is_empty() || authority.contains('@') || path.contains(['?', '#']) {
        return Err(AuthError::Configuration);
    }
    let (host, port) = match authority.rsplit_once(':') {
        Some((host, port)) if !host.ends_with(':') && !port.contains(']') => (
            host,
            port.parse::<u16>().map_err(|_| AuthError::Configuration)?,
        ),
        _ => (authority, if https { 443 } else { 80 }),
    };
    let mut prefix = if path.is_empty() {
        "/".to_owned()
    } else {
        path.to_owned()
    };
    if !prefix.ends_with('/') {
        prefix.push('/');
    }
    let endpoint = if https {
        // Without usable trust roots an https authority is a configuration
        // error; no file is read here, on a worker.
        let config = tls.ok_or(AuthError::Configuration)?;
        RemoteEndpoint::https(host, port, config)
    } else {
        RemoteEndpoint::http(host, port)
    }
    .map_err(|_| AuthError::Configuration)?;
    Ok((endpoint, prefix))
}

/// GUID-like identifiers: ASCII letters, digits, `-`, and `.` only.
fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'.')
}

/// Read and trim the federated token through the worker's io_uring file
/// operations, so the reactor never blocks on the file. It is reread for
/// every exchange because the kubelet rotates the projected file.
async fn read_assertion(
    reactor: &Reactor,
    path: &std::path::Path,
    scope: &RequestScope,
) -> Result<Zeroizing<String>, AuthError> {
    use uring_runtime::reactor::filesystem::secure;
    // One byte over the cap tells an oversized file from a full one.
    let bytes = secure::read_path(reactor, path, MAX_ASSERTION as usize + 1, scope)
        .await
        .map_err(|error| match error {
            Error::Cancelled => AuthError::Cancelled,
            Error::DeadlineExceeded => AuthError::DeadlineExceeded,
            _ => AuthError::Configuration,
        })?;
    parse_assertion(&bytes)
}

/// Validate and trim a federated token read from its file.
fn parse_assertion(bytes: &[u8]) -> Result<Zeroizing<String>, AuthError> {
    if bytes.len() as u64 > MAX_ASSERTION {
        return Err(AuthError::Configuration);
    }
    let text = std::str::from_utf8(bytes).map_err(|_| AuthError::Configuration)?;
    let text = text.trim();
    if text.is_empty() {
        return Err(AuthError::Configuration);
    }
    Ok(Zeroizing::new(text.to_owned()))
}

/// `application/x-www-form-urlencoded` component encoding.
fn form_encode(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for b in value.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// Parse an IMDS or Entra token response. `expires_on` (absolute seconds) wins
/// over `expires_in` (relative to `issued`); numbers may be JSON strings.
/// `now` is the wall time the response arrived.
fn parse_token(body: &[u8], issued: SystemTime, now: SystemTime) -> Result<CachedToken, AuthError> {
    let value: serde_json::Value =
        serde_json::from_slice(body).map_err(|_| AuthError::Malformed)?;
    let token = value
        .get("access_token")
        .and_then(|v| v.as_str())
        .filter(|t| !t.is_empty() && t.bytes().all(|b| b.is_ascii_graphic()))
        .ok_or(AuthError::Malformed)?;
    let number = |name: &str| {
        value.get(name).and_then(|v| {
            v.as_u64()
                .or_else(|| v.as_str().and_then(|s| s.trim().parse().ok()))
        })
    };
    let expires = if let Some(on) = number("expires_on") {
        SystemTime::UNIX_EPOCH + Duration::from_secs(on)
    } else if let Some(within) = number("expires_in") {
        issued + Duration::from_secs(within)
    } else {
        return Err(AuthError::Malformed);
    };
    let lifetime = expires
        .duration_since(now)
        .map_err(|_| AuthError::Malformed)?;
    Ok(CachedToken {
        token: Arc::from(token),
        expires,
        refresh: refresh_time(now, lifetime),
    })
}

/// Refresh at the smaller lead of [`REFRESH_LEAD`] and half the remaining lifetime.
fn refresh_time(now: SystemTime, lifetime: Duration) -> SystemTime {
    now + lifetime - REFRESH_LEAD.min(lifetime / 2)
}

/// A storage bearer challenge.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BearerChallenge {
    /// `authorization_uri` parameter.
    pub authorization_uri: String,

    /// Tenant: the first path segment of `authorization_uri`.
    pub tenant_id: String,

    /// `resource_id` parameter, if present.
    pub resource_id: Option<String>,
}

/// Parse `WWW-Authenticate: Bearer authorization_uri=https://login.microsoftonline.com/<tenant>/oauth2/authorize resource_id=...`.
/// Parameters may be separated by spaces or commas, and values may be quoted.
pub fn parse_bearer_challenge(header: &[u8]) -> Option<BearerChallenge> {
    let text = std::str::from_utf8(header).ok()?.trim();
    let (scheme, rest) = text.split_at(text.find(char::is_whitespace)?);
    if !scheme.eq_ignore_ascii_case("bearer") {
        return None;
    }
    let mut authorization_uri = None;
    let mut resource_id = None;
    let mut rest = rest.trim_start_matches(|c: char| c.is_whitespace() || c == ',');
    while !rest.is_empty() {
        let equals = rest.find('=')?;
        let name = rest[..equals].trim();
        rest = &rest[equals + 1..];
        let value;
        if let Some(quoted) = rest.strip_prefix('"') {
            let end = quoted.find('"')?;
            value = &quoted[..end];
            rest = &quoted[end + 1..];
        } else {
            let end = rest
                .find(|c: char| c.is_whitespace() || c == ',')
                .unwrap_or(rest.len());
            value = &rest[..end];
            rest = &rest[end..];
        }
        rest = rest.trim_start_matches(|c: char| c.is_whitespace() || c == ',');
        if name.eq_ignore_ascii_case("authorization_uri") {
            authorization_uri = Some(value.to_owned());
        } else if name.eq_ignore_ascii_case("resource_id") {
            resource_id = Some(value.to_owned());
        }
    }
    let authorization_uri = authorization_uri?;
    let path = authorization_uri
        .strip_prefix("https://")
        .or_else(|| authorization_uri.strip_prefix("http://"))?;
    let tenant = path.split('/').nth(1)?;
    if !valid_id(tenant) {
        return None;
    }
    Some(BearerChallenge {
        tenant_id: tenant.to_owned(),
        authorization_uri,
        resource_id,
    })
}

#[cfg(test)]
mod tests;
