//! Native object origin definitions carried in cache publications.
//!
//! `AzureContainerUrl` is a port of the Go `ParseAzureContainerURL` and
//! `ValidateAzureAccountURL` checks in `internal/racerobject`. The credential
//! check is split out: `OriginDefinition::validate` requires HTTPS for every
//! credential other than `Anonymous`.

use crate::control::{Error, Result};
use std::time::Duration;

/// Default for `OriginDefinition::metadata_ttl`.
pub const DEFAULT_ORIGIN_METADATA_TTL: Duration = Duration::from_secs(30);

/// Default for `OriginDefinition::request_timeout`.
pub const DEFAULT_ORIGIN_REQUEST_TIMEOUT: Duration = Duration::from_secs(60);

/// Default for `OriginDefinition::max_concurrent_requests`.
pub const DEFAULT_ORIGIN_MAX_CONCURRENT_REQUESTS: u32 = 512;

/// Highest accepted `OriginDefinition::max_concurrent_requests`.
pub const MAX_ORIGIN_CONCURRENT_REQUESTS: u32 = 65536;

/// Longest accepted container URL, matching the ClusterCache CRD bound.
pub const MAX_CONTAINER_URL_BYTES: usize = 320;

/// Object origin the dataplane reads directly for one cache.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct OriginDefinition {
    pub azure: AzureOriginDefinition,

    /// How long object metadata is reused before revalidation. Millisecond precision.
    pub metadata_ttl: Duration,

    /// Bound on each upstream request. Positive, millisecond precision.
    pub request_timeout: Duration,

    /// Concurrent upstream requests per node, 1 through `MAX_ORIGIN_CONCURRENT_REQUESTS`.
    pub max_concurrent_requests: u32,
}

/// One Azure Blob Storage container and how to authenticate to it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AzureOriginDefinition {
    pub container_url: AzureContainerUrl,

    pub credential: AzureCredential,
}

/// How the dataplane authenticates to Azure Blob Storage.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AzureCredential {
    /// Unauthenticated requests. The only credential allowed over plain HTTP.
    Anonymous,

    /// Instance metadata managed identity, optionally user-assigned by client ID.
    ManagedIdentity { client_id: Option<String> },

    /// Microsoft Entra Workload ID federated token exchange.
    WorkloadIdentity {
        client_id: String,
        tenant_id: Option<String>,
    },
}

/// Validated Azure Blob container URL such as
/// `https://account.blob.core.windows.net/models`, or an Azurite-style URL with
/// one account path segment such as `http://127.0.0.1:10000/account/models`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AzureContainerUrl {
    original: String,
    https: bool,
    host: String,
    ipv6: bool,
    port: u16,
    explicit_port: bool,
    account_path: Option<String>,
    container: String,
}

impl AzureContainerUrl {
    /// Parse and validate a container URL. Errors carry no input, so URLs that
    /// embed secrets are never echoed.
    pub fn parse(value: &str) -> Result<Self> {
        let invalid = Err(Error::InvalidRequest);
        if value.len() > MAX_CONTAINER_URL_BYTES || value.contains(['?', '#']) {
            return invalid;
        }
        let Some((scheme, rest)) = value.split_once("://") else {
            return invalid;
        };
        let https = match scheme {
            "https" => true,
            "http" => false,
            _ => return invalid,
        };
        let Some(slash) = rest.rfind('/') else {
            return invalid;
        };
        if slash < 1 || rest.as_bytes()[slash - 1] == b'/' {
            return invalid;
        }
        let (account, container) = (&rest[..slash], &rest[slash + 1..]);
        if !valid_container_name(container) {
            return invalid;
        }
        let account = parse_account(account)?;
        let port = account.port.unwrap_or(if https { 443 } else { 80 });
        Ok(Self {
            original: value.into(),
            https,
            host: account.host.into(),
            ipv6: account.ipv6,
            port,
            explicit_port: account.port.is_some(),
            account_path: account.segment.map(Into::into),
            container: container.into(),
        })
    }

