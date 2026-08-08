// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Minimal URL parser for backend `url` config values.
//!
//! A backend URL is a value of the form
//! `scheme://host[:port]` where `scheme` is `http` or `https`.
//! Parsing of the authority (host and optional port) is delegated to
//! [`http::Uri`] from the `http` crate, which the backends already
//! depend on, rather than re-implementing URL parsing or pulling in the
//! `url` crate. `http::Uri` is permissive in ways a config value must
//! not be, so this is a thin wrapper that enforces the strict rules:
//! the scheme must be `http` or `https`, userinfo, IPv6 literals, and endpoint
//! path/query/fragment components are rejected, the host must be non-empty, and
//! a present port must be a valid `u16` (`http::Uri` silently ignores an
//! out-of-range or non-numeric port). Backends only need the scheme (to
//! decide TLS), the host (for DNS resolution, TLS SNI, and the `Host:`
//! header), and the port (defaulted from the scheme when absent). The
//! Object paths are derived from requests, not the endpoint.

use std::fmt;

use ::http::Uri;

/// Transport scheme of a backend URL.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scheme {
    /// Plaintext HTTP/1.1 (default port 80).
    Http,
    /// HTTP/1.1 over TLS (default port 443).
    Https,
}

impl Scheme {
    /// Default TCP port for the scheme.
    pub fn default_port(self) -> u16 {
        match self {
            Scheme::Http => 80,
            Scheme::Https => 443,
        }
    }

    /// Whether the scheme requires a TLS connection.
    pub fn is_tls(self) -> bool {
        matches!(self, Scheme::Https)
    }
}

/// Parsed backend URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EndpointUrl {
    /// Transport scheme (`http` or `https`).
    pub scheme: Scheme,
    /// Host without brackets or port (DNS name or IP literal).
    pub host: String,
    /// Resolved port: explicit when present, else the scheme default.
    pub port: u16,
}

impl EndpointUrl {
    /// Host and port rendered as `host:port`, suitable for DNS
    /// resolution via `ToSocketAddrs`.
    pub fn authority(&self) -> String {
        format!("{}:{}", self.host, self.port)
    }

    /// Value for the HTTP `Host:` header. The default port for the
    /// scheme is omitted so a default-port endpoint sends the canonical
    /// bare host (`example.com`, not `example.com:443`); virtual-hosted
    /// origins (S3, Azure Blob, CDN front ends) that route on the bare
    /// server name reject or mis-route a redundant default port. A
    /// non-default port is retained, as the peer expects it.
    pub fn host_header(&self) -> String {
        if self.port == self.scheme.default_port() {
            self.host.clone()
        } else {
            format!("{}:{}", self.host, self.port)
        }
    }
}

/// Error returned when a backend URL string is not valid.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum UrlError {
    /// The string has no `scheme://` prefix.
    MissingScheme,
    /// The scheme is neither `http` nor `https`.
    UnsupportedScheme(String),
    /// The host component is empty.
    EmptyHost,
    /// The port component is present but not a valid `u16`.
    InvalidPort(String),
    /// User credentials are not supported in backend endpoint URLs.
    Userinfo,
    /// Origin connections currently support IPv4 only.
    Ipv6Unsupported,
    /// A non-root path, query, or fragment would be discarded by the backend.
    UnsupportedTarget,
}

impl fmt::Display for UrlError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            UrlError::MissingScheme => {
                write!(f, "backend url must be of the form scheme://host[:port]")
            }
            UrlError::UnsupportedScheme(s) => {
                let _ = s;
                write!(
                    f,
                    "unsupported backend url scheme (expected http or https)"
                )
            }
            UrlError::EmptyHost => write!(f, "backend url host must not be empty"),
            UrlError::InvalidPort(p) => {
                let _ = p;
                write!(f, "invalid backend url port")
            }
            UrlError::Userinfo => write!(f, "backend url must not contain userinfo"),
            UrlError::Ipv6Unsupported => {
                write!(f, "IPv6 backend urls are not supported")
            }
            UrlError::UnsupportedTarget => write!(
                f,
                "backend url must not contain a base path, query, or fragment"
            ),
        }
    }
}

impl std::error::Error for UrlError {}

/// Parse a backend `url` string into an [`EndpointUrl`].
pub fn parse_endpoint(url: &str) -> Result<EndpointUrl, UrlError> {
    let (scheme_str, rest) = url.split_once("://").ok_or(UrlError::MissingScheme)?;
    if rest.is_empty() {
        return Err(UrlError::EmptyHost);
    }

    let scheme = match scheme_str.to_ascii_lowercase().as_str() {
        "http" => Scheme::Http,
        "https" => Scheme::Https,
        other => return Err(UrlError::UnsupportedScheme(other.to_string())),
    };

    if rest.contains('#') {
        return Err(UrlError::UnsupportedTarget);
    }

    // http::Uri handles authority and path/query parsing. It rejects an empty
    // host outright (parse error), which we surface as `EmptyHost`.
    let uri: Uri = url.parse().map_err(|_| UrlError::EmptyHost)?;
    let authority = uri.authority().ok_or(UrlError::EmptyHost)?;
    if authority.as_str().contains('@') {
        return Err(UrlError::Userinfo);
    }
    if uri
        .path_and_query()
        .is_some_and(|target| target.as_str() != "/")
    {
        return Err(UrlError::UnsupportedTarget);
    }

    let raw_host = authority.host();
    if raw_host.is_empty() {
        return Err(UrlError::EmptyHost);
    }
    if raw_host.starts_with('[') {
        return Err(UrlError::Ipv6Unsupported);
    }

    let host = raw_host.trim_end_matches('.').to_string();

    if host.is_empty() {
        return Err(UrlError::EmptyHost);
    }

    let port = resolve_port(authority.as_str(), raw_host, scheme)?;

    Ok(EndpointUrl { scheme, host, port })
}

