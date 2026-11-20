//! Azure Blob container listing and token acquisition for fills.
//!
//! The authority has no Azure client, so this is a minimal Get Blob list
//! client over the control reactor's TCP and TLS primitives. It supports
//! anonymous access, IMDS managed identity, and workload identity federation.
//! Versioned blobs are not expanded.

use super::manifest::{ListedObject, MAX_FILL_OBJECTS};
use crate::{
    conn::{self, Conn},
    kube,
    rt::{self, Tcp},
    tls::TlsStream,
};
use racer_wire::control::{AzureContainerUrl, AzureCredential};
use racer_wire::object::StrongEtag;
use rustls::pki_types::ServerName;
use serde_json::Value;
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
    net::{SocketAddr, ToSocketAddrs},
    path::PathBuf,
    sync::{Arc, Mutex, OnceLock, Weak, mpsc},
    task::{Poll, Waker},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// Service version the Go SDK sends (azblob v1.8.2).
pub const AZURE_SERVICE_VERSION: &str = "2026-12-06";

/// One list request deadline.
const LIST_TIMEOUT: Duration = Duration::from_secs(10);

/// Leave time for another DNS address if one route silently drops packets.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(1);

/// Bound the address walk and its retained lookup result.
const MAX_ADDRESSES: usize = 16;

/// Largest list page body accepted.
const MAX_PAGE_BYTES: usize = 16 * 1024 * 1024;

/// Largest token or metadata body accepted.
const MAX_SMALL_BODY: usize = 64 * 1024;

/// Refresh tokens this long before they expire.
const REFRESH_LEAD: Duration = Duration::from_secs(300);

/// Bound retained credentials even if caches churn faster than tokens expire.
const MAX_CACHED_TOKENS: usize = 1024;

/// IMDS address.
const IMDS_ADDRESS: &str = "169.254.169.254";

/// Token resource for Azure Storage.
const STORAGE_RESOURCE: &str = "https://storage.azure.com/";

/// Token scope for Azure Storage.
const STORAGE_SCOPE: &str = "https://storage.azure.com/.default";

/// Why a fill could not list its origin.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ListError {
    /// The origin or its credential is transiently unavailable.
    Failed,
    /// The credential cannot list the container, or the service refused it.
    Forbidden,
}

impl ListError {
    /// Condition reason reported in fill status.
    pub fn reason(self) -> &'static str {
        match self {
            Self::Failed => "ListFailed",
            Self::Forbidden => "Forbidden",
        }
    }
}

/// Azure settings the dataplane already holds.
#[derive(Clone, Debug)]
pub struct Settings {
    pub trust_bundle: PathBuf,
    pub authority_host: String,
    pub federated_token_file: Option<PathBuf>,
}

struct CachedToken {
    token: String,
    refresh_at: Instant,
}

/// Credential configuration contains identifiers, not bearer tokens or assertions.
/// Keep it in the key so an in-place cache edit cannot reuse another identity.
#[derive(Clone, Eq, Ord, PartialEq, PartialOrd)]
struct TokenKey {
    cache_uid: String,
    identity: (u8, Option<String>, Option<String>),
}

impl TokenKey {
    fn new(cache_uid: &str, credential: &AzureCredential) -> Self {
        let identity = match credential {
            AzureCredential::Anonymous => (0, None, None),
            AzureCredential::ManagedIdentity { client_id } => (1, client_id.clone(), None),
            AzureCredential::WorkloadIdentity {
                client_id,
                tenant_id,
            } => (2, Some(client_id.clone()), tenant_id.clone()),
        };
        Self {
            cache_uid: cache_uid.to_owned(),
            identity,
        }
    }
}

#[derive(Default)]
struct TokenCache(BTreeMap<TokenKey, CachedToken>);

impl TokenCache {
    fn get(&mut self, key: &TokenKey, now: Instant) -> Option<String> {
        // Expired and replaced identities are discarded on every lookup,
        // including anonymous access after a credential was removed.
        self.0.retain(|stored, token| {
            now < token.refresh_at && (stored.cache_uid != key.cache_uid || stored == key)
        });
        self.0.get(key).map(|cached| cached.token.clone())
    }

    fn insert(&mut self, key: TokenKey, token: String, refresh_at: Instant) {
        if self.0.len() >= MAX_CACHED_TOKENS
            && let Some(oldest) = self
                .0
                .iter()
                .min_by_key(|(_, token)| token.refresh_at)
                .map(|(key, _)| key.clone())
        {
            self.0.remove(&oldest);
        }
        self.0.insert(key, CachedToken { token, refresh_at });
    }

    fn invalidate(&mut self, key: &TokenKey) {
        self.0.remove(key);
    }
}

/// Per-process listing client and token cache. Only the fill listing task
/// uses it, so it is not shared.
pub struct Client {
    settings: Settings,
    trust: Option<Arc<rustls::ClientConfig>>,
    tokens: RefCell<TokenCache>,
}

impl Client {
    /// Load the origin trust bundle once.
    pub async fn new(settings: Settings) -> Self {
        let trust = match rt::read_file(&settings.trust_bundle, 1024 * 1024).await {
            Ok(pem) => match kube::tls_config(&pem) {
                Ok(config) => Some(config),
                Err(_) => {
                    eprintln!(
                        "racer: fill HTTPS listing unavailable: invalid trust bundle {}",
                        settings.trust_bundle.display()
                    );
                    None
                }
            },
            Err(error) => {
                eprintln!(
                    "racer: fill HTTPS listing unavailable: cannot read trust bundle {}: {error}",
                    settings.trust_bundle.display()
                );
                None
            }
        };
        Self {
            settings,
            trust,
            tokens: RefCell::new(TokenCache::default()),
        }
    }

    /// List every blob under any of `prefixes`, capped at one page beyond the
    /// manifest limit so the caller can report `TooManyObjects`.
    pub async fn list(
        &self,
        url: &AzureContainerUrl,
        credential: &AzureCredential,
        cache_uid: &str,
        prefixes: &[String],
    ) -> Result<Vec<ListedObject>, ListError> {
        let key = TokenKey::new(cache_uid, credential);
        let mut bearer = self.token(credential, &key).await?;
        // One refresh per complete listing, not per page or prefix. A second
        // 401 still evicts the rejected token, but cannot loop on bad credentials.
        let mut refreshed = false;
        let mut objects = Vec::new();
        let mut seen = BTreeSet::new();
        for prefix in prefixes {
            let mut marker = String::new();
            loop {
                let (status, body) = self
                    .list_page(url, &marker, prefix, bearer.as_deref())
                    .await?;
                if status == 401 && bearer.is_some() {
                    self.tokens.borrow_mut().invalidate(&key);
                    if !refreshed {
                        refreshed = true;
                        bearer = self.token(credential, &key).await?;
                        continue;
                    }
                }
                if status != 200 {
                    return Err(match status {
                        401 | 403 => ListError::Forbidden,
                        _ => ListError::Failed,
                    });
                }
                let page = parse_list(&body).map_err(|_| ListError::Failed)?;
                if extend_unique(&mut objects, &mut seen, page.blobs) {
                    return Ok(objects);
                }
                marker = page.next_marker;
                if marker.is_empty() {
                    break;
                }
            }
        }
        Ok(objects)
    }

