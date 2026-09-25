//! Environment and deployment configuration, validated before startup.
//!
//! Default to at most eight total userspace threads (four I/O/crypto worker pairs).
//! Shares and rail alignment come exclusively from accepted controller membership.
//! See ../CONFIGURATION.md for environment names, defaults, and startup obligations.

use crate::{
    error::{Error, Result},
    model::{
        identity::{ClusterId, NodeId},
        limits::Limits,
        range::PAGE_BYTES,
    },
    store::format::MAX_HEADER_BYTES,
};
use std::{
    net::{IpAddr, SocketAddr},
    num::NonZeroUsize,
    path::{Path, PathBuf},
    time::Duration,
};

pub const DEFAULT_MAX_THREADS: usize = 8;
/// Not a Node UID. Only verified enrollment/local identity recovery may replace it.
pub const UNRESOLVED_NODE_ID: &str = "";
const MIB: u64 = 1024 * 1024;
const MAX_BYTES: u64 = 64 * 1024 * MIB;
const MAX_ENTRIES: usize = 1_048_576;

pub struct Config {
    pub cluster: ClusterId,
    /// Resolved by verified bootstrap/local identity recovery before workers start,
    /// not from a caller-provided UID or the Downward API's node name.
    /// from_env leaves this as UNRESOLVED_NODE_ID; validation is not authentication.
    pub node: NodeId,
    /// Total thread cap, minimum two; odd caps round down to complete worker pairs.
    /// Control and diagnostics run on I/O threads within this budget.
    pub max_threads: usize,
    pub enable_rdma: bool,
    pub control_endpoint: String,
    pub peer_listen: std::net::SocketAddr,
    pub diagnostics_listen: std::net::SocketAddr,
    pub trust_bundle: PathBuf,
    pub service_account_token: PathBuf,
    pub secret_directory: PathBuf,
    /// Node-private persistent keys, separate from projected Secrets and slabs.
    pub identity_directory: PathBuf,
    pub slab_directory: PathBuf,
    pub slab_bytes: u64,
    pub segment_bytes: u64,
    pub free_segment_reserve: usize,
    pub limits: Limits,
    pub request_timeout: Duration,
    pub reader_stall_timeout: Duration,
    pub shutdown_timeout: Duration,
}

impl Config {
    /// Read only supported settings. No files, sockets, threads, or pools are opened.
    pub fn from_env() -> Result<Self> {
        Self::from_lookup(|name| match std::env::var(name) {
            Ok(value) => Ok(Some(value)),
            Err(std::env::VarError::NotPresent) => Ok(None),
            Err(std::env::VarError::NotUnicode(_)) => Err(Error::InvalidConfiguration),
        })
    }

