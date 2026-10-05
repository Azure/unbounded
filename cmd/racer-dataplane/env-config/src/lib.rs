//! Strict lexical helpers. Names, defaults, limits, and cross-field policy belong
//! to the application. No filesystem access or hostname resolution is performed.

use std::net::{IpAddr, SocketAddr};
use std::num::{NonZeroU32, NonZeroUsize};
use std::path::Path;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidValue;
pub type Result<T> = std::result::Result<T, InvalidValue>;

pub fn lookup(name: &str) -> Result<Option<String>> {
    lookup_result(std::env::var(name))
}

fn lookup_result(value: std::result::Result<String, std::env::VarError>) -> Result<Option<String>> {
    match value {
        Ok(value) => Ok(Some(value)),
        Err(std::env::VarError::NotPresent) => Ok(None),
        Err(std::env::VarError::NotUnicode(_)) => Err(InvalidValue),
    }
}

pub fn text(value: Option<String>, default: Option<&str>, maximum: usize) -> Result<String> {
    let value = value
        .or_else(|| default.map(str::to_owned))
        .ok_or(InvalidValue)?;
    if value.is_empty() || value.len() > maximum || value.chars().any(char::is_control) {
        return Err(InvalidValue);
    }
    Ok(value)
}

pub fn boolean(value: &str) -> Result<bool> {
    value.parse().map_err(|_| InvalidValue)
}

pub fn unsigned(value: &str) -> Result<u64> {
    if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
        return Err(InvalidValue);
    }
    value.parse().map_err(|_| InvalidValue)
}

pub fn usize_value(value: u64) -> Result<usize> {
    usize::try_from(value).map_err(|_| InvalidValue)
}

pub fn nonzero_usize(value: u64) -> Result<NonZeroUsize> {
    NonZeroUsize::new(usize_value(value)?).ok_or(InvalidValue)
}

pub fn nonzero_u32(value: u64) -> Result<NonZeroU32> {
    NonZeroU32::new(u32::try_from(value).map_err(|_| InvalidValue)?).ok_or(InvalidValue)
}

/// Lexically absolute, non-root UTF-8 path with no empty, dot, or parent parts.
/// Limits are byte lengths. This does not assert anything about symlinks or mounts.
pub fn absolute_path(path: &Path, maximum: usize, component_maximum: usize) -> Result<()> {
    let text = path.to_str().ok_or(InvalidValue)?;
    if !path.is_absolute()
        || text.len() > maximum
        || text.chars().any(char::is_control)
        || text[1..].split('/').any(|part| {
            part.is_empty() || matches!(part, "." | "..") || part.len() > component_maximum
        })
    {
        return Err(InvalidValue);
    }
    Ok(())
}

/// Numeric socket syntax only, with no DNS or application-specific normalization.
pub fn socket_address(value: &str) -> Result<SocketAddr> {
    value.parse().map_err(|_| InvalidValue)
}

/// Nonzero-port unicast or unspecified address without IPv6 scope/flow metadata
/// or mapped IPv4. Applications decide whether unspecified addresses are allowed.
pub fn unscoped_socket(address: SocketAddr) -> Result<()> {
    if address.port() == 0
        || address.ip().is_multicast()
        || matches!(address.ip(), IpAddr::V4(ip) if ip.is_broadcast())
        || matches!(address, SocketAddr::V6(ip) if ip.flowinfo() != 0 || ip.scope_id() != 0 || ip.ip().to_ipv4_mapped().is_some())
    {
        return Err(InvalidValue);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lookup_distinguishes_absence_empty_and_nonunicode() {
        assert_eq!(lookup_result(Ok(String::new())), Ok(Some(String::new())));
        assert_eq!(lookup_result(Ok("value".into())), Ok(Some("value".into())));
        assert_eq!(lookup_result(Err(std::env::VarError::NotPresent)), Ok(None));
        #[cfg(unix)]
        {
            use std::os::unix::ffi::OsStringExt;
            let value = std::ffi::OsString::from_vec(vec![0xff]);
            assert_eq!(
                lookup_result(Err(std::env::VarError::NotUnicode(value))),
                Err(InvalidValue)
            );
        }
    }

    #[test]
    fn text_defaults_and_strict_values() {
        assert_eq!(text(None, Some("ok"), 2), Ok("ok".into()));
        assert_eq!(text(Some("é".into()), None, 2), Ok("é".into()));
        assert!(text(Some("é".into()), None, 1).is_err());
        assert!(text(None, None, 10).is_err());
        for value in ["", "a\n", "\0", "\u{85}", "abc"] {
            assert!(text(Some(value.into()), Some("ok"), 2).is_err());
        }
        assert_eq!(boolean("true"), Ok(true));
        assert_eq!(boolean("false"), Ok(false));
        for value in ["", "TRUE", "1", " false", "false\n"] {
            assert!(boolean(value).is_err());
        }
        assert_eq!(unsigned("000"), Ok(0));
        assert_eq!(unsigned("18446744073709551615"), Ok(u64::MAX));
        for value in [
            "",
            "+1",
            "-1",
            " 1",
            "1 ",
            "1\n",
            "１",
            "18446744073709551616",
        ] {
            assert!(unsigned(value).is_err());
        }
        assert!(nonzero_u32(0).is_err());
        assert!(nonzero_u32(u32::MAX as u64 + 1).is_err());
        assert_eq!(nonzero_u32(u32::MAX as u64).unwrap().get(), u32::MAX);
        assert!(nonzero_usize(0).is_err());
        assert_eq!(nonzero_usize(usize::MAX as u64).unwrap().get(), usize::MAX);
    }

    #[test]
    fn lexical_path_limits() {
        assert!(absolute_path(Path::new("/ab/c"), 5, 2).is_ok());
        assert!(absolute_path(Path::new("/ab/c"), 4, 2).is_err());
        assert!(absolute_path(Path::new("/ab/c"), 5, 1).is_err());
        for path in [
            "", "/", "relative", "/a/", "/a//b", "/a/./b", "/a/../b", "/a\0", "/a\n",
        ] {
            assert!(absolute_path(Path::new(path), 4095, 255).is_err());
        }
        #[cfg(unix)]
        {
            use std::os::unix::ffi::OsStrExt;
            assert!(
                absolute_path(Path::new(std::ffi::OsStr::from_bytes(b"/\xff")), 4095, 255).is_err()
            );
        }
    }

    #[test]
    fn numeric_socket_profile() {
        for value in ["0.0.0.0:1", "[::]:1", "127.0.0.1:65535", "[::1]:1"] {
            unscoped_socket(socket_address(value).unwrap()).unwrap();
        }
        for value in [
            "localhost:1",
            "[127.0.0.1]:1",
            "127.0.0.1:+1",
            "127.0.0.1:65536",
        ] {
            assert!(socket_address(value).is_err());
        }
        for value in [
            "127.0.0.1:0",
            "224.0.0.1:1",
            "255.255.255.255:1",
            "[ff02::1]:1",
            "[::ffff:127.0.0.1]:1",
            "[fe80::1%2]:1",
        ] {
            assert!(unscoped_socket(socket_address(value).unwrap()).is_err());
        }
        assert!(
            unscoped_socket(SocketAddr::V6(std::net::SocketAddrV6::new(
                std::net::Ipv6Addr::LOCALHOST,
                1,
                1,
                0
            )))
            .is_err()
        );
    }
}