    async fn list_page(
        &self,
        url: &AzureContainerUrl,
        marker: &str,
        prefix: &str,
        bearer: Option<&str>,
    ) -> Result<(u16, Vec<u8>), ListError> {
        let mut target = String::with_capacity(96 + prefix.len() * 3);
        target.push('/');
        if let Some(account) = url.account_path() {
            target.push_str(account);
            target.push('/');
        }
        target.push_str(url.container());
        target.push_str("?restype=container&comp=list&maxresults=5000");
        if !prefix.is_empty() {
            target.push_str("&prefix=");
            target.push_str(&kube::escape(prefix));
        }
        if !marker.is_empty() {
            target.push_str("&marker=");
            target.push_str(&kube::escape(marker));
        }
        let date = http_date(
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map_or(0, |d| d.as_secs()),
        );
        let auth = bearer.map(|t| format!("Bearer {t}"));
        let mut headers = vec![
            ("x-ms-version", AZURE_SERVICE_VERSION),
            ("x-ms-date", date.as_str()),
            ("User-Agent", "racer"),
            ("Accept", "application/xml"),
        ];
        if let Some(auth) = auth.as_deref() {
            headers.push(("Authorization", auth));
        }
        exchange(
            &self.trust,
            url.scheme_is_https(),
            url.host(),
            url.port(),
            &url.host_header(),
            "GET",
            &target,
            &headers,
            &[],
            MAX_PAGE_BYTES,
        )
        .await
    }

    async fn token(
        &self,
        credential: &AzureCredential,
        key: &TokenKey,
    ) -> Result<Option<String>, ListError> {
        let cached = self.tokens.borrow_mut().get(key, rt::now());
        if matches!(credential, AzureCredential::Anonymous) {
            return Ok(None);
        }
        if let Some(cached) = cached {
            return Ok(Some(cached));
        }
        let (token, lifetime) = match credential {
            AzureCredential::Anonymous => unreachable!("checked above"),
            AzureCredential::ManagedIdentity { client_id } => {
                self.imds(client_id.as_deref()).await?
            }
            AzureCredential::WorkloadIdentity {
                client_id,
                tenant_id,
            } => {
                self.workload_identity(client_id, tenant_id.as_deref())
                    .await?
            }
        };
        let lead = REFRESH_LEAD.min(lifetime / 2);
        self.tokens.borrow_mut().insert(
            key.clone(),
            token.clone(),
            rt::now() + lifetime.saturating_sub(lead),
        );
        Ok(Some(token))
    }

    async fn imds(&self, client_id: Option<&str>) -> Result<(String, Duration), ListError> {
        let mut target = format!(
            "/metadata/identity/oauth2/token?api-version=2018-02-01&resource={}",
            kube::escape(STORAGE_RESOURCE)
        );
        if let Some(client_id) = client_id {
            target.push_str("&client_id=");
            target.push_str(&kube::escape(client_id));
        }
        let (status, body) = exchange(
            &None,
            false,
            IMDS_ADDRESS,
            80,
            IMDS_ADDRESS,
            "GET",
            &target,
            &[("Metadata", "true")],
            &[],
            MAX_SMALL_BODY,
        )
        .await?;
        if status == 400 || status == 403 || status == 404 {
            return Err(ListError::Forbidden);
        }
        if status != 200 {
            return Err(ListError::Failed);
        }
        token_response(&body)
    }

    async fn workload_identity(
        &self,
        client_id: &str,
        tenant_id: Option<&str>,
    ) -> Result<(String, Duration), ListError> {
        let Some(tenant) = tenant_id else {
            // Without a tenant there is no exchange endpoint. A storage
            // challenge is not used by the listing path.
            return Err(ListError::Forbidden);
        };
        let assertion = match &self.settings.federated_token_file {
            Some(path) => rt::read_file(path, MAX_SMALL_BODY)
                .await
                .map_err(|_| ListError::Forbidden)?,
            None => return Err(ListError::Forbidden),
        };
        let assertion = String::from_utf8(assertion).map_err(|_| ListError::Forbidden)?;
        let assertion = assertion.trim();
        if assertion.is_empty() {
            return Err(ListError::Forbidden);
        }
        let authority = self.settings.authority_host.trim_end_matches('/');
        let target = format!("/{tenant}/oauth2/v2.0/token");
        let (host, https, port) = split_authority(authority)?;
        let form = format!(
            "client_id={}&scope={}&grant_type=client_credentials&client_assertion_type={}&client_assertion={}",
            kube::escape(client_id),
            kube::escape(STORAGE_SCOPE),
            kube::escape("urn:ietf:params:oauth:client-assertion-type:jwt-bearer"),
            kube::escape(assertion),
        );
        let (status, body) = exchange(
            &self.trust,
            https,
            &host,
            port,
            &host,
            "POST",
            &target,
            &[("Content-Type", "application/x-www-form-urlencoded")],
            form.as_bytes(),
            MAX_SMALL_BODY,
        )
        .await?;
        if status == 400 || status == 401 || status == 403 {
            return Err(ListError::Forbidden);
        }
        if status != 200 {
            return Err(ListError::Failed);
        }
        token_response(&body)
    }
}

fn split_authority(authority: &str) -> Result<(String, bool, u16), ListError> {
    let (scheme, rest) = authority.split_once("://").ok_or(ListError::Failed)?;
    let (host, port) = match rest.split_once(':') {
        Some((host, port)) => (host, port.parse::<u16>().map_err(|_| ListError::Failed)?),
        None => (rest, if scheme == "https" { 443 } else { 80 }),
    };
    if host.is_empty() || !matches!(scheme, "https" | "http") {
        return Err(ListError::Failed);
    }
    Ok((host.to_owned(), scheme == "https", port))
}

fn token_response(body: &[u8]) -> Result<(String, Duration), ListError> {
    let v: Value = serde_json::from_slice(body).map_err(|_| ListError::Failed)?;
    let token = v
        .get("access_token")
        .and_then(Value::as_str)
        .filter(|t| !t.is_empty() && t.len() <= 16 * 1024)
        .ok_or(ListError::Failed)?
        .to_owned();
    let seconds = v
        .get("expires_in")
        .and_then(|e| match e {
            Value::Number(n) => n.as_u64(),
            Value::String(s) => s.parse().ok(),
            _ => None,
        })
        .unwrap_or(3600)
        .max(60);
    Ok((token, Duration::from_secs(seconds)))
}

#[allow(clippy::too_many_arguments)] // Forward endpoint, request, and bounds without a parameter shim.
async fn exchange(
    trust: &Option<Arc<rustls::ClientConfig>>,
    https: bool,
    host: &str,
    port: u16,
    host_header: &str,
    method: &str,
    target: &str,
    headers: &[(&str, &str)],
    body: &[u8],
    limit: usize,
) -> Result<(u16, Vec<u8>), ListError> {
    // Initialization already diagnosed this local configuration failure. Do
    // not spend the request deadline resolving or connecting without roots.
    if https && trust.is_none() {
        return Err(ListError::Failed);
    }
    let request =
        zeroize::Zeroizing::new(conn::request(method, host_header, target, headers, body));
    rt::timeout(LIST_TIMEOUT, async {
        let addresses = resolve(host, port).await?;
        let tcp = connect_addresses(&addresses).await?;
        if https {
            let config = trust.clone().ok_or(ListError::Failed)?;
            let name = ServerName::try_from(host.to_owned()).map_err(|_| ListError::Failed)?;
            let stream = TlsStream::client(tcp, config, name)
                .await
                .map_err(|_| ListError::Failed)?;
            let mut conn = Conn::new(stream);
            conn.tls
                .write_all(&request)
                .await
                .map_err(|_| ListError::Failed)?;
            let head = conn
                .read_head(false, crate::limits::MAX_HEAD_BYTES)
                .await
                .map_err(|_| ListError::Failed)?
                .ok_or(ListError::Failed)?;
            let body = conn
                .read_response_body(&head, limit)
                .await
                .map_err(|_| ListError::Failed)?;
            Ok((head.status, body))
        } else {
            plain_exchange(tcp, &request, limit).await
        }
    })
    .await
    .map_err(|_| ListError::Failed)?
}