/// Resolve the URL port from an `http::Uri` authority.
///
/// `http::Uri` treats an out-of-range or non-numeric port as absent
/// (`port_u16()` returns `None`), so a typo'd port would silently fall
/// back to the scheme default. Detect a present-but-invalid port from
/// the raw authority string and reject it instead. `authority` is the
/// `[userinfo@]host[:port]` string and `raw_host` is its host component
/// with IPv6 brackets still attached, so the substring after the host is
/// the literal `:port` (or empty when no port is given).
fn resolve_port(authority: &str, raw_host: &str, scheme: Scheme) -> Result<u16, UrlError> {
    let host_port = match authority.rsplit_once('@') {
        Some((_, hp)) => hp,
        None => authority,
    };

    match host_port[raw_host.len()..].strip_prefix(':') {
        Some(p) => p
            .parse::<u16>()
            .map_err(|_| UrlError::InvalidPort(p.to_string())),
        None => Ok(scheme.default_port()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_http_default_port() {
        let u = parse_endpoint("http://origin.example.com").unwrap();
        assert_eq!(u.scheme, Scheme::Http);
        assert_eq!(u.host, "origin.example.com");
        assert_eq!(u.port, 80);
        assert_eq!(u.authority(), "origin.example.com:80");
    }

    #[test]
    fn parses_https_default_port() {
        let u = parse_endpoint("https://origin.example.com").unwrap();
        assert_eq!(u.scheme, Scheme::Https);
        assert_eq!(u.port, 443);
        assert!(u.scheme.is_tls());
    }

    #[test]
    fn parses_explicit_port() {
        let u = parse_endpoint("https://s3.us-east-1.amazonaws.com:8443").unwrap();
        assert_eq!(u.host, "s3.us-east-1.amazonaws.com");
        assert_eq!(u.port, 8443);
    }

    #[test]
    fn host_header_omits_default_port() {
        // Default ports are dropped so the Host header stays canonical
        // and virtual-hosted origins route on the bare server name.
        let u = parse_endpoint("https://s3.example.com").unwrap();
        assert_eq!(u.authority(), "s3.example.com:443");
        assert_eq!(u.host_header(), "s3.example.com");

        let u = parse_endpoint("http://origin.example.com").unwrap();
        assert_eq!(u.host_header(), "origin.example.com");

        // An explicitly written default port is also collapsed.
        let u = parse_endpoint("https://s3.example.com:443").unwrap();
        assert_eq!(u.host_header(), "s3.example.com");
    }

    #[test]
    fn host_header_retains_non_default_port() {
        let u = parse_endpoint("https://minio.local:9000").unwrap();
        assert_eq!(u.host_header(), "minio.local:9000");

        let u = parse_endpoint("http://origin.example.com:8080").unwrap();
        assert_eq!(u.host_header(), "origin.example.com:8080");
    }

    #[test]
    fn accepts_root_path() {
        let u = parse_endpoint("http://host:81/").unwrap();
        assert_eq!(u.host, "host");
    }

    #[test]
    fn rejects_path_query_fragment_and_userinfo() {
        assert_eq!(
            parse_endpoint("http://host:81/bucket/object"),
            Err(UrlError::UnsupportedTarget)
        );
        assert_eq!(
            parse_endpoint("http://host:81/?x=1"),
            Err(UrlError::UnsupportedTarget)
        );
        assert_eq!(
            parse_endpoint("http://host:81/#frag"),
            Err(UrlError::UnsupportedTarget)
        );
        assert_eq!(
            parse_endpoint("https://user:pass@host:443"),
            Err(UrlError::Userinfo)
        );
    }

    #[test]
    fn parses_ipv4_literal() {
        let u = parse_endpoint("http://127.0.0.1:8080").unwrap();
        assert_eq!(u.host, "127.0.0.1");
        assert_eq!(u.port, 8080);
    }

    #[test]
    fn rejects_bracketed_ipv6() {
        assert_eq!(
            parse_endpoint("https://[::1]:9443"),
            Err(UrlError::Ipv6Unsupported)
        );
        assert_eq!(
            parse_endpoint("https://[2001:db8::1]"),
            Err(UrlError::Ipv6Unsupported)
        );
    }

    #[test]
    fn case_insensitive_scheme() {
        assert_eq!(parse_endpoint("HTTPS://h").unwrap().scheme, Scheme::Https);
    }

    #[test]
    fn rejects_missing_scheme() {
        assert_eq!(
            parse_endpoint("origin.example.com:443"),
            Err(UrlError::MissingScheme)
        );
    }

    #[test]
    fn rejects_unsupported_scheme() {
        assert_eq!(
            parse_endpoint("ftp://host"),
            Err(UrlError::UnsupportedScheme("ftp".to_string()))
        );
    }

    #[test]
    fn rejects_empty_host() {
        assert_eq!(parse_endpoint("http:///path"), Err(UrlError::EmptyHost));
    }

    #[test]
    fn rejects_invalid_port() {
        assert_eq!(
            parse_endpoint("http://host:99999"),
            Err(UrlError::InvalidPort("99999".to_string()))
        );
        assert!(matches!(
            parse_endpoint("http://host:abc"),
            Err(UrlError::InvalidPort(_))
        ));
    }
}