    /// Injectable lookup keeps parser tests independent of the process environment.
    fn from_lookup(mut lookup: impl FnMut(&str) -> Result<Option<String>>) -> Result<Self> {
        // Fail closed on common obsolete authority overrides, even if empty.
        for name in [
            "RACER_NODE_UID",
            "RACER_NODE_ID",
            "RACER_SHARES",
            "RACER_RAILS",
            "RACER_ALIGNED_RAILS",
        ] {
            if lookup(name)?.is_some() {
                return Err(Error::InvalidConfiguration);
            }
        }
        let mut text = |name: &str, default: Option<&str>| -> Result<String> {
            let value = lookup(name)?
                .or_else(|| default.map(str::to_owned))
                .ok_or(Error::InvalidConfiguration)?;
            if value.is_empty() || value.len() > 4096 || value.chars().any(char::is_control) {
                return Err(Error::InvalidConfiguration);
            }
            Ok(value)
        };
        let cluster = ClusterId(text("RACER_CLUSTER_ID", None)?);
        let control_endpoint = text("RACER_CONTROL_ENDPOINT", None)?;
        let enable_rdma = match text("RACER_ENABLE_RDMA", Some("false"))?.as_str() {
            "true" => true,
            "false" => false,
            _ => return Err(Error::InvalidConfiguration),
        };
        let peer_listen = text("RACER_PEER_LISTEN", Some("0.0.0.0:7443"))?
            .parse()
            .map_err(|_| Error::InvalidConfiguration)?;
        let diagnostics_listen = text("RACER_DIAGNOSTICS_LISTEN", Some("127.0.0.1:9090"))?
            .parse()
            .map_err(|_| Error::InvalidConfiguration)?;
        let trust_bundle = text("RACER_TRUST_BUNDLE", Some("/etc/racer/trust/ca.crt"))?.into();
        let service_account_token = text(
            "RACER_SERVICE_ACCOUNT_TOKEN",
            Some("/var/run/secrets/racer-control/token"),
        )?
        .into();
        let secret_directory = text("RACER_SECRET_DIRECTORY", Some("/etc/racer/keys"))?.into();
        let identity_directory =
            text("RACER_IDENTITY_DIRECTORY", Some("/var/lib/racer/identity"))?.into();
        let slab_directory = text("RACER_SLAB_DIRECTORY", Some("/var/lib/racer/slabs"))?.into();
        let mut number = |name: &str, default: u64| -> Result<u64> {
            let value = text(name, Some(&default.to_string()))?;
            if !value.bytes().all(|b| b.is_ascii_digit()) {
                return Err(Error::InvalidConfiguration);
            }
            value.parse().map_err(|_| Error::InvalidConfiguration)
        };
        let max_threads = to_usize(number("RACER_MAX_THREADS", DEFAULT_MAX_THREADS as u64)?)?;
        let slab_bytes = number("RACER_SLAB_BYTES", 1024 * MIB)?;
        let segment_bytes = number("RACER_SEGMENT_BYTES", 64 * MIB)?;
        let free_segment_reserve = to_usize(number("RACER_FREE_SEGMENT_RESERVE", 2)?)?;
        let request_timeout = Duration::from_millis(number("RACER_REQUEST_TIMEOUT_MS", 30_000)?);
        let reader_stall_timeout =
            Duration::from_millis(number("RACER_READER_STALL_TIMEOUT_MS", 10_000)?);
        let shutdown_timeout = Duration::from_millis(number("RACER_SHUTDOWN_TIMEOUT_MS", 30_000)?);
        let mut limit = |name: &str, default| {
            NonZeroUsize::new(to_usize(number(name, default)?)?).ok_or(Error::InvalidConfiguration)
        };
        let limits = Limits {
            plaintext_bytes: limit("RACER_PLAINTEXT_BYTES", 256 * MIB)?,
            ciphertext_bytes: limit("RACER_CIPHERTEXT_BYTES", 256 * MIB)?,
            dirty_bytes: limit("RACER_DIRTY_BYTES", 128 * MIB)?,
            registered_bytes: limit("RACER_REGISTERED_BYTES", 128 * MIB)?,
            request_context_bytes: limit("RACER_REQUEST_CONTEXT_BYTES", 16 * MIB)?,
            flights: limit("RACER_FLIGHTS", 64)?,
            waiters_per_flight: limit("RACER_WAITERS_PER_FLIGHT", 64)?,
            queue_entries: limit("RACER_QUEUE_ENTRIES", 256)?,
            connections_per_neighbor: limit("RACER_CONNECTIONS_PER_NEIGHBOR", 2)?,
            client_connections: limit("RACER_CLIENT_CONNECTIONS", 128)?,
            pipes: limit("RACER_PIPES", 16)?,
            range_window_pages: limit("RACER_RANGE_WINDOW_PAGES", 2)?,
            replay_entries: limit("RACER_REPLAY_ENTRIES", 4096)?,
            header_bytes: limit("RACER_HEADER_BYTES", 32 * 1024)?,
            route_search_work: limit("RACER_ROUTE_SEARCH_WORK", 4096)?,
            cached_rankings: limit("RACER_CACHED_RANKINGS", 128)?,
            cached_paths: limit("RACER_CACHED_PATHS", 128)?,
            retained_snapshots: limit("RACER_RETAINED_SNAPSHOTS", 2)?,
            metadata_entries: limit("RACER_METADATA_ENTRIES", 4096)?,
            relay_transfers: limit("RACER_RELAY_TRANSFERS", 16)?,
        };
        let config = Self {
            cluster,
            node: NodeId(UNRESOLVED_NODE_ID.into()),
            max_threads,
            enable_rdma,
            control_endpoint,
            peer_listen,
            diagnostics_listen,
            trust_bundle,
            service_account_token,
            secret_directory,
            identity_directory,
            slab_directory,
            slab_bytes,
            segment_bytes,
            free_segment_reserve,
            limits,
            request_timeout,
            reader_stall_timeout,
            shutdown_timeout,
        };
        config.validate()?;
        Ok(config)
    }