/// Add one page's blobs, skipping names already listed under another prefix.
/// Returns true once the unique count exceeds the manifest limit.
fn extend_unique(
    objects: &mut Vec<ListedObject>,
    seen: &mut BTreeSet<String>,
    blobs: Vec<ListedObject>,
) -> bool {
    for blob in blobs {
        if seen.insert(blob.name.clone()) {
            objects.push(blob);
        }
    }
    objects.len() > MAX_FILL_OBJECTS
}

async fn connect_addresses(addresses: &[SocketAddr]) -> Result<Tcp, ListError> {
    for &address in addresses.iter().take(MAX_ADDRESSES) {
        if let Ok(Ok(tcp)) = rt::timeout(CONNECT_TIMEOUT, Tcp::connect(address)).await {
            return Ok(tcp);
        }
    }
    Err(ListError::Failed)
}

#[derive(Default)]
struct ResolveState {
    addresses: Option<Vec<SocketAddr>>,
    waker: Option<Waker>,
}

struct ResolveJob {
    host: String,
    port: u16,
    reply: Weak<Mutex<ResolveState>>,
}

/// libc resolution is blocking and cannot be canceled. Keep one process-wide
/// worker and at most one queued job, even when lookup callers time out. A
/// stuck system resolver therefore cannot accumulate detached OS threads.
fn resolver() -> Result<&'static mpsc::SyncSender<ResolveJob>, ListError> {
    static RESOLVER: OnceLock<Result<mpsc::SyncSender<ResolveJob>, ListError>> = OnceLock::new();
    RESOLVER
        .get_or_init(|| {
            let (sender, _worker) = spawn_resolver(|host, port| {
                (host, port)
                    .to_socket_addrs()
                    .map(|addresses| addresses.take(MAX_ADDRESSES).collect())
                    .unwrap_or_default()
            })?;
            Ok(sender)
        })
        .as_ref()
        .map_err(|error| *error)
}

fn spawn_resolver(
    mut lookup: impl FnMut(&str, u16) -> Vec<SocketAddr> + Send + 'static,
) -> Result<(mpsc::SyncSender<ResolveJob>, std::thread::JoinHandle<()>), ListError> {
    let (sender, receiver) = mpsc::sync_channel::<ResolveJob>(1);
    let worker = std::thread::Builder::new()
        .name("racer-fill-resolve".into())
        .spawn(move || {
            for job in receiver {
                // Dropping a timed-out caller cancels a queued lookup and
                // releases its waker without waiting for libc to return.
                if job.reply.strong_count() == 0 {
                    continue;
                }
                let addresses = lookup(&job.host, job.port);
                if let Some(reply) = job.reply.upgrade() {
                    let waker = {
                        let mut state = lock(&reply);
                        state.addresses = Some(addresses);
                        state.waker.take()
                    };
                    if let Some(waker) = waker {
                        waker.wake();
                    }
                }
            }
        })
        .map_err(|_| ListError::Failed)?;
    Ok((sender, worker))
}

/// Resolve off the control thread so a slow lookup cannot stall the primary
/// lease or other authority work. Literals, including IMDS, never need a worker.
async fn resolve(host: &str, port: u16) -> Result<Vec<SocketAddr>, ListError> {
    if let Ok(ip) = host.parse::<std::net::IpAddr>() {
        return Ok(vec![SocketAddr::new(ip, port)]);
    }
    resolve_with(resolver()?, host, port).await
}

async fn resolve_with(
    sender: &mpsc::SyncSender<ResolveJob>,
    host: &str,
    port: u16,
) -> Result<Vec<SocketAddr>, ListError> {
    let reply = Arc::new(Mutex::new(ResolveState::default()));
    sender
        .try_send(ResolveJob {
            host: host.to_owned(),
            port,
            reply: Arc::downgrade(&reply),
        })
        .map_err(|_| ListError::Failed)?;
    std::future::poll_fn(move |cx| {
        let mut state = lock(&reply);
        match state.addresses.take() {
            Some(addresses) if addresses.is_empty() => Poll::Ready(Err(ListError::Failed)),
            Some(addresses) => Poll::Ready(Ok(addresses)),
            None => {
                state.waker = Some(cx.waker().clone());
                Poll::Pending
            }
        }
    })
    .await
}

fn lock<T>(value: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    value.lock().unwrap_or_else(|poison| poison.into_inner())
}

/// Plain HTTP for IMDS, which is link-local and never TLS.
async fn plain_exchange(
    mut tcp: Tcp,
    request: &[u8],
    limit: usize,
) -> Result<(u16, Vec<u8>), ListError> {
    tcp.send_all(request.to_vec())
        .await
        .map_err(|_| ListError::Failed)?;
    let mut buf = Vec::new();
    let header_end = loop {
        if let Some(i) = find(&buf, b"\r\n\r\n") {
            break i;
        }
        if buf.len() > limit + 8192 {
            return Err(ListError::Failed);
        }
        let mut chunk = Vec::new();
        if tcp
            .recv_into(&mut chunk)
            .await
            .map_err(|_| ListError::Failed)?
            == 0
        {
            return Err(ListError::Failed);
        }
        buf.extend_from_slice(&chunk);
    };
    let head = std::str::from_utf8(&buf[..header_end]).map_err(|_| ListError::Failed)?;
    let status = head
        .lines()
        .next()
        .and_then(|l| l.split(' ').nth(1))
        .and_then(|s| s.parse().ok())
        .ok_or(ListError::Failed)?;
    let mut length = 0usize;
    for line in head.lines().skip(1) {
        if let Some((name, value)) = line.split_once(':')
            && name.eq_ignore_ascii_case("content-length")
        {
            length = value.trim().parse().map_err(|_| ListError::Failed)?;
        }
    }
    if length > limit {
        return Err(ListError::Failed);
    }
    let body_start = header_end + 4;
    while buf.len() < body_start + length {
        let mut chunk = Vec::new();
        if tcp
            .recv_into(&mut chunk)
            .await
            .map_err(|_| ListError::Failed)?
            == 0
        {
            break;
        }
        buf.extend_from_slice(&chunk);
    }
    Ok((
        status,
        buf[body_start..buf.len().min(body_start + length)].to_vec(),
    ))
}

fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

/// One parsed List Blobs page.
#[derive(Debug, Default, Eq, PartialEq)]
pub struct Page {
    pub blobs: Vec<ListedObject>,
    pub next_marker: String,
}

/// Malformed list XML.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct XmlError;