    /// Whether the scheme is `https`.
    pub fn scheme_is_https(&self) -> bool {
        self.https
    }

    /// Host to connect to: a DNS name, an IPv4 address, or an IPv6 address
    /// without brackets.
    pub fn host(&self) -> &str {
        &self.host
    }

    /// Explicit port, or 443 for HTTPS and 80 for HTTP.
    pub fn port(&self) -> u16 {
        self.port
    }

    /// HTTP `Host` header value: IPv6 hosts in brackets, with `:port` when the
    /// URL spelled a port that is not the scheme default.
    pub fn host_header(&self) -> String {
        let host = if self.ipv6 {
            format!("[{}]", self.host)
        } else {
            self.host.clone()
        };
        let default = if self.https { 443 } else { 80 };
        if self.explicit_port && self.port != default {
            format!("{host}:{}", self.port)
        } else {
            host
        }
    }

    /// Azurite-style account path segment without slashes, if any.
    pub fn account_path(&self) -> Option<&str> {
        self.account_path.as_deref()
    }

    /// Container name, the last path segment.
    pub fn container(&self) -> &str {
        &self.container
    }

    /// The URL exactly as configured.
    pub fn as_str(&self) -> &str {
        &self.original
    }
}

/// Azure container naming rules: 3 to 63 lowercase letters, digits, and single
/// hyphens, starting and ending with a letter or digit.
pub fn valid_container_name(name: &str) -> bool {
    (3..=63).contains(&name.len())
        && !name.starts_with('-')
        && !name.ends_with('-')
        && !name.contains("--")
        && name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

struct Account<'a> {
    host: &'a str,
    ipv6: bool,
    port: Option<u16>,
    segment: Option<&'a str>,
}

/// Validate the part of an account URL after `scheme://`, mirroring Go's
/// `ValidateAzureAccountURL`: a host with an optional port, then at most one
/// simple path segment with an optional trailing slash.
fn parse_account(rest: &str) -> Result<Account<'_>> {
    let invalid = Err(Error::InvalidRequest);
    let (authority, path) = rest.split_at(rest.find('/').unwrap_or(rest.len()));
    // Hosts are DNS names, IPv4 addresses, or bracketed IP literals with an
    // optional port. This also rejects userinfo and zone identifiers.
    if authority.is_empty()
        || authority.ends_with(':')
        || !authority
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b':' | b'[' | b']'))
    {
        return invalid;
    }
    let (host, ipv6, port) = if let Some(bracketed) = authority.strip_prefix('[') {
        let Some((host, after)) = bracketed.split_once(']') else {
            return invalid;
        };
        // Stricter than Go: only IPv6 literals belong in brackets.
        if host.parse::<std::net::Ipv6Addr>().is_err() {
            return invalid;
        }
        let port = match after {
            "" => None,
            _ => Some(after.strip_prefix(':').ok_or(Error::InvalidRequest)?),
        };
        (host, true, port)
    } else {
        if authority.contains(['[', ']']) {
            return invalid;
        }
        match authority.split_once(':') {
            Some((host, port)) => (host, false, Some(port)),
            None => (authority, false, None),
        }
    };
    if host.is_empty() {
        return invalid;
    }
    let port = match port {
        None => None,
        Some(p) => {
            if p.is_empty() || !p.bytes().all(|b| b.is_ascii_digit()) {
                return invalid;
            }
            match p.parse::<u16>() {
                Ok(n) if n != 0 => Some(n),
                _ => return invalid,
            }
        }
    };
    if path == "//" {
        return invalid;
    }
    let segment = path.strip_prefix('/').unwrap_or(path);
    let segment = segment.strip_suffix('/').unwrap_or(segment);
    if !segment
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_'))
    {
        return invalid;
    }
    Ok(Account {
        host,
        ipv6,
        port,
        segment: (!segment.is_empty()).then_some(segment),
    })
}