    /// Check arithmetic and progress reserves; filesystem alignment is additionally
    /// discovered and checked by store::slab at open, not guessed from this config.
    pub fn validate(&self) -> Result<()> {
        if !valid_uuid(&self.cluster.0)
            || (self.node.0 != UNRESOLVED_NODE_ID && !valid_uuid(&self.node.0))
            || !(2..=256).contains(&self.max_threads)
        {
            return Err(Error::InvalidConfiguration);
        }
        validate_endpoint(&self.control_endpoint)?;
        for address in [self.peer_listen, self.diagnostics_listen] {
            validate_socket(address)?;
        }
        if self.peer_listen.port() == self.diagnostics_listen.port()
            && (self.peer_listen.ip() == self.diagnostics_listen.ip()
                || self.peer_listen.ip().is_unspecified()
                || self.diagnostics_listen.ip().is_unspecified())
        {
            return Err(Error::InvalidConfiguration);
        }
        let paths = [
            &self.trust_bundle,
            &self.service_account_token,
            &self.secret_directory,
            &self.identity_directory,
            &self.slab_directory,
        ];
        for path in paths {
            validate_path(path)?;
        }
        // Writable state must not contain or live inside projected credentials.
        // Lexical checks cannot prove mounts/symlinks are distinct; open-time checks
        // remain mandatory and must allow Kubernetes projection symlinks.
        for (index, left) in paths.iter().enumerate() {
            for right in &paths[index + 1..] {
                if left.starts_with(right) || right.starts_with(left) {
                    return Err(Error::InvalidConfiguration);
                }
            }
        }
        let record_bytes = PAGE_BYTES
            .checked_add(16)
            .and_then(|n| n.checked_add(MAX_HEADER_BYTES as u64))
            .ok_or(Error::InvalidConfiguration)?;
        if self.segment_bytes < record_bytes
            || self.slab_bytes > i64::MAX as u64
            || self.slab_bytes == 0
            || !self.slab_bytes.is_multiple_of(self.segment_bytes)
            || self.free_segment_reserve == 0
        {
            return Err(Error::InvalidConfiguration);
        }
        let segments = to_usize(self.slab_bytes / self.segment_bytes)?;
        if segments > MAX_ENTRIES || self.free_segment_reserve >= segments {
            return Err(Error::InvalidConfiguration);
        }
        let limits = &self.limits;
        let mut bytes = 0u64;
        for limit in [
            limits.plaintext_bytes,
            limits.ciphertext_bytes,
            limits.dirty_bytes,
            limits.registered_bytes,
            limits.request_context_bytes,
        ] {
            let value = limit.get() as u64;
            if value > MAX_BYTES || limit.get() > isize::MAX as usize {
                return Err(Error::InvalidConfiguration);
            }
            bytes = bytes
                .checked_add(value)
                .ok_or(Error::InvalidConfiguration)?;
        }
        // Node-wide budgets are partitioned after affinity discovery. Integration
        // must recheck progress floors per worker and reduce pairs if necessary.
        if bytes > 256 * 1024 * MIB || bytes > isize::MAX as u64 {
            return Err(Error::InvalidConfiguration);
        }
        for (limit, maximum) in [
            (limits.flights, 65_536),
            (limits.waiters_per_flight, 4096),
            (limits.queue_entries, 65_536),
            (limits.connections_per_neighbor, 1024),
            (limits.client_connections, 65_536),
            (limits.pipes, 65_536),
            (limits.range_window_pages, 64),
            (limits.replay_entries, MAX_ENTRIES),
            (limits.header_bytes, 32 * 1024),
            (limits.route_search_work, MAX_ENTRIES),
            (limits.cached_rankings, MAX_ENTRIES),
            (limits.cached_paths, MAX_ENTRIES),
            (limits.retained_snapshots, 64),
            (limits.metadata_entries, MAX_ENTRIES),
            (limits.relay_transfers, 65_536),
        ] {
            if limit.get() > maximum {
                return Err(Error::InvalidConfiguration);
            }
        }
        // Keep one page of acquisition progress beyond a full range window. The
        // ciphertext margin also fits one maximum unpadded storage record.
        let window = limits.range_window_pages.get() as u64;
        let plaintext = (window + 1)
            .checked_mul(PAGE_BYTES)
            .ok_or(Error::InvalidConfiguration)?;
        let ciphertext = window
            .checked_mul(PAGE_BYTES + 16)
            .and_then(|n| n.checked_add(record_bytes))
            .ok_or(Error::InvalidConfiguration)?;
        if (limits.plaintext_bytes.get() as u64) < plaintext
            || (limits.ciphertext_bytes.get() as u64) < ciphertext
            || (limits.dirty_bytes.get() as u64) < PAGE_BYTES + 16
            || (self.enable_rdma && (limits.registered_bytes.get() as u64) < PAGE_BYTES + 16)
            || limits.header_bytes.get() < 1024
            || limits.request_context_bytes.get() < 128 * 1024
            || limits.queue_entries.get() < 2
            || limits.retained_snapshots.get() < 2
            || limits.connections_per_neighbor > limits.client_connections
            || limits
                .flights
                .get()
                .checked_mul(limits.waiters_per_flight.get())
                .is_none_or(|n| n > MAX_ENTRIES)
        {
            return Err(Error::InvalidConfiguration);
        }
        for (timeout, maximum) in [
            (self.request_timeout, Duration::from_secs(86_400)),
            (self.reader_stall_timeout, self.request_timeout),
            (self.shutdown_timeout, Duration::from_secs(3600)),
        ] {
            if timeout < Duration::from_millis(1) || timeout > maximum {
                return Err(Error::InvalidConfiguration);
            }
        }
        Ok(())
    }
}

