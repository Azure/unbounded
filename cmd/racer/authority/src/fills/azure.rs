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
    sync::{Arc, Mutex},
    task::{Poll, Waker},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// Service version the Go SDK sends (azblob v1.8.2).
pub const AZURE_SERVICE_VERSION: &str = "2026-12-06";

/// One list request deadline.
const LIST_TIMEOUT: Duration = Duration::from_secs(10);

/// Largest list page body accepted.
const MAX_PAGE_BYTES: usize = 16 * 1024 * 1024;

/// Largest token or metadata body accepted.
const MAX_SMALL_BODY: usize = 64 * 1024;

/// Refresh tokens this long before they expire.
const REFRESH_LEAD: Duration = Duration::from_secs(300);

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

/// Per-process listing client and token cache. Only the fill listing task
/// uses it, so it is not shared.
pub struct Client {
    settings: Settings,
    trust: Option<Arc<rustls::ClientConfig>>,
    tokens: RefCell<BTreeMap<String, CachedToken>>,
}

impl Client {
    /// Load the origin trust bundle once.
    pub async fn new(settings: Settings) -> Self {
        let trust = rt::read_file(&settings.trust_bundle, 1024 * 1024)
            .await
            .ok()
            .and_then(|pem| kube::tls_config(&pem).ok());
        Self {
            settings,
            trust,
            tokens: RefCell::new(BTreeMap::new()),
        }
    }

    /// List every blob under any of `prefixes`, capped at one page beyond the
    /// manifest limit so the caller can report `TooManyObjects`.
    pub async fn list(
        &self,
        url: &AzureContainerUrl,
        credential: &AzureCredential,
        key: &str,
        prefixes: &[String],
    ) -> Result<Vec<ListedObject>, ListError> {
        let bearer = self.token(credential, key).await?;
        let mut objects = Vec::new();
        let mut seen = BTreeSet::new();
        for prefix in prefixes {
            let mut marker = String::new();
            loop {
                let (status, body) = self
                    .list_page(url, &marker, prefix, bearer.as_deref())
                    .await?;
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
        key: &str,
    ) -> Result<Option<String>, ListError> {
        if matches!(credential, AzureCredential::Anonymous) {
            return Ok(None);
        }
        if let Some(cached) = self.tokens.borrow().get(key)
            && rt::now() < cached.refresh_at
        {
            return Ok(Some(cached.token.clone()));
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
            key.to_owned(),
            CachedToken {
                token: token.clone(),
                refresh_at: rt::now() + lifetime.saturating_sub(lead),
            },
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
    let request =
        zeroize::Zeroizing::new(conn::request(method, host_header, target, headers, body));
    rt::timeout(LIST_TIMEOUT, async {
        let address = resolve(host, port).await?;
        let tcp = Tcp::connect(address).await.map_err(|_| ListError::Failed)?;
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

/// Resolve off the control thread so a slow lookup cannot stall the primary
/// lease or other authority work.
async fn resolve(host: &str, port: u16) -> Result<SocketAddr, ListError> {
    let host = host.to_owned();
    let result = Arc::new(Mutex::new(None));
    let waker = Arc::new(Mutex::new(None::<Waker>));
    {
        let result = Arc::clone(&result);
        let waker = Arc::clone(&waker);
        std::thread::Builder::new()
            .name("racer-fill-resolve".into())
            .spawn(move || {
                let address = (host.as_str(), port)
                    .to_socket_addrs()
                    .ok()
                    .and_then(|mut addresses| addresses.next());
                *lock(&result) = Some(address);
                if let Some(waker) = lock(&waker).take() {
                    waker.wake();
                }
            })
            .map_err(|_| ListError::Failed)?;
    }
    std::future::poll_fn(move |cx| {
        if let Some(address) = *lock(&result) {
            return Poll::Ready(address.ok_or(ListError::Failed));
        }
        *lock(&waker) = Some(cx.waker().clone());
        // Re-check after publishing the waker so a completion cannot be lost.
        if let Some(address) = *lock(&result) {
            return Poll::Ready(address.ok_or(ListError::Failed));
        }
        Poll::Pending
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
            if let Some(comment) = tag.strip_prefix("!--") {
                if !comment.contains("--") {
                    return Err(XmlError);
                }
                self.rest = &self.rest[end + 1..];
                continue;
            }
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
mod tests {
    use super::*;

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