/// Parse `EnumerationResults/Blobs/Blob` pages. Strict about structure and
/// entities; ignores unknown elements.
pub fn parse_list(xml: &[u8]) -> Result<Page, XmlError> {
    let xml = std::str::from_utf8(xml).map_err(|_| XmlError)?;
    let mut page = Page::default();
    let mut scanner = Scanner::new(xml);
    let mut root = false;
    let mut in_blob = false;
    let mut capture = Capture::None;
    let mut name = String::new();
    let mut name_encoded = false;
    let mut etag = String::new();
    let mut length = String::new();
    let mut content_type = String::new();
    while let Some(token) = scanner.next()? {
        match token {
            Token::Start(tag, encoded) => {
                if !root {
                    if tag != "EnumerationResults" {
                        return Err(XmlError);
                    }
                    root = true;
                    continue;
                }
                match tag {
                    "Blob" => {
                        in_blob = true;
                        name.clear();
                        name_encoded = false;
                        etag.clear();
                        length.clear();
                    }
                    "Name" if in_blob => {
                        name_encoded = encoded;
                        capture = Capture::Name;
                    }
                    "Etag" if in_blob => capture = Capture::Etag,
                    "Content-Length" if in_blob => capture = Capture::Length,
                    "Content-Type" if in_blob => capture = Capture::ContentType,
                    "NextMarker" => capture = Capture::Marker,
                    _ => capture = Capture::None,
                }
            }
            Token::End(tag) => match tag {
                "Blob" => {
                    if !in_blob {
                        return Err(XmlError);
                    }
                    let length = length.parse().map_err(|_| XmlError)?;
                    if name_encoded {
                        name = percent_decode(&name)?;
                    }
                    page.blobs.push(ListedObject {
                        name: std::mem::take(&mut name),
                        etag: list_etag(&etag)?,
                        length,
                        content_type: (!content_type.is_empty())
                            .then(|| std::mem::take(&mut content_type)),
                    });
                    in_blob = false;
                }
                "Name" | "Etag" | "Content-Length" | "Content-Type" | "NextMarker" => {
                    capture = Capture::None;
                }
                "EnumerationResults" => root = false,
                _ => {}
            },
            Token::Text(text) => match capture {
                Capture::Name => name.push_str(&text),
                Capture::Etag => etag.push_str(&text),
                Capture::Length => length.push_str(&text),
                Capture::ContentType => content_type.push_str(&text),
                Capture::Marker => page.next_marker.push_str(&text),
                Capture::None => {}
            },
        }
    }
    if xml.trim().is_empty() {
        return Err(XmlError);
    }
    if root {
        return Err(XmlError);
    }
    Ok(page)
}

/// List XML may omit the quotes that GET and HEAD use around Azure ETags.
fn list_etag(value: &str) -> Result<String, XmlError> {
    let quoted = if let Some(hex) = value.strip_prefix("0x") {
        if hex.is_empty() || !hex.bytes().all(|byte| byte.is_ascii_hexdigit()) {
            return Err(XmlError);
        }
        format!("\"{value}\"")
    } else {
        value.to_owned()
    };
    StrongEtag::parse(quoted.as_bytes()).map_err(|_| XmlError)?;
    Ok(quoted)
}

enum Capture {
    None,
    Name,
    Etag,
    Length,
    ContentType,
    Marker,
}

enum Token<'a> {
    Start(&'a str, bool),
    End(&'a str),
    Text(String),
}

struct Scanner<'a> {
    rest: &'a str,
    pending_end: Option<&'a str>,
    open: Vec<&'a str>,
}

impl<'a> Scanner<'a> {
    fn new(xml: &'a str) -> Self {
        Self {
            rest: xml,
            pending_end: None,
            open: Vec::new(),
        }
    }

    fn next(&mut self) -> Result<Option<Token<'a>>, XmlError> {
        if let Some(name) = self.pending_end.take() {
            return Ok(Some(Token::End(name)));
        }
        loop {
            if self.rest.is_empty() {
                if !self.open.is_empty() {
                    return Err(XmlError);
                }
                return Ok(None);
            }
            let Some(at) = self.rest.find('<') else {
                let text = decode(self.rest)?;
                self.rest = "";
                return Ok(Some(Token::Text(text)));
            };
            if at > 0 {
                let text = decode(&self.rest[..at])?;
                self.rest = &self.rest[at..];
                return Ok(Some(Token::Text(text)));
            }
            // Comment contents are not attributes: quotes and '>' are literal.
            if let Some(comment) = self.rest.strip_prefix("<!--") {
                let end = comment.find("-->").ok_or(XmlError)?;
                if comment[..end].contains("--") {
                    return Err(XmlError);
                }
                self.rest = &comment[end + 3..];
                continue;
            }
            let mut quote = None;
            let Some(end) = self.rest.char_indices().find_map(|(at, ch)| {
                match (quote, ch) {
                    (Some(q), ch) if q == ch => quote = None,
                    (None, '\'' | '"') => quote = Some(ch),
                    (None, '>') => return Some(at),
                    _ => {}
                }
                None
            }) else {
                return Err(XmlError);
            };
            let tag = &self.rest[1..end];
            if tag.starts_with('?') || tag.starts_with('!') {
                self.rest = &self.rest[end + 1..];
                continue;
            }
            self.rest = &self.rest[end + 1..];
            if let Some(name) = tag.strip_prefix('/') {
                let name = name.trim();
                if name.is_empty() || self.open.pop() != Some(name) {
                    return Err(XmlError);
                }
                return Ok(Some(Token::End(name)));
            }
            let element = tag.trim().trim_end_matches('/').trim_end();
            let name = element.split_whitespace().next().ok_or(XmlError)?;
            if name.is_empty() {
                return Err(XmlError);
            }
            if tag.trim_end().ends_with('/') {
                self.pending_end = Some(name);
            } else {
                self.open.push(name);
            }
            let encoded = encoded_attribute(&element[name.len()..])?;
            return Ok(Some(Token::Start(name, encoded)));
        }
    }
}

/// Read XML attributes without treating spaces or delimiters inside quotes as markup.
fn encoded_attribute(mut attributes: &str) -> Result<bool, XmlError> {
    let mut encoded = None;
    while !attributes.trim().is_empty() {
        attributes = attributes.trim_start();
        let at = attributes.find('=').ok_or(XmlError)?;
        let name = attributes[..at].trim_end();
        if name.is_empty() || name.contains(char::is_whitespace) {
            return Err(XmlError);
        }
        attributes = attributes[at + 1..].trim_start();
        let quote = attributes.chars().next().ok_or(XmlError)?;
        if !matches!(quote, '\'' | '"') {
            return Err(XmlError);
        }
        attributes = &attributes[1..];
        let end = attributes.find(quote).ok_or(XmlError)?;
        let value = decode(&attributes[..end])?;
        if name == "Encoded" {
            if encoded.is_some() {
                return Err(XmlError);
            }
            encoded = Some(match value.as_str() {
                "true" => true,
                "false" => false,
                _ => return Err(XmlError),
            });
        }
        attributes = &attributes[end + 1..];
        if !attributes.is_empty() && !attributes.starts_with(char::is_whitespace) {
            return Err(XmlError);
        }
    }
    Ok(encoded.unwrap_or(false))
}

/// Azure encodes names, not form data: a literal plus is not a space.
fn percent_decode(text: &str) -> Result<String, XmlError> {
    let mut bytes = text.bytes();
    let mut decoded = Vec::with_capacity(text.len());
    while let Some(byte) = bytes.next() {
        if byte == b'%' {
            let high = char::from(bytes.next().ok_or(XmlError)?)
                .to_digit(16)
                .ok_or(XmlError)?;
            let low = char::from(bytes.next().ok_or(XmlError)?)
                .to_digit(16)
                .ok_or(XmlError)?;
            decoded.push((high * 16 + low) as u8);
        } else {
            decoded.push(byte);
        }
    }
    String::from_utf8(decoded).map_err(|_| XmlError)
}

