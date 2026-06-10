// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Minimal URL parser for backend `url` config values.
//!
//! A backend URL is a value of the form
//! `scheme://host[:port][/path]` where `scheme` is `http` or `https`.
//! This is intentionally a tiny hand-rolled parser rather than a
//! dependency on the `url` crate: backends only need the scheme (to
//! decide TLS), the host (for DNS resolution, TLS SNI, and the `Host:`
//! header), and the port (defaulted from the scheme when absent). The
//! path component is accepted and ignored; object paths are derived
//! from the request, not the endpoint.

use std::fmt;

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
}

impl fmt::Display for UrlError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            UrlError::MissingScheme => {
                write!(f, "backend url must be of the form scheme://host[:port]")
            }
            UrlError::UnsupportedScheme(s) => {
                write!(f, "unsupported backend url scheme {s:?} (expected http or https)")
            }
            UrlError::EmptyHost => write!(f, "backend url host must not be empty"),
            UrlError::InvalidPort(p) => write!(f, "invalid backend url port {p:?}"),
        }
    }
}

impl std::error::Error for UrlError {}

/// Parse a backend `url` string into an [`EndpointUrl`].
pub fn parse_endpoint(url: &str) -> Result<EndpointUrl, UrlError> {
    let (scheme_str, rest) = url.split_once("://").ok_or(UrlError::MissingScheme)?;

    let scheme = match scheme_str.to_ascii_lowercase().as_str() {
        "http" => Scheme::Http,
        "https" => Scheme::Https,
        other => return Err(UrlError::UnsupportedScheme(other.to_string())),
    };

    // Strip any path, query, or fragment; only the authority matters.
    let authority = rest
        .split(['/', '?', '#'])
        .next()
        .unwrap_or("")
        .trim_end_matches('.');

    // Drop optional userinfo (`user:pass@host`); anonymous origins only.
    let authority = match authority.rsplit_once('@') {
        Some((_, host_port)) => host_port,
        None => authority,
    };

    let (host, port) = split_host_port(authority, scheme)?;

    if host.is_empty() {
        return Err(UrlError::EmptyHost);
    }

    Ok(EndpointUrl {
        scheme,
        host: host.to_string(),
        port,
    })
}

/// Split an authority into host and port, honoring `[ipv6]:port`
/// bracket form and defaulting the port from the scheme.
fn split_host_port(authority: &str, scheme: Scheme) -> Result<(&str, u16), UrlError> {
    if let Some(after_bracket) = authority.strip_prefix('[') {
        // Bracketed IPv6 literal: `[host]` or `[host]:port`.
        let (host, tail) = after_bracket
            .split_once(']')
            .ok_or_else(|| UrlError::InvalidPort(authority.to_string()))?;
        let port = match tail.strip_prefix(':') {
            Some(p) => parse_port(p)?,
            None if tail.is_empty() => scheme.default_port(),
            None => return Err(UrlError::InvalidPort(authority.to_string())),
        };
        return Ok((host, port));
    }

    match authority.rsplit_once(':') {
        Some((host, p)) => Ok((host, parse_port(p)?)),
        None => Ok((authority, scheme.default_port())),
    }
}

/// Parse a non-empty port string into a `u16`.
fn parse_port(p: &str) -> Result<u16, UrlError> {
    p.parse::<u16>()
        .map_err(|_| UrlError::InvalidPort(p.to_string()))
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
    fn strips_path_query_fragment() {
        let u = parse_endpoint("http://host:81/bucket/object?x=1#frag").unwrap();
        assert_eq!(u.host, "host");
        assert_eq!(u.port, 81);
    }

    #[test]
    fn strips_userinfo() {
        let u = parse_endpoint("https://user:pass@host:443").unwrap();
        assert_eq!(u.host, "host");
        assert_eq!(u.port, 443);
    }

    #[test]
    fn parses_ipv4_literal() {
        let u = parse_endpoint("http://127.0.0.1:8080").unwrap();
        assert_eq!(u.host, "127.0.0.1");
        assert_eq!(u.port, 8080);
    }

    #[test]
    fn parses_bracketed_ipv6() {
        let u = parse_endpoint("https://[::1]:9443").unwrap();
        assert_eq!(u.host, "::1");
        assert_eq!(u.port, 9443);

        let u = parse_endpoint("https://[2001:db8::1]").unwrap();
        assert_eq!(u.host, "2001:db8::1");
        assert_eq!(u.port, 443);
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