fn to_usize(value: u64) -> Result<usize> {
    usize::try_from(value).map_err(|_| Error::InvalidConfiguration)
}

fn valid_uuid(value: &str) -> bool {
    value.len() == 36
        && value != "00000000-0000-0000-0000-000000000000"
        && value.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || matches!(b, b'a'..=b'f')
            }
        })
}

fn validate_socket(address: SocketAddr) -> Result<()> {
    if address.port() == 0
        || address.ip().is_multicast()
        || matches!(address.ip(), IpAddr::V4(ip) if ip.is_broadcast())
        || matches!(address, SocketAddr::V6(ip) if ip.flowinfo() != 0 || ip.scope_id() != 0 || ip.ip().to_ipv4_mapped().is_some())
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

fn validate_endpoint(url: &str) -> Result<()> {
    let authority = url
        .strip_prefix("https://")
        .ok_or(Error::InvalidConfiguration)?;
    let authority = authority.strip_suffix('/').unwrap_or(authority);
    if authority.is_empty()
        || authority.len() > 320
        || authority
            .bytes()
            .any(|b| !b.is_ascii_graphic() || b"/?#@\\%".contains(&b))
    {
        return Err(Error::InvalidConfiguration);
    }
    let (host, port) = if let Some(rest) = authority.strip_prefix('[') {
        let (host, rest) = rest.split_once(']').ok_or(Error::InvalidConfiguration)?;
        host.parse::<std::net::Ipv6Addr>()
            .map_err(|_| Error::InvalidConfiguration)?;
        (
            host,
            if rest.is_empty() {
                None
            } else {
                Some(rest.strip_prefix(':').ok_or(Error::InvalidConfiguration)?)
            },
        )
    } else {
        let (host, port) = authority
            .split_once(':')
            .map_or((authority, None), |(h, p)| (h, Some(p)));
        if host.len() > 253
            || host.split('.').any(|label| {
                label.is_empty()
                    || label.len() > 63
                    || label.starts_with('-')
                    || label.ends_with('-')
                    || !label
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b == b'-')
            })
        {
            return Err(Error::InvalidConfiguration);
        }
        (host, port)
    };
    if let Some(port) = port {
        if port.is_empty()
            || !port.bytes().all(|b| b.is_ascii_digit())
            || port.parse::<u16>().ok().is_none_or(|p| p == 0)
        {
            return Err(Error::InvalidConfiguration);
        }
    }
    rustls::pki_types::ServerName::try_from(host).map_err(|_| Error::InvalidConfiguration)?;
    if let Ok(ip) = host.parse::<IpAddr>() {
        validate_socket(SocketAddr::new(ip, 443))?;
        if ip.is_unspecified() {
            return Err(Error::InvalidConfiguration);
        }
    }
    Ok(())
}