/// Canonical GUID syntax accepted by the CRD, in either letter case.
fn valid_guid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_hexdigit()
            }
        })
}

impl AzureCredential {
    /// Validate identifier syntax.
    pub fn validate(&self) -> Result<()> {
        let ok = match self {
            Self::Anonymous => true,
            Self::ManagedIdentity { client_id } => client_id.as_deref().is_none_or(valid_guid),
            Self::WorkloadIdentity {
                client_id,
                tenant_id,
            } => valid_guid(client_id) && tenant_id.as_deref().is_none_or(valid_guid),
        };
        ok.then_some(()).ok_or(Error::InvalidRequest)
    }
}

impl OriginDefinition {
    /// Build a definition with default tuning.
    pub fn azure(container_url: AzureContainerUrl, credential: AzureCredential) -> Self {
        Self {
            azure: AzureOriginDefinition {
                container_url,
                credential,
            },
            metadata_ttl: DEFAULT_ORIGIN_METADATA_TTL,
            request_timeout: DEFAULT_ORIGIN_REQUEST_TIMEOUT,
            max_concurrent_requests: DEFAULT_ORIGIN_MAX_CONCURRENT_REQUESTS,
        }
    }

    /// Validate bounds, millisecond precision, credentials, and that every
    /// credential except `Anonymous` uses HTTPS.
    pub fn validate(&self) -> Result<()> {
        let millis = |d: Duration| {
            d.subsec_nanos().is_multiple_of(1_000_000) && d.as_millis() <= u128::from(u64::MAX)
        };
        self.azure.credential.validate()?;
        if !millis(self.metadata_ttl)
            || !millis(self.request_timeout)
            || self.request_timeout.is_zero()
            || !(1..=MAX_ORIGIN_CONCURRENT_REQUESTS).contains(&self.max_concurrent_requests)
            || (self.azure.credential != AzureCredential::Anonymous
                && !self.azure.container_url.scheme_is_https())
        {
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Go-compatible account validation for tests, with "default" requiring HTTPS.
    fn account(value: &str, credential: &str) -> Result<()> {
        if credential != "default" && credential != "anonymous" {
            return Err(Error::InvalidRequest);
        }
        if value.contains(['?', '#']) {
            return Err(Error::InvalidRequest);
        }
        let (scheme, rest) = value.split_once("://").ok_or(Error::InvalidRequest)?;
        if scheme != "https" && scheme != "http" {
            return Err(Error::InvalidRequest);
        }
        parse_account(rest)?;
        if credential == "default" && scheme != "https" {
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }

    fn container(value: &str, credential: &str) -> Result<(String, String)> {
        let url = AzureContainerUrl::parse(value)?;
        let credential = match credential {
            "default" => AzureCredential::ManagedIdentity { client_id: None },
            "anonymous" => AzureCredential::Anonymous,
            _ => return Err(Error::InvalidRequest),
        };
        OriginDefinition::azure(url.clone(), credential).validate()?;
        let account = &value[..value.len() - url.container().len() - 1];
        Ok((account.into(), url.container().into()))
    }

    /// Port of Go TestParseAzureContainerURL.
    #[test]
    fn parse_container_url() {
        let a63 = "a".repeat(63);
        for (value, credential, account, name) in [
            (
                "https://account.blob.core.windows.net/models".to_string(),
                "default",
                "https://account.blob.core.windows.net",
                "models",
            ),
            (
                "https://account.blob.core.windows.net:443/a-1".into(),
                "anonymous",
                "https://account.blob.core.windows.net:443",
                "a-1",
            ),
            (
                "http://127.0.0.1:10000/devstoreaccount1/container".into(),
                "anonymous",
                "http://127.0.0.1:10000/devstoreaccount1",
                "container",
            ),
            (
                "https://[::1]/account/abc".into(),
                "default",
                "https://[::1]/account",
                "abc",
            ),
            (
                format!("https://host/{a63}"),
                "default",
                "https://host",
                &a63,
            ),
        ] {
            assert_eq!(
                container(&value, credential),
                Ok((account.into(), name.into())),
                "{value}"
            );
        }
        for value in [
            "".to_string(),
            "models".into(),
            "https://account.blob.core.windows.net".into(),
            "https://account.blob.core.windows.net/".into(),
            "https://account.blob.core.windows.net/models/".into(),
            "https://host/account/models/extra".into(),
            "ftp://host/models".into(),
            "https://user:private-token@host/models".into(),
            "https://host/models?sig=private-token".into(),
            "https://host/models#private-token".into(),
            "https://host/Models".into(),
            "https://host/ab".into(),
            format!("https://host/{}", "a".repeat(64)),
            "https://host/-ab".into(),
            "https://host/ab-".into(),
            "https://host/a--b".into(),
            "https://host/a_b".into(),
            "https://host/a.b".into(),
            "https://host/%61bc".into(),
            "https://host//models".into(),
            "https:///models".into(),
            "https://host/acc%2Fount/models".into(),
            "https://host:0/models".into(),
        ] {
            for credential in ["default", "anonymous"] {
                assert!(
                    container(&value, credential).is_err(),
                    "{value} {credential}"
                );
            }
        }
        assert!(
            container(
                "http://127.0.0.1:10000/devstoreaccount1/container",
                "default"
            )
            .is_err()
        );
        assert!(container("https://host/models", "workload").is_err());
    }

    /// Port of Go TestAzureAccountURLValidation.
    #[test]
    fn account_url_validation() {
        for value in [
            "https://account.blob.core.windows.net",
            "https://account/",
            "https://account:443",
            "https://localhost:10000/devstoreaccount1",
            "https://localhost:10000/devstoreaccount1/",
            "https://[::1]:443/account",
            "https://[::1]/account",
            "https://host/account-name_1",
        ] {
            for credential in ["default", "anonymous"] {
                assert_eq!(account(value, credential), Ok(()), "{value} {credential}");
            }
        }
        for value in [
            "http://localhost:10000/devstoreaccount1",
            "http://127.0.0.1/",
            "http://[::1]:80",
        ] {
            assert_eq!(account(value, "anonymous"), Ok(()), "{value}");
            assert!(account(value, "default").is_err(), "{value}");
        }
        for value in [
            "",
            "account",
            "ftp://account",
            "https://",
            "https:///account",
            "https:account",
            "https://user:private-token@account",
            "https://account?sig=private-token",
            "https://account?",
            "https://account#private-token",
            "https://account#",
            "https://account:0",
            "https://account:65536",
            "https://account:abc",
            "https://account:",
            "https://account:-1",
            "https://account:+80",
            "https://account:99999999999999999999999",
            "https://::1",
            "https://[host]:80",
            "https://[::1%25eth0]:80",
            "https://host:80:90",
            "https://host//",
            "https://host//account",
            "https://host/account//",
            "https://host/account/container",
            "https://host/.",
            "https://host/..",
            "https://host/%2e%2e",
            "https://host/%2f",
            "https://host/account%2Fcontainer",
            "https://host/%61ccount",
            "https://host/%252f",
            "https://host/account%5ccontainer",
            "https://$(STORAGE_HOST)",
            "https://host$(X).net",
            "https://ho_st",
            "https://host/account\\container",
            "https://host/account;param",
            "https://host/account name",
            "https://host/\u{96ea}",
            "https://host/%00",
            "https://host/%zz",
        ] {
            for credential in ["default", "anonymous"] {
                assert!(account(value, credential).is_err(), "{value} {credential}");
            }
        }
        assert!(account("https://account.blob.core.windows.net", "workload").is_err());
    }

    #[test]
    fn accessors() {
        let u = AzureContainerUrl::parse("https://account.blob.core.windows.net/models").unwrap();
        assert!(u.scheme_is_https());
        assert_eq!(
            (u.host(), u.port(), u.account_path(), u.container()),
            ("account.blob.core.windows.net", 443, None, "models")
        );
        assert_eq!(u.host_header(), "account.blob.core.windows.net");
        assert_eq!(u.as_str(), "https://account.blob.core.windows.net/models");

        let u =
            AzureContainerUrl::parse("http://127.0.0.1:10000/devstoreaccount1/container").unwrap();
        assert!(!u.scheme_is_https());
        assert_eq!(
            (u.host(), u.port(), u.account_path(), u.container()),
            ("127.0.0.1", 10000, Some("devstoreaccount1"), "container")
        );
        assert_eq!(u.host_header(), "127.0.0.1:10000");

        let u = AzureContainerUrl::parse("http://host:80/abc").unwrap();
        assert_eq!((u.port(), u.host_header().as_str()), (80, "host"));

        let u = AzureContainerUrl::parse("https://[::1]/account/abc").unwrap();
        assert_eq!((u.host(), u.port()), ("::1", 443));
        assert_eq!(u.host_header(), "[::1]");
        let u = AzureContainerUrl::parse("http://[::1]:10000/abc").unwrap();
        assert_eq!(u.host_header(), "[::1]:10000");

        let long = format!("https://{}.net/abc", "a".repeat(MAX_CONTAINER_URL_BYTES));
        assert!(AzureContainerUrl::parse(&long).is_err());
        assert!(AzureContainerUrl::parse("HTTPS://host/abc").is_err());
    }

    #[test]
    fn origin_validation() {
        let https = AzureContainerUrl::parse("https://host/abc").unwrap();
        let http = AzureContainerUrl::parse("http://host/abc").unwrap();
        let guid = "0123abcd-0123-4abc-89ab-0123456789AB".to_string();
        let ok = OriginDefinition::azure(https.clone(), AzureCredential::Anonymous);
        assert_eq!(ok.validate(), Ok(()));
        assert_eq!(
            OriginDefinition::azure(http.clone(), AzureCredential::Anonymous).validate(),
            Ok(())
        );
        for credential in [
            AzureCredential::ManagedIdentity { client_id: None },
            AzureCredential::ManagedIdentity {
                client_id: Some(guid.clone()),
            },
            AzureCredential::WorkloadIdentity {
                client_id: guid.clone(),
                tenant_id: None,
            },
            AzureCredential::WorkloadIdentity {
                client_id: guid.clone(),
                tenant_id: Some(guid.clone()),
            },
        ] {
            assert_eq!(
                OriginDefinition::azure(https.clone(), credential.clone()).validate(),
                Ok(())
            );
            assert!(
                OriginDefinition::azure(http.clone(), credential)
                    .validate()
                    .is_err()
            );
        }
        for credential in [
            AzureCredential::ManagedIdentity {
                client_id: Some(String::new()),
            },
            AzureCredential::WorkloadIdentity {
                client_id: "nope".into(),
                tenant_id: None,
            },
            AzureCredential::WorkloadIdentity {
                client_id: guid.clone(),
                tenant_id: Some("x".into()),
            },
        ] {
            assert!(
                OriginDefinition::azure(https.clone(), credential)
                    .validate()
                    .is_err()
            );
        }
        let bad = |f: fn(&mut OriginDefinition)| {
            let mut o = ok.clone();
            f(&mut o);
            o.validate()
        };
        assert!(bad(|o| o.request_timeout = Duration::ZERO).is_err());
        assert!(bad(|o| o.request_timeout = Duration::from_micros(1500)).is_err());
        assert!(bad(|o| o.metadata_ttl = Duration::from_nanos(1)).is_err());
        assert!(bad(|o| o.max_concurrent_requests = 0).is_err());
        assert!(bad(|o| o.max_concurrent_requests = 65537).is_err());
        assert_eq!(bad(|o| o.metadata_ttl = Duration::ZERO), Ok(()));
        assert_eq!(bad(|o| o.max_concurrent_requests = 65536), Ok(()));
    }
}