/// Decode the five XML entities plus numeric character references.
fn decode(text: &str) -> Result<String, XmlError> {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(at) = rest.find('&') {
        out.push_str(&rest[..at]);
        let after = &rest[at + 1..];
        let end = after.find(';').ok_or(XmlError)?;
        let entity = &after[..end];
        match entity {
            "amp" => out.push('&'),
            "lt" => out.push('<'),
            "gt" => out.push('>'),
            "quot" => out.push('"'),
            "apos" => out.push('\''),
            _ => {
                let value = if let Some(hex) = entity.strip_prefix("#x") {
                    u32::from_str_radix(hex, 16).map_err(|_| XmlError)?
                } else if let Some(dec) = entity.strip_prefix('#') {
                    dec.parse().map_err(|_| XmlError)?
                } else {
                    return Err(XmlError);
                };
                out.push(char::from_u32(value).ok_or(XmlError)?);
            }
        }
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    Ok(out)
}

/// Format seconds since the Unix epoch as an RFC 1123 IMF-fixdate.
pub fn http_date(secs: u64) -> String {
    const DAYS: [&str; 7] = ["Thu", "Fri", "Sat", "Sun", "Mon", "Tue", "Wed"];
    const MONTHS: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    let days = secs / 86_400;
    let rem = secs % 86_400;
    let weekday = DAYS[(days % 7) as usize];
    let z = days + 719_468;
    let era = z / 146_097;
    let doe = z % 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + u64::from(month <= 2);
    format!(
        "{weekday}, {day:02} {} {year:04} {:02}:{:02}:{:02} GMT",
        MONTHS[(month - 1) as usize],
        rem / 3600,
        rem / 60 % 60,
        rem % 60
    )
}

#[cfg(test)]
mod resolver_tests {
    use super::*;
    use std::{future::Future, task::Context};

    #[test]
    fn literal_addresses_do_not_require_the_resolver_worker() {
        // Literal resolution needs neither an executor nor a blocking worker.
        assert_eq!(
            futures::executor::block_on(resolve("127.0.0.1", 80)),
            Ok(vec!["127.0.0.1:80".parse().unwrap()])
        );
        assert_eq!(
            futures::executor::block_on(resolve("::1", 443)),
            Ok(vec!["[::1]:443".parse().unwrap()])
        );
    }

    #[test]
    fn blocked_resolver_has_one_worker_one_queue_slot_and_skips_canceled_jobs() {
        let (started_tx, started_rx) = mpsc::channel();
        let (release_tx, release_rx) = mpsc::channel();
        let (sender, worker) = spawn_resolver(move |_, _| {
            started_tx.send(()).unwrap();
            release_rx.recv().unwrap();
            vec!["127.0.0.1:80".parse().unwrap()]
        })
        .unwrap();
        let waker = futures::task::noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut first = Box::pin(resolve_with(&sender, "first", 80));
        assert!(first.as_mut().poll(&mut cx).is_pending());
        started_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        // Simulate cancellation by the surrounding request deadline while
        // libc is still blocked. It must not create another worker.
        drop(first);
        let mut queued = Box::pin(resolve_with(&sender, "queued", 80));
        assert!(queued.as_mut().poll(&mut cx).is_pending());
        let mut overflow = Box::pin(resolve_with(&sender, "overflow", 80));
        assert_eq!(
            overflow.as_mut().poll(&mut cx),
            Poll::Ready(Err(ListError::Failed))
        );
        drop(overflow);
        drop(queued);
        drop(sender);
        release_tx.send(()).unwrap();
        worker.join().unwrap();
        // The canceled queued lookup never reached the blocking resolver.
        assert!(started_rx.try_recv().is_err());
    }

    #[test]
    fn resolver_delivers_every_address_and_reports_lookup_failure() {
        let expected = vec!["[::1]:80".parse().unwrap(), "127.0.0.1:80".parse().unwrap()];
        let addresses = expected.clone();
        let (sender, worker) = spawn_resolver(move |host, _| {
            if host == "missing" {
                Vec::new()
            } else {
                addresses.clone()
            }
        })
        .unwrap();
        assert_eq!(
            futures::executor::block_on(resolve_with(&sender, "test", 80)),
            Ok(expected)
        );
        assert_eq!(
            futures::executor::block_on(resolve_with(&sender, "missing", 80)),
            Err(ListError::Failed)
        );
        drop(sender);
        worker.join().unwrap();
    }

    #[test]
    fn connect_falls_back_after_refusal_and_timeout() {
        use racer_io::{
            ctx::VirtualClock,
            sim::{Fault, SimOs},
        };
        use std::{cell::Cell, rc::Rc};
        for stalled in [false, true] {
            let clock = VirtualClock::new();
            let os = SimOs::new();
            let mut executor = rt::Executor::simulated(os.clone(), &clock, 0, 32).unwrap();
            let done = Rc::new(Cell::new(false));
            let completed = done.clone();
            executor.enter(|| {
                rt::spawn(async move {
                    let good: SocketAddr = "127.0.0.1:10001".parse().unwrap();
                    let _listener = rt::Listener::bind(good).unwrap();
                    if stalled {
                        os.inject("connect", Fault::Delay(usize::MAX)).unwrap();
                    }
                    let start = rt::now();
                    assert!(
                        connect_addresses(&["[::1]:10002".parse().unwrap(), good])
                            .await
                            .is_ok()
                    );
                    if stalled {
                        assert!(rt::now() >= start + CONNECT_TIMEOUT);
                    }
                    assert!(connect_addresses(&[]).await.is_err());
                    assert!(
                        connect_addresses(&["127.0.0.1:10003".parse().unwrap()])
                            .await
                            .is_err()
                    );
                    completed.set(true);
                })
                .unwrap()
            });
            // A deliberately stalled simulated operation remains runnable;
            // drive virtual time explicitly instead of waiting for quiescence.
            for _ in 0..100 {
                executor.turn().unwrap();
                if done.get() {
                    break;
                }
                clock.advance(Duration::from_millis(100)).unwrap();
            }
            assert!(done.get(), "address failover did not complete");
        }
    }
}

#[cfg(test)]
mod auth_tests {
    use super::*;
    use std::{collections::VecDeque, rc::Rc};

    fn run(body: impl std::future::Future<Output = ()> + 'static) {
        let mut executor = rt::Executor::new(32).unwrap();
        let done = Rc::new(std::cell::Cell::new(false));
        let completed = done.clone();
        executor.enter(|| {
            rt::spawn(async move {
                body.await;
                completed.set(true);
            })
            .unwrap()
        });
        executor.run(|| done.get());
    }

    fn credential(client: &str, tenant: &str) -> AzureCredential {
        AzureCredential::WorkloadIdentity {
            client_id: client.into(),
            tenant_id: Some(tenant.into()),
        }
    }

    #[test]
    fn missing_trust_fails_https_before_dns_or_network_io() {
        // No reactor is installed: reaching DNS, sockets, or a request timer
        // would panic instead of returning the configuration failure.
        assert_eq!(
            futures::executor::block_on(exchange(
                &None,
                true,
                "unused.invalid",
                443,
                "unused.invalid",
                "GET",
                "/",
                &[],
                &[],
                MAX_SMALL_BODY,
            )),
            Err(ListError::Failed)
        );
    }

    #[test]
    fn missing_and_invalid_trust_bundles_leave_https_disabled() {
        run(async {
            for path in [
                concat!(env!("CARGO_MANIFEST_DIR"), "/Cargo.toml/missing"),
                concat!(env!("CARGO_MANIFEST_DIR"), "/Cargo.toml"),
            ] {
                let client = Client::new(Settings {
                    trust_bundle: path.into(),
                    authority_host: String::new(),
                    federated_token_file: None,
                })
                .await;
                assert!(client.trust.is_none());
            }
        });
    }

    #[test]
    fn token_cache_reuses_identity_and_discards_replaced_and_expired_entries() {
        let now = Instant::now();
        let mut cache = TokenCache::default();
        let key = TokenKey::new("cache", &credential("client", "tenant"));
        cache.insert(key.clone(), "token".into(), now + REFRESH_LEAD);
        assert_eq!(cache.get(&key, now).as_deref(), Some("token"));
        assert_eq!(cache.get(&key, now).as_deref(), Some("token"));
        for changed in [
            credential("other", "tenant"),
            credential("client", "other"),
            AzureCredential::ManagedIdentity {
                client_id: Some("client".into()),
            },
            AzureCredential::Anonymous,
        ] {
            cache.insert(key.clone(), "token".into(), now + REFRESH_LEAD);
            assert!(cache.get(&TokenKey::new("cache", &changed), now).is_none());
            assert!(cache.0.is_empty());
        }
        cache.insert(key.clone(), "token".into(), now);
        assert!(cache.get(&key, now).is_none());
        assert!(cache.0.is_empty());
    }

    #[test]
    fn token_cache_is_bounded_under_cache_churn() {
        let now = Instant::now();
        let mut cache = TokenCache::default();
        for i in 0..MAX_CACHED_TOKENS + 100 {
            let key = TokenKey::new(&format!("cache-{i}"), &credential("client", "tenant"));
            cache.get(&key, now);
            cache.insert(key, "token".into(), now + Duration::from_secs(i as u64 + 1));
            assert!(cache.0.len() <= MAX_CACHED_TOKENS);
        }
        assert!(
            !cache
                .0
                .contains_key(&TokenKey::new("cache-0", &credential("client", "tenant")))
        );
        let key = TokenKey::new("unused", &AzureCredential::Anonymous);
        cache.get(&key, now + Duration::from_secs(10_000));
        assert!(cache.0.is_empty());
    }

    /// Loopback token and list endpoints. The manifest is empty so these tests
    /// exercise authentication and requests, not XML parsing.
    fn endpoint(statuses: &[u16]) -> (Client, AzureContainerUrl, Rc<RefCell<Vec<String>>>) {
        let listener = rt::Listener::bind("127.0.0.1:0".parse().unwrap()).unwrap();
        let address = listener.local_addr().unwrap();
        let requests = Rc::new(RefCell::new(Vec::new()));
        let recorded = requests.clone();
        let mut statuses = VecDeque::from(statuses.to_vec());
        let mut tokens = 0;
        rt::spawn(async move {
            loop {
                let (mut tcp, _) = listener.accept().await.unwrap();
                let mut request = Vec::new();
                loop {
                    assert_ne!(tcp.recv_into(&mut request).await.unwrap(), 0);
                    if let Some(end) = find(&request, b"\r\n\r\n") {
                        let head = std::str::from_utf8(&request[..end]).unwrap();
                        let length = head.lines().find_map(|line| {
                            let (name, value) = line.split_once(':')?;
                            name.eq_ignore_ascii_case("content-length")
                                .then(|| value.trim().parse::<usize>().unwrap())
                        }).unwrap_or(0);
                        if request.len() >= end + 4 + length { break; }
                    }
                }
                let request = String::from_utf8(request).unwrap();
                let (status, body) = if request.starts_with("POST ") {
                    tokens += 1;
                    (200, format!(r#"{{"access_token":"token-{tokens}","expires_in":3600}}"#))
                } else {
                    (statuses.pop_front().unwrap_or(200),
                        "<EnumerationResults><Blobs></Blobs></EnumerationResults>".into())
                };
                recorded.borrow_mut().push(request);
                tcp.send_all(format!("HTTP/1.1 {status} Response\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).into_bytes()).await.unwrap();
            }
        }).unwrap();
        let client = Client {
            settings: Settings {
                trust_bundle: PathBuf::new(),
                authority_host: format!("http://{address}"),
                // A nonempty local fixture is enough for the fake token endpoint.
                federated_token_file: Some(PathBuf::from(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/Cargo.toml"
                ))),
            },
            trust: None,
            tokens: RefCell::new(TokenCache::default()),
        };
        let url = AzureContainerUrl::parse(&format!("http://{address}/account/models")).unwrap();
        (client, url, requests)
    }

    #[test]
    fn fills_share_tokens_but_credential_edits_exchange_again() {
        run(async {
            let (client, url, requests) = endpoint(&[]);
            for cred in [
                credential("client", "tenant"),
                credential("client", "tenant"),
                credential("other", "tenant"),
                credential("other", "other"),
            ] {
                assert!(
                    client
                        .list(&url, &cred, "cache", &["".into()])
                        .await
                        .is_ok()
                );
            }
            let requests = requests.borrow();
            assert_eq!(
                requests.iter().filter(|r| r.starts_with("POST ")).count(),
                3
            );
            assert_eq!(requests.iter().filter(|r| r.starts_with("GET ")).count(), 4);
            assert_eq!(client.tokens.borrow().0.len(), 1);
        });
    }

    #[test]
    fn unauthorized_refreshes_once_and_forbidden_does_not_retry() {
        for (statuses, expected, exchanges, retained) in [
            (vec![401, 200], Ok(()), 2, true),
            (vec![401, 401], Err(ListError::Forbidden), 2, false),
            (vec![403], Err(ListError::Forbidden), 1, true),
        ] {
            run(async move {
                let (client, url, requests) = endpoint(&statuses);
                assert_eq!(
                    client
                        .list(&url, &credential("client", "tenant"), "cache", &["".into()])
                        .await
                        .map(|_| ()),
                    expected
                );
                let requests = requests.borrow();
                assert_eq!(
                    requests.iter().filter(|r| r.starts_with("POST ")).count(),
                    exchanges
                );
                assert_eq!(
                    requests.iter().filter(|r| r.starts_with("GET ")).count(),
                    statuses.len()
                );
                assert_eq!(!client.tokens.borrow().0.is_empty(), retained);
                if statuses[0] == 401 {
                    let gets: Vec<_> = requests.iter().filter(|r| r.starts_with("GET ")).collect();
                    assert!(gets[0].contains("Bearer token-1"));
                    assert!(gets[1].contains("Bearer token-2"));
                }
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        io::{Read, Write},
        net::TcpListener,
        thread::{self, JoinHandle},
    };

    fn mock_list_server(
        responses: Vec<(u16, String)>,
    ) -> (AzureContainerUrl, JoinHandle<Vec<String>>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let url = AzureContainerUrl::parse(&format!(
            "http://{}/devstoreaccount1/models",
            listener.local_addr().unwrap()
        ))
        .unwrap();
        let server = thread::spawn(move || {
            let mut requests = Vec::new();
            for (status, body) in responses {
                let deadline = Instant::now() + Duration::from_secs(5);
                let mut stream = loop {
                    match listener.accept() {
                        Ok((stream, _)) => break stream,
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            assert!(Instant::now() < deadline, "missing list request");
                            thread::sleep(Duration::from_millis(1));
                        }
                        Err(error) => panic!("accept: {error}"),
                    }
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                stream
                    .set_write_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let mut request = Vec::new();
                while find(&request, b"\r\n\r\n").is_none() {
                    let mut buf = [0; 1024];
                    let n = stream.read(&mut buf).unwrap();
                    assert!(n > 0, "incomplete request");
                    request.extend_from_slice(&buf[..n]);
                    assert!(request.len() < 64 * 1024);
                }
                requests.push(String::from_utf8(request).unwrap());
                write!(stream, "HTTP/1.1 {status} Mock\r\nContent-Length: {}\r\nContent-Type: application/xml\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            }
            requests
        });
        (url, server)
    }

    fn list_from_mock(
        url: &AzureContainerUrl,
        prefixes: &[String],
    ) -> Result<Vec<ListedObject>, ListError> {
        let client = Client {
            settings: Settings {
                trust_bundle: PathBuf::new(),
                authority_host: String::new(),
                federated_token_file: None,
            },
            trust: None,
            tokens: RefCell::new(TokenCache::default()),
        };
        // Spawn the operation as production does. The task waker notifies the
        // reactor when the DNS helper thread completes; block_on's test-only
        // flag waker does not interrupt a parked io_uring wait.
        let mut executor = rt::Executor::new(32).unwrap();
        let result = std::rc::Rc::new(RefCell::new(None));
        let completed = result.clone();
        let url = url.clone();
        let prefixes = prefixes.to_vec();
        executor.enter(|| {
            rt::spawn(async move {
                *completed.borrow_mut() = Some(
                    client
                        .list(&url, &AzureCredential::Anonymous, "test", &prefixes)
                        .await,
                );
            })
            .unwrap();
        });
        executor.run(|| result.borrow().is_some());
        result.borrow_mut().take().unwrap()
    }

    #[test]
    fn client_lists_all_pages_and_prefixes_with_opaque_markers() {
        let last = PAGE.replace("<NextMarker>next+/=</NextMarker>", "<NextMarker />\n");
        let (url, server) = mock_list_server(vec![
            // Azure may return no blobs and still require another request.
            (200, "<EnumerationResults><Blobs /><NextMarker>next+/=&amp;</NextMarker></EnumerationResults>".into()),
            (200, last.clone()),
            (200, last),
        ]);
        let objects = list_from_mock(&url, &["a/ +&".into(), "a/".into()]).unwrap();
        assert_eq!(objects, parse_list(PAGE.as_bytes()).unwrap().blobs);
        let requests = server.join().unwrap();
        assert_eq!(requests.len(), 3);
        let base = "GET /devstoreaccount1/models?restype=container&comp=list&maxresults=5000";
        assert!(requests[0].starts_with(&format!("{base}&prefix=a%2F%20%2B%26 HTTP/1.1\r\n")));
        assert!(requests[1].starts_with(&format!(
            "{base}&prefix=a%2F%20%2B%26&marker=next%2B%2F%3D%26 HTTP/1.1\r\n"
        )));
        assert!(requests[2].starts_with(&format!("{base}&prefix=a%2F HTTP/1.1\r\n")));
        for request in requests {
            let request = request.to_ascii_lowercase();
            assert!(request.contains(&format!("\r\nx-ms-version: {AZURE_SERVICE_VERSION}\r\n")));
            assert!(request.contains("\r\naccept: application/xml\r\n"));
            assert!(request.contains(&format!("\r\nhost: {}\r\n", url.host_header())));
            assert!(!request.contains("\r\nauthorization:"));
        }
    }

    #[test]
    fn client_maps_http_errors_and_discards_partial_listings() {
        for (status, expected) in [
            (401, ListError::Forbidden),
            (403, ListError::Forbidden),
            (404, ListError::Failed),
            (429, ListError::Failed),
            (500, ListError::Failed),
            (503, ListError::Failed),
        ] {
            let (url, server) = mock_list_server(vec![
                (200, PAGE.into()),
                (status, "<Error><Code>ServiceError</Code></Error>".into()),
            ]);
            assert_eq!(
                list_from_mock(&url, &[String::new()]),
                Err(expected),
                "{status}"
            );
            let requests = server.join().unwrap();
            assert_eq!(requests.len(), 2);
            assert!(!requests[0].contains("&prefix="));
            assert!(requests[1].contains("&marker=next%2B%2F%3D "));
        }
    }

    #[test]
    fn client_rejects_bad_xml_without_returning_partial_results() {
        for body in [
            "<EnumerationResults><Blobs>",
            "<EnumerationResults><Blobs><Blob><Name Encoded='true'>bad%GG</Name><Etag>0x12</Etag><Content-Length>1</Content-Length></Blob></Blobs></EnumerationResults>",
            "<EnumerationResults><Blobs><Blob><Name>bad</Name><Etag>W/\"0x12\"</Etag><Content-Length>1</Content-Length></Blob></Blobs></EnumerationResults>",
        ] {
            let (url, server) = mock_list_server(vec![(200, PAGE.into()), (200, body.into())]);
            assert_eq!(
                list_from_mock(&url, &[String::new()]),
                Err(ListError::Failed)
            );
            assert_eq!(server.join().unwrap().len(), 2);
        }
    }

    const PAGE: &str = r#"<?xml version="1.0" encoding="utf-8"?>
<EnumerationResults ServiceEndpoint="https://a.blob.core.windows.net/" ContainerName="models">
  <Blobs>
    <Blob>
      <Name>a/one &amp; two.txt</Name>
      <Properties>
        <Content-Length>12</Content-Length>
        <Etag>&quot;0x8D1&quot;</Etag>
        <Content-Type>text/plain</Content-Type>
      </Properties>
    </Blob>
    <Blob>
      <Name>a/&#x96EA;&#38632;.bin</Name>
      <Properties><Content-Length>0</Content-Length><Etag>&quot;0x8D2&quot;</Etag></Properties>
    </Blob>
  </Blobs>
  <NextMarker>next+/=</NextMarker>
</EnumerationResults>"#;

    #[test]
    fn extend_unique_skips_duplicates_and_caps_unique_names() {
        let blob = |name: &str| ListedObject {
            name: name.to_owned(),
            etag: "\"e\"".into(),
            length: 1,
            content_type: None,
        };
        let mut objects = Vec::new();
        let mut seen = BTreeSet::new();
        assert!(!extend_unique(
            &mut objects,
            &mut seen,
            vec![blob("a/one"), blob("a/one"), blob("a/two")]
        ));
        assert_eq!(
            objects.iter().map(|o| o.name.as_str()).collect::<Vec<_>>(),
            vec!["a/one", "a/two"]
        );

        let mut objects = Vec::new();
        let mut seen = BTreeSet::new();
        let many = (0..=MAX_FILL_OBJECTS)
            .map(|i| blob(&format!("a/{i}")))
            .collect();
        assert!(extend_unique(&mut objects, &mut seen, many));
        assert_eq!(objects.len(), MAX_FILL_OBJECTS + 1);
    }

    #[test]
    fn parses_names_entities_and_next_marker() {
        let page = parse_list(PAGE.as_bytes()).unwrap();
        assert_eq!(
            page.blobs,
            vec![
                ListedObject {
                    name: "a/one & two.txt".into(),
                    etag: "\"0x8D1\"".into(),
                    length: 12,
                    content_type: Some("text/plain".into()),
                },
                ListedObject {
                    name: "a/雪雨.bin".into(),
                    etag: "\"0x8D2\"".into(),
                    length: 0,
                    content_type: None,
                },
            ]
        );
        assert_eq!(page.next_marker, "next+/=");
        let empty = parse_list(
            br#"<EnumerationResults><Blobs></Blobs><NextMarker /></EnumerationResults>"#,
        )
        .unwrap();
        assert!(empty.blobs.is_empty());
        assert!(empty.next_marker.is_empty());
        assert_eq!(
            parse_list(
                br#"<EnumerationResults><NextMarker>a&amp;b</NextMarker></EnumerationResults>"#
            )
            .unwrap()
            .next_marker,
            "a&b"
        );
    }

    #[test]
    fn normalizes_azure_list_etags_to_get_and_head_format() {
        // Microsoft's Enumerating Blob Resources XML examples use this form:
        // https://learn.microsoft.com/rest/api/storageservices/enumerating-blob-resources
        let xml = br#"<EnumerationResults><Blobs><Blob>
            <Name>blob1.txt</Name><Properties>
            <Etag>0x8CBFF45D8A29A19</Etag><Content-Length>100</Content-Length>
            <Content-Type>text/html</Content-Type></Properties>
            </Blob></Blobs><NextMarker /></EnumerationResults>"#;
        let page = parse_list(xml).unwrap();
        assert_eq!(page.blobs[0].etag, "\"0x8CBFF45D8A29A19\"");
        let manifest =
            super::super::manifest::build("models", &page.blobs, &[String::new()], 0).unwrap();
        assert_eq!(manifest.objects[0].etag.as_str(), page.blobs[0].etag);
        assert_eq!(list_etag("\"0x8D1\"").unwrap(), "\"0x8D1\"");
        assert_eq!(list_etag("\"literal,\\tag\"").unwrap(), "\"literal,\\tag\"");
        for bad in [
            "",
            "0x",
            "0xZZ",
            "0x12\r\n",
            "W/\"0x12\"",
            "0x12\"",
            "\"0x12",
            "unquoted",
        ] {
            assert_eq!(list_etag(bad), Err(XmlError), "{bad:?}");
            let xml = String::from_utf8(xml.to_vec())
                .unwrap()
                .replace("0x8CBFF45D8A29A19", bad);
            assert_eq!(parse_list(xml.as_bytes()), Err(XmlError), "{bad:?}");
        }
    }

    #[test]
    fn self_closing_elements_end_capture_before_following_whitespace() {
        let xml = br#"<EnumerationResults><Blobs><Blob>
            <Name>one</Name><Properties><Etag>0x12</Etag>
            <Content-Length>1</Content-Length><Content-Type />
            </Properties></Blob></Blobs><NextMarker />
            </EnumerationResults>"#;
        let page = parse_list(xml).unwrap();
        assert_eq!(page.blobs.len(), 1);
        assert_eq!(page.blobs[0].content_type, None);
        assert_eq!(page.next_marker, "");
        for xml in [
            "<EnumerationResults />",
            "<EnumerationResults><Blobs/>\n<NextMarker/>\n</EnumerationResults>",
        ] {
            assert_eq!(parse_list(xml.as_bytes()).unwrap(), Page::default());
        }
        for xml in [
            "<EnumerationResults><Blobs><Blob /></Blobs></EnumerationResults>",
            "<EnumerationResults><Blobs /></Blobs></EnumerationResults>",
            "<EnumerationResults><Blobs></EnumerationResults>",
            "<EnumerationResults><Blobs/>",
        ] {
            assert_eq!(parse_list(xml.as_bytes()), Err(XmlError), "{xml}");
        }
        for field in ["Etag", "Content-Length"] {
            let xml = String::from_utf8(xml.to_vec()).unwrap().replace(
                if field == "Etag" {
                    "<Etag>0x12</Etag>"
                } else {
                    "<Content-Length>1</Content-Length>"
                },
                &format!("<{field} />"),
            );
            assert_eq!(parse_list(xml.as_bytes()), Err(XmlError), "{field}");
        }
    }

    #[test]
    fn decodes_only_names_marked_encoded_once() {
        // List Blobs percent-encodes XML-invalid names starting with 2021-02-12.
        // https://learn.microsoft.com/rest/api/storageservices/list-blobs
        let blob = |attributes: &str, name: &str| {
            format!(
                "<Blob><Name{attributes}>{name}</Name><Properties><Etag>0x12</Etag><Content-Length>1</Content-Length></Properties></Blob>"
            )
        };
        let xml = format!(
            "<EnumerationResults><Blobs>{}{}{}{}{}</Blobs><NextMarker>%2F</NextMarker></EnumerationResults>",
            blob(" Encoded=\"true\"", "a%2fb%25EF+%EF%BF%BE&amp;%E9%9B%AA"),
            blob("", "literal%2F+"),
            blob(" Encoded='false'", "false%2F+"),
            blob(" Other='space > value' Encoded = 'true'", "%252F"),
            blob("", "reset%2F"),
        );
        let page = parse_list(xml.as_bytes()).unwrap();
        assert_eq!(
            page.blobs
                .iter()
                .map(|b| b.name.as_str())
                .collect::<Vec<_>>(),
            [
                "a/b%EF+\u{fffe}&雪",
                "literal%2F+",
                "false%2F+",
                "%2F",
                "reset%2F"
            ]
        );
        assert_eq!(page.next_marker, "%2F");
        for bad in ["%", "%2", "%GG", "%FF", "%C0%AF", "%ED%A0%80"] {
            let xml = format!(
                "<EnumerationResults><Blobs>{}{}</Blobs></EnumerationResults>",
                blob("", "good"),
                blob(" Encoded='true'", bad)
            );
            assert_eq!(parse_list(xml.as_bytes()), Err(XmlError), "{bad}");
        }
        for attributes in [
            " Encoded='yes'",
            " Encoded=true",
            " Encoded='true' Encoded='false'",
            " Encoded='true'Other='x'",
        ] {
            let xml = format!(
                "<EnumerationResults><Blobs>{}</Blobs></EnumerationResults>",
                blob(attributes, "x")
            );
            assert_eq!(parse_list(xml.as_bytes()), Err(XmlError), "{attributes}");
        }
    }

    #[test]
    fn xml_comments_do_not_parse_quotes_or_greater_than_as_markup() {
        let xml = PAGE.replace("<Blobs>", "<!-- don't parse > or \" here -->\n<Blobs>");
        assert_eq!(parse_list(xml.as_bytes()), parse_list(PAGE.as_bytes()));
        for comment in ["<!-- unfinished", "<!-- bad -- comment -->"] {
            let xml = PAGE.replace("<Blobs>", &format!("{comment}<Blobs>"));
            assert_eq!(parse_list(xml.as_bytes()), Err(XmlError));
        }
    }

    #[test]
    fn rejects_malformed_xml() {
        for xml in [
            &b""[..],
            b"<Blobs></Blobs>",
            b"<EnumerationResults><Blob><Name>x</Name></Blob></EnumerationResults>",
            b"<EnumerationResults><Blobs><Blob><Name>x</Name></Blob></Blobs>",
            b"<EnumerationResults><Blobs><Blob><Name>a&bogus;</Name><Etag>\"e\"</Etag><Content-Length>1</Content-Length></Blob></Blobs></EnumerationResults>",
            b"<EnumerationResults><Blobs><Blob><Name>x</Name><Etag>\"e\"</Etag><Content-Length>x</Content-Length></Blob></Blobs></EnumerationResults>",
        ] {
            assert!(parse_list(xml).is_err(), "{:?}", String::from_utf8_lossy(xml));
        }
    }

    #[test]
    fn decodes_every_entity_and_rejects_bad_references() {
        assert_eq!(
            decode("&amp;&lt;&gt;&quot;&apos;&#65;&#x42;").unwrap(),
            "&<>\"'AB"
        );
        for bad in ["&", "&;", "&nope;", "&#xZZ;", "&#1114112;"] {
            assert_eq!(decode(bad), Err(XmlError), "{bad}");
        }
    }

    #[test]
    fn http_date_matches_go() {
        assert_eq!(http_date(0), "Thu, 01 Jan 1970 00:00:00 GMT");
        assert_eq!(http_date(1_793_738_843), "Tue, 03 Nov 2026 20:47:23 GMT");
    }
}