fn validate_path(path: &Path) -> Result<()> {
    let text = path.to_str().ok_or(Error::InvalidConfiguration)?;
    if !path.is_absolute()
        || text.len() > 4095
        || text.chars().any(char::is_control)
        || text[1..]
            .split('/')
            .any(|part| part.is_empty() || matches!(part, "." | "..") || part.len() > 255)
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(overrides: &[(&str, &str)]) -> Result<Config> {
        Config::from_lookup(|name| {
            Ok(overrides
                .iter()
                .find(|(key, _)| *key == name)
                .map(|(_, value)| (*value).to_owned())
                .or_else(|| match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
                    _ => None,
                }))
        })
    }

    #[test]
    fn defaults_are_bounded_and_identity_is_unresolved() {
        let config = parse(&[]).unwrap();
        assert_eq!(config.max_threads, 8);
        assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        assert!(!config.enable_rdma);
        assert_eq!(config.slab_bytes / config.segment_bytes, 16);
        assert_eq!(config.free_segment_reserve, 2);
        assert!(config.diagnostics_listen.ip().is_loopback());
        assert_eq!(config.request_timeout, Duration::from_secs(30));
        assert_eq!(config.validate(), Ok(()));
        assert!(parse(&[("RACER_ENABLE_RDMA", "true"), ("RACER_MAX_THREADS", "7")]).is_ok());
    }

    #[test]
    fn required_identity_and_lookup_errors_fail_closed() {
        assert!(matches!(
            Config::from_lookup(|_| Ok(None)),
            Err(Error::InvalidConfiguration)
        ));
        assert!(matches!(
            Config::from_lookup(|_| Err(Error::InvalidConfiguration)),
            Err(Error::InvalidConfiguration)
        ));
        for value in [
            "",
            "node-name",
            "00000000-0000-0000-0000-000000000000",
            "00000000-0000-4000-8000-00000000000A",
        ] {
            assert!(parse(&[("RACER_CLUSTER_ID", value)]).is_err(), "{value}");
        }
        for name in [
            "RACER_NODE_UID",
            "RACER_NODE_ID",
            "RACER_SHARES",
            "RACER_RAILS",
            "RACER_ALIGNED_RAILS",
        ] {
            assert!(parse(&[(name, "")]).is_err(), "{name}");
        }
        let mut config = parse(&[]).unwrap();
        config.node = NodeId("node-name".into());
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.node = NodeId("00000000-0000-4000-8000-000000000002".into());
        assert_eq!(config.validate(), Ok(()));
    }

    #[test]
    fn numeric_and_boolean_parsing_is_strict() {
        for value in [
            "",
            " 8",
            "8 ",
            "+8",
            "-8",
            "8MiB",
            "1.5",
            "18446744073709551616",
            "8\n",
        ] {
            assert!(parse(&[("RACER_MAX_THREADS", value)]).is_err(), "{value:?}");
        }
        for value in ["TRUE", "1", "yes", " false"] {
            assert!(parse(&[("RACER_ENABLE_RDMA", value)]).is_err());
        }
        for value in ["0", "1", "257", "18446744073709551615"] {
            assert!(parse(&[("RACER_MAX_THREADS", value)]).is_err());
        }
    }

    #[test]
    fn endpoint_and_listener_validation_precedes_network_io() {
        for url in [
            "https://control.example",
            "https://localhost:443/",
            "https://127.0.0.1:7443",
            "https://[::1]:7443/",
        ] {
            assert!(parse(&[("RACER_CONTROL_ENDPOINT", url)]).is_ok(), "{url}");
        }
        for url in [
            "http://control.example",
            "https://",
            "https://user@host",
            "https://host/path",
            "https://host//",
            "https://host?x",
            "https://host#x",
            "https://host:0",
            "https://host:65536",
            "https://host:+443",
            "https://bad host",
            "https://host\r\nX: y",
            "https://host\\evil",
            "https://%68ost",
            "https://[::]",
            "https://[::1",
            "https://::1",
            "https://0.0.0.0",
            "https://-host",
            "https://host..example",
        ] {
            assert!(
                parse(&[("RACER_CONTROL_ENDPOINT", url)]).is_err(),
                "{url:?}"
            );
        }
        for address in [
            "localhost:7443",
            "127.0.0.1:0",
            "224.0.0.1:7443",
            "255.255.255.255:7443",
            "[ff02::1]:7443",
            "[::ffff:127.0.0.1]:7443",
        ] {
            assert!(
                parse(&[("RACER_PEER_LISTEN", address)]).is_err(),
                "{address}"
            );
        }
        assert!(parse(&[("RACER_DIAGNOSTICS_LISTEN", "127.0.0.1:7443")]).is_err());
        assert!(parse(&[("RACER_PEER_LISTEN", "[::]:7443")]).is_ok());
    }

    #[test]
    fn paths_are_absolute_canonical_and_separate_without_filesystem_access() {
        for name in [
            "RACER_TRUST_BUNDLE",
            "RACER_SERVICE_ACCOUNT_TOKEN",
            "RACER_SECRET_DIRECTORY",
            "RACER_IDENTITY_DIRECTORY",
            "RACER_SLAB_DIRECTORY",
        ] {
            for path in [
                "",
                "relative/path",
                "/",
                "/safe/../keys",
                "/safe/./keys",
                "/safe//keys",
                "/safe/keys/",
                "/safe/\0keys",
            ] {
                assert!(parse(&[(name, path)]).is_err(), "{name}: {path:?}");
            }
        }
        for path in [
            "/etc/racer/keys",
            "/etc/racer/keys/private",
            "/etc/racer",
            "/var/lib/racer/slabs",
            "/var/lib/racer",
            "/etc/racer/trust/ca.crt/private",
        ] {
            assert!(
                parse(&[("RACER_IDENTITY_DIRECTORY", path)]).is_err(),
                "{path}"
            );
        }
        assert!(
            parse(&[
                ("RACER_IDENTITY_DIRECTORY", "/absent-config-test/identity"),
                ("RACER_SLAB_DIRECTORY", "/absent-config-test/slabs")
            ])
            .is_ok()
        );
        assert!(parse(&[("RACER_TRUST_BUNDLE", &format!("/{}", "a".repeat(256)))]).is_err());
    }

    #[test]
    fn geometry_includes_aead_and_envelope_and_preserves_a_writable_segment() {
        let mut config = parse(&[]).unwrap();
        let minimum = PAGE_BYTES + 16 + MAX_HEADER_BYTES as u64;
        config.segment_bytes = minimum;
        config.slab_bytes = minimum * 3;
        assert_eq!(config.validate(), Ok(()));
        config.segment_bytes -= 1;
        config.slab_bytes = config.segment_bytes * 3;
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        for (name, value) in [
            ("RACER_SEGMENT_BYTES", "0"),
            ("RACER_SLAB_BYTES", "0"),
            ("RACER_SEGMENT_BYTES", "16777216"),
            ("RACER_SLAB_BYTES", "1073741825"),
            ("RACER_FREE_SEGMENT_RESERVE", "0"),
            ("RACER_FREE_SEGMENT_RESERVE", "16"),
            ("RACER_SLAB_BYTES", "18446744073709551615"),
            ("RACER_FREE_SEGMENT_RESERVE", "18446744073709551615"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
    }

    #[test]
    fn budgets_require_page_window_and_control_progress() {
        for (name, value) in [
            ("RACER_PLAINTEXT_BYTES", "16777216"),
            ("RACER_CIPHERTEXT_BYTES", "16777216"),
            ("RACER_DIRTY_BYTES", "16777216"),
            ("RACER_REQUEST_CONTEXT_BYTES", "32768"),
            ("RACER_QUEUE_ENTRIES", "1"),
            ("RACER_RETAINED_SNAPSHOTS", "1"),
            ("RACER_HEADER_BYTES", "32769"),
            ("RACER_HEADER_BYTES", "1023"),
            ("RACER_FLIGHTS", "0"),
            ("RACER_PIPES", "65537"),
            ("RACER_RANGE_WINDOW_PAGES", "16"),
            ("RACER_REPLAY_ENTRIES", "1048577"),
            ("RACER_PLAINTEXT_BYTES", "18446744073709551615"),
            ("RACER_CONNECTIONS_PER_NEIGHBOR", "129"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        assert!(
            parse(&[
                ("RACER_FLIGHTS", "65536"),
                ("RACER_WAITERS_PER_FLIGHT", "4096")
            ])
            .is_err()
        );
        assert!(
            parse(&[
                ("RACER_ENABLE_RDMA", "true"),
                ("RACER_REGISTERED_BYTES", "16777216")
            ])
            .is_err()
        );
        assert!(parse(&[("RACER_REGISTERED_BYTES", "1")]).is_ok());
        assert!(
            parse(&[
                ("RACER_PLAINTEXT_BYTES", "68719476736"),
                ("RACER_CIPHERTEXT_BYTES", "68719476736"),
                ("RACER_DIRTY_BYTES", "68719476736"),
                ("RACER_REGISTERED_BYTES", "68719476736"),
            ])
            .is_err()
        );
        let mut config = parse(&[]).unwrap();
        let window = config.limits.range_window_pages.get();
        config.limits.plaintext_bytes =
            NonZeroUsize::new((window + 1) * PAGE_BYTES as usize).unwrap();
        config.limits.ciphertext_bytes =
            NonZeroUsize::new((window + 1) * (PAGE_BYTES as usize + 16) + MAX_HEADER_BYTES)
                .unwrap();
        assert_eq!(config.validate(), Ok(()));
        config.limits.ciphertext_bytes =
            NonZeroUsize::new(config.limits.ciphertext_bytes.get() - 1).unwrap();
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
    }

    #[test]
    fn timeouts_are_positive_bounded_and_stall_fits_request() {
        for (name, value) in [
            ("RACER_REQUEST_TIMEOUT_MS", "0"),
            ("RACER_REQUEST_TIMEOUT_MS", "86400001"),
            ("RACER_READER_STALL_TIMEOUT_MS", "0"),
            ("RACER_READER_STALL_TIMEOUT_MS", "30001"),
            ("RACER_SHUTDOWN_TIMEOUT_MS", "0"),
            ("RACER_SHUTDOWN_TIMEOUT_MS", "3600001"),
            ("RACER_REQUEST_TIMEOUT_MS", "18446744073709551615"),
        ] {
            assert!(parse(&[(name, value)]).is_err(), "{name}={value}");
        }
        assert!(
            parse(&[
                ("RACER_REQUEST_TIMEOUT_MS", "1"),
                ("RACER_READER_STALL_TIMEOUT_MS", "1"),
                ("RACER_SHUTDOWN_TIMEOUT_MS", "1")
            ])
            .is_ok()
        );
    }
}
