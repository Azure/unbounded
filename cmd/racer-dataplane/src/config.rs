//! Environment and deployment configuration, validated before startup.
//!
//! Default to all eligible physical cores, subject to CPU and resource budgets.
//! Shares and physical NIC bindings come from accepted controller membership.

use crate::error::Error;
use crate::error::Result;
use crate::model::ClusterId;
use crate::config::Limits;
use crate::model::NodeId;
use crate::model::PAGE_BYTES;
use crate::store::MAX_HEADER_BYTES;
use std::net::IpAddr;
use std::net::SocketAddr;
use std::num::NonZeroUsize;
use std::path::Path;
use std::path::PathBuf;
use std::time::Duration;

pub const DEFAULT_MAX_THREADS: usize = usize::MAX;
/// Not a Node UID. Only verified enrollment/local identity recovery may replace it.
pub const UNRESOLVED_NODE_ID: &str = "";
const MIB: u64 = 1024 * 1024;
const MAX_BYTES: u64 = 64 * 1024 * MIB;
const MAX_ENTRIES: usize = 1_048_576;

pub struct Config {
    pub send_crc_pair: Option<crate::telemetry::Pair>,
    pub page_hedge: crate::read::candidates::HedgeConfig,
    pub peer_admission: crate::peer::adaptive::Config,
    pub shares: std::num::NonZeroU32,
    pub disk_page_entries: NonZeroUsize,
    pub checkpoint_bytes: NonZeroUsize,
    pub cluster: ClusterId,
    /// Resolved by verified bootstrap/local identity recovery before workers start,
    /// not from a caller-provided UID or the Downward API's node name.
    /// Environment parsing leaves this unresolved; validation is not authentication.
    pub node: NodeId,
    /// Total thread cap, minimum two; usize::MAX means no configured ceiling.
    /// Control and diagnostics run on I/O threads within this budget.
    pub max_threads: usize,
    /// Opt into logical-CPU sizing instead of one eligible CPU per physical core.
    pub allow_smt: bool,
    pub enable_rdma: bool,
    /// Opt into experimental opaque HTTP transit; materialized relay is the default.
    pub opaque_relay: bool,
    /// Experimental peer-only TCP_NODELAY; false preserves existing socket defaults.
    pub peer_tcp_nodelay: bool,
    pub control_endpoint: String,
    pub peer_listen: std::net::SocketAddr,
    pub diagnostics_listen: std::net::SocketAddr,
    pub trust_bundle: PathBuf,
    pub service_account_token: PathBuf,
    /// Node-private persistent keys, separate from projected Secrets and slabs.
    pub identity_directory: PathBuf,
    pub slab_directory: PathBuf,
    pub slab_bytes: u64,
    pub segment_bytes: u64,
    pub free_segment_reserve: usize,
    pub limits: Limits,
    /// Per-cache, per-worker origin HTTP cap, separate from peer connections.
    pub origin_connections_per_cache: NonZeroUsize,
    pub request_timeout: Duration,
    /// Nonrenewable local peer-exchange cap; never serialized as authority.
    pub peer_attempt_timeout: Duration,
    pub reader_stall_timeout: Duration,
    pub shutdown_timeout: Duration,
}

impl Config {
    pub fn from_env() -> Result<Self> {
        Self::from_lookup(env_value)
    }

    /// Injectable lookup keeps parser tests independent of the process environment.
    pub fn from_lookup(mut lookup: impl FnMut(&str) -> Result<Option<String>>) -> Result<Self> {
        // Fail closed on common obsolete authority overrides, even if empty.
        for name in [
            "RACER_NODE_UID",
            "RACER_NODE_ID",
            "RACER_RAILS",
            "RACER_ALIGNED_RAILS",
            "RACER_FABRIC_PORTS",
            "RACER_FABRIC_PORTS_FILE",
        ] {
            if lookup(name)?.is_some() {
                return Err(Error::InvalidConfiguration);
            }
        }
        // Removed selectors must fail explicitly, never silently change topology.
        if lookup("RACER_ROUTING_ALGORITHM")?.is_some() {
            return Err(Error::InvalidConfiguration);
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
        let enable_rdma = match text("RACER_ENABLE_RDMA", Some("auto"))?.as_str() {
            // Auto reserves native resources only with usable startup hardware.
            // Explicit true reserves them even when absent, enabling hotplug
            // recovery without resizing live workers or stealing HTTP budgets.
            "auto" => {
                cfg!(feature = "rdma")
                    && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
            }
            "true" => true,
            "false" => false,
            _ => return Err(Error::InvalidConfiguration),
        };
        let mut boolean = |name| {
            text(name, Some("false"))?
                .parse::<bool>()
                .map_err(|_| Error::InvalidConfiguration)
        };
        let allow_smt = boolean("RACER_ALLOW_SMT")?;
        let opaque_relay = boolean("RACER_OPAQUE_RELAY")?;
        let peer_tcp_nodelay = boolean("RACER_PEER_TCP_NODELAY")?;
        let peer_listen =
            parse_listener_address(&text("RACER_PEER_LISTEN", Some("0.0.0.0:7443"))?)?;
        let diagnostics_listen =
            parse_listener_address(&text("RACER_DIAGNOSTICS_LISTEN", Some("127.0.0.1:9090"))?)?;
        let trust_bundle = text("RACER_TRUST_BUNDLE", Some("/etc/racer/trust/ca.crt"))?.into();
        let service_account_token = text(
            "RACER_SERVICE_ACCOUNT_TOKEN",
            Some("/var/run/secrets/racer-control/token"),
        )?
        .into();
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
        let page_hedge = crate::read::candidates::HedgeConfig {
            delay: Duration::from_millis(number("RACER_PAGE_HEDGE_DELAY_MS", 100)?),
            slots: to_usize(number("RACER_PAGE_HEDGE_SLOTS", 0)?)?,
            bytes: to_usize(number(
                "RACER_PAGE_HEDGE_BYTES",
                crate::read::candidates::DUPLICATE_BYTES as u64,
            )?)?,
        };
        let peer_admission = crate::peer::adaptive::Config {
            total: to_usize(number("RACER_PEER_INFLIGHT_MAX", 256)?)?,
            per_peer: to_usize(number("RACER_PEER_PER_NEIGHBOR_MAX", 32)?)?,
        };
        let shares = std::num::NonZeroU32::new(
            u32::try_from(number("RACER_SHARES", 4)?).map_err(|_| Error::InvalidConfiguration)?,
        )
        .ok_or(Error::InvalidConfiguration)?;
        let slab_bytes = number("RACER_SLAB_BYTES", 1024 * MIB)?;
        let segment_bytes = number("RACER_SEGMENT_BYTES", 64 * MIB)?;
        let free_segment_reserve = to_usize(number("RACER_FREE_SEGMENT_RESERVE", 2)?)?;
        let request_timeout = Duration::from_millis(number("RACER_REQUEST_TIMEOUT_MS", 30_000)?);
        let peer_attempt_timeout =
            Duration::from_millis(number("RACER_PEER_ATTEMPT_TIMEOUT_MS", 30_000)?);
        let reader_stall_timeout =
            Duration::from_millis(number("RACER_READER_STALL_TIMEOUT_MS", 10_000)?);
        let shutdown_timeout = Duration::from_millis(number("RACER_SHUTDOWN_TIMEOUT_MS", 30_000)?);
        let ranking_bytes = number("RACER_PLACEMENT_CACHE_BYTES", 16 * MIB)?;
        if ranking_bytes < crate::topology::RANKING_BYTES as u64
            || ranking_bytes > 512 * MIB
        {
            return Err(Error::InvalidConfiguration);
        }
        let ranking_entries = ranking_bytes / crate::topology::RANKING_BYTES as u64;
        let mut limit = |name: &str, default| {
            NonZeroUsize::new(to_usize(number(name, default)?)?).ok_or(Error::InvalidConfiguration)
        };
        let limits = Limits {
            plaintext_bytes: limit("RACER_PLAINTEXT_BYTES", 256 * MIB)?,
            ciphertext_bytes: limit("RACER_CIPHERTEXT_BYTES", 256 * MIB)?,
            dirty_bytes: limit("RACER_DIRTY_BYTES", 128 * MIB)?,
            registered_bytes: limit("RACER_REGISTERED_BYTES", 128 * MIB)?,
            request_context_bytes: limit("RACER_REQUEST_CONTEXT_BYTES", 64 * MIB)?,
            flights: limit("RACER_FLIGHTS", 64)?,
            waiters_per_flight: limit("RACER_WAITERS_PER_FLIGHT", 64)?,
            queue_entries: limit("RACER_QUEUE_ENTRIES", 256)?,
            connections_per_neighbor: limit("RACER_CONNECTIONS_PER_NEIGHBOR", 2)?,
            client_connections: limit("RACER_CLIENT_CONNECTIONS", 128)?,
            pipes: limit("RACER_PIPES", 16)?,
            range_window_pages: limit("RACER_RANGE_WINDOW_PAGES", 2)?,
            header_bytes: limit("RACER_HEADER_BYTES", 32 * 1024)?,
            cached_rankings: limit("RACER_CACHED_RANKINGS", ranking_entries)?,
            cached_paths: limit("RACER_CACHED_PATHS", 128)?,
            retained_snapshots: limit("RACER_RETAINED_SNAPSHOTS", 2)?,
            metadata_entries: limit("RACER_METADATA_ENTRIES", 4096)?,
            relay_transfers: limit("RACER_RELAY_TRANSFERS", 16)?,
        };
        let origin_connections_per_cache = limit("RACER_ORIGIN_CONNECTIONS_PER_CACHE", 8)?;
        let disk_page_entries = limit("RACER_DISK_PAGE_ENTRIES", 65536)?;
        let checkpoint_bytes = limit("RACER_CHECKPOINT_BYTES", 64 * MIB)?;
        let send_crc_pair = lookup("RACER_SEND_CRC_PAIR")?
            .map(|value| crate::telemetry::Pair::parse(&value))
            .transpose()?;
        let config = Self {
            send_crc_pair,
            page_hedge,
            peer_admission,
            shares,
            disk_page_entries,
            checkpoint_bytes,
            cluster,
            node: NodeId(UNRESOLVED_NODE_ID.into()),
            max_threads,
            allow_smt,
            enable_rdma,
            opaque_relay,
            peer_tcp_nodelay,
            control_endpoint,
            peer_listen,
            diagnostics_listen,
            trust_bundle,
            service_account_token,
            identity_directory,
            slab_directory,
            slab_bytes,
            segment_bytes,
            free_segment_reserve,
            limits,
            origin_connections_per_cache,
            request_timeout,
            peer_attempt_timeout,
            reader_stall_timeout,
            shutdown_timeout,
        };
        config.validate()?;
        Ok(config)
    }

    /// Check arithmetic and progress reserves; filesystem alignment is additionally
    /// discovered and checked when opening the slabs, not guessed from this config.
    pub fn validate(&self) -> Result<()> {
        if let Some(pair) = &self.send_crc_pair {
            pair.validate()?;
        }
        self.page_hedge.validate()?;
        self.peer_admission.validate()?;
        if self.disk_page_entries.get() > MAX_ENTRIES
            || self.checkpoint_bytes.get() > 512 * MIB as usize
        {
            return Err(Error::InvalidConfiguration);
        }
        if !valid_uuid(&self.cluster.0)
            || (self.node.0 != UNRESOLVED_NODE_ID && !valid_uuid(&self.node.0))
            || self.max_threads < 2
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
        if self.origin_connections_per_cache.get() > 1024
            || self.origin_connections_per_cache > limits.client_connections
        {
            return Err(Error::InvalidConfiguration);
        }
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
        // must recheck progress floors per I/O shard and reduce workers if necessary.
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
            (limits.header_bytes, 32 * 1024),
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
            (self.peer_attempt_timeout, Duration::from_secs(86_400)),
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

fn env_value(name: &str) -> Result<Option<String>> {
    match std::env::var(name) {
        Ok(value) => Ok(Some(value)),
        Err(std::env::VarError::NotPresent) => Ok(None),
        Err(std::env::VarError::NotUnicode(_)) => Err(Error::InvalidConfiguration),
    }
}

fn to_usize(value: u64) -> Result<usize> {
    usize::try_from(value).map_err(|_| Error::InvalidConfiguration)
}

fn valid_uuid(value: &str) -> bool {
    racer_identity::canonical_uuid(value) && value != "00000000-0000-0000-0000-000000000000"
}

// Kubernetes expands a single bracketed Pod IP template for both IP families.
// SocketAddr already accepts bracketed IPv6; normalize only bracketed IPv4 here.
fn parse_listener_address(value: &str) -> Result<SocketAddr> {
    if let Some((host, port)) = value.strip_prefix('[').and_then(|v| v.split_once("]:"))
        && let Ok(ip) = host.parse::<std::net::Ipv4Addr>()
    {
        return format!("{ip}:{port}")
            .parse()
            .map_err(|_| Error::InvalidConfiguration);
    }
    value.parse().map_err(|_| Error::InvalidConfiguration)
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
    use std::io::Read;

    #[test]
    fn configured_ports_do_not_override_membership_or_discovered_hardware() {
        use crate::rdma::match_publication;
        use racer_control_wire::RailId;
        use racer_control_wire::RailMapping;
        let nic = RailMapping {
            device: "a".into(),
            port: 1,
            rail: RailId(7),
            gid: Some([1; 16]),
            numa_node: Some(9),
        };
        let live = rdma_verbs::PortInfo {
            device: "a".into(),
            port: 1,
            gid: [1; 16],
            numa_node: Some(0),
        };
        assert_eq!(
            match_publication(&[nic.clone()], &[live.clone()]).unwrap()[0]
                .0
                .numa_node,
            Some(9)
        );
        for bad in [
            rdma_verbs::PortInfo {
                device: "b".into(),
                ..live.clone()
            },
            rdma_verbs::PortInfo {
                port: 2,
                ..live.clone()
            },
            rdma_verbs::PortInfo {
                gid: [2; 16],
                ..live.clone()
            },
        ] {
            assert!(match_publication(&[nic.clone()], &[bad]).is_err());
        }
        assert!(match_publication(&[nic.clone()], &[live.clone(), live.clone()]).is_err());
        assert!(match_publication(&[nic.clone(), nic.clone()], &[live]).is_err());
        assert!(match_publication(&[nic], &[]).is_err());
    }

    // Retain the retired loader safety tests under their historical names. The
    // contract now lives in authenticated wire NICs, not a second local parser.
    fn nic_request(nics: &str) -> Vec<u8> {
        String::from_utf8(
            include_bytes!("../../../internal/racer/wire/testdata/bootstrap-request.json").to_vec(),
        )
        .unwrap()
        .replace("\"rdma_nics\":[]", &format!("\"rdma_nics\":{nics}"))
        .into_bytes()
    }
    #[test]
    fn fabric_ports_reject_malformed_ambiguous_or_unsafe_configuration() {
        for nics in [
            "null",
            "{}",
            "[null]",
            r#"[{"device":"a","port":0,"rail":0}]"#,
            r#"[{"device":"a","port":1,"rail":0,"gid":""}]"#,
            r#"[{"device":"a","port":1,"rail":0,"gid":null}]"#,
            r#"[{"device":"a","port":1,"port":2,"rail":0}]"#,
            r#"[{"device":"a","port":1,"rail":0,"fabric":"old"}]"#,
            r#"[{"device":"a","port":1,"rail":0},{"device":"a","port":1,"rail":1}]"#,
        ] {
            assert!(
                racer_control_wire::decode_enrollment_request(&nic_request(nics)).is_err(),
                "{nics}"
            );
        }
    }
    #[test]
    fn projected_fabric_configuration_is_bounded_and_errors_fail_closed() {
        let entries: Vec<_> = (0..65)
            .map(|i| format!(r#"{{"device":"d{i}","port":1,"rail":0}}"#))
            .collect();
        assert!(
            racer_control_wire::decode_enrollment_request(&nic_request(&format!(
                "[{}]",
                entries[..64].join(",")
            )))
            .is_ok()
        );
        assert!(
            racer_control_wire::decode_enrollment_request(&nic_request(&format!(
                "[{}]",
                entries.join(",")
            )))
            .is_err()
        );
        assert!(
            racer_control_wire::decode_enrollment_request(&vec![
                b' ';
                racer_control_wire::MAX_ENROLLMENT_BYTES
                    + 1
            ])
            .is_err()
        );
        for name in ["RACER_FABRIC_PORTS", "RACER_FABRIC_PORTS_FILE"] {
            assert!(parse(&[(name, "[]")]).is_err());
        }
    }
    #[test]
    fn fabric_loader_is_selected_once_and_its_output_is_validated() {
        assert!(Config::from_lookup(|_| Err(Error::InvalidConfiguration)).is_err());
        for value in ["", "/missing", "[]", "not-json"] {
            assert!(parse(&[("RACER_FABRIC_PORTS_FILE", value)]).is_err());
            assert!(parse(&[("RACER_FABRIC_PORTS", value)]).is_err());
        }
        assert!(
            racer_control_wire::decode_enrollment_request(&nic_request(
                r#"[{"device":"a","port":1,"rail":0}]"#
            ))
            .is_ok()
        );
    }
    #[test]
    fn auto_without_ports_preserves_http_budget_explicit_true_reserves_hotplug() {
        let sim = rdma_verbs::simulation::Simulation::new()
            .with_devices(vec![])
            .unwrap();
        let _environment = sim.enter();
        assert!(
            !parse(&[("RACER_REGISTERED_BYTES", "1")])
                .unwrap()
                .enable_rdma
        );
        assert!(
            parse(&[
                ("RACER_ENABLE_RDMA", "true"),
                ("RACER_REGISTERED_BYTES", "1")
            ])
            .is_err()
        );
        assert!(parse(&[("RACER_ENABLE_RDMA", "true")]).unwrap().enable_rdma);
    }

    fn lookup(name: &str) -> Result<Option<String>> {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
            _ => None,
        })
    }

    #[test]
    fn obsolete_nic_authority_configuration_fails_even_when_disabled() {
        for key in [
            "RACER_FABRIC_PORTS",
            "RACER_FABRIC_PORTS_FILE",
            "RACER_RAILS",
            "RACER_ALIGNED_RAILS",
        ] {
            for value in ["", "[]", "anything"] {
                assert!(parse(&[(key, value), ("RACER_ENABLE_RDMA", "false")]).is_err());
            }
        }
    }

    #[test]
    fn native_auto_requires_build_and_available_library() {
        for requested in [None, Some("auto"), Some("false")] {
            let config = Config::from_lookup(|name| {
                Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    "RACER_ENABLE_RDMA" => requested.map(str::to_owned),
                    _ => None,
                })
            })
            .unwrap();
            assert_eq!(
                config.enable_rdma,
                requested != Some("false")
                    && cfg!(feature = "rdma")
                    && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
            );
        }
    }

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
    fn send_crc_pair_is_opt_in_and_validated() {
        assert!(parse(&[]).unwrap().send_crc_pair.is_none());
        let valid = "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111";
        assert!(
            parse(&[("RACER_SEND_CRC_PAIR", valid)])
                .unwrap()
                .send_crc_pair
                .is_some()
        );
        for invalid in [
            "",
            "a,b",
            "a,b,c",
            "8816d91d-e896-49bf-ba8a-da97ede93818,8816d91d-e896-49bf-ba8a-da97ede93818",
        ] {
            assert!(parse(&[("RACER_SEND_CRC_PAIR", invalid)]).is_err());
        }
    }

    #[test]
    fn defaults_are_bounded_and_identity_is_unresolved() {
        let config = parse(&[]).unwrap();
        assert_eq!(config.max_threads, usize::MAX);
        assert!(!config.allow_smt);
        assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        assert_eq!(
            config.enable_rdma,
            cfg!(feature = "rdma") && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
        );
        assert_eq!(config.slab_bytes / config.segment_bytes, 16);
        assert!(!config.opaque_relay);
        assert_eq!(config.free_segment_reserve, 2);
        assert!(config.diagnostics_listen.ip().is_loopback());
        assert_eq!(config.request_timeout, Duration::from_secs(30));
        assert_eq!(config.peer_attempt_timeout, Duration::from_secs(30));
        assert_eq!(config.origin_connections_per_cache.get(), 8);
        assert_eq!(config.limits.connections_per_neighbor.get(), 2);
        assert_eq!(config.validate(), Ok(()));
        assert!(parse(&[("RACER_ENABLE_RDMA", "true"), ("RACER_MAX_THREADS", "7")]).is_ok());
    }

    #[test]
    fn peer_admission_environment_is_bounded_and_node_scoped() {
        let config = parse(&[
            ("RACER_PEER_INFLIGHT_MAX", "8"),
            ("RACER_PEER_PER_NEIGHBOR_MAX", "2"),
        ])
        .unwrap();
        assert_eq!(config.peer_admission.total, 8);
        assert_eq!(config.peer_admission.per_peer, 2);
        for overrides in [
            vec![("RACER_PEER_INFLIGHT_MAX", "0")],
            vec![("RACER_PEER_INFLIGHT_MAX", "65537")],
            vec![("RACER_PEER_PER_NEIGHBOR_MAX", "0")],
            vec![("RACER_PEER_PER_NEIGHBOR_MAX", "257")],
        ] {
            assert!(parse(&overrides).is_err());
        }
    }
    #[test]
    fn page_hedges_are_off_by_default_and_require_bounded_capacity() {
        assert_eq!(parse(&[]).unwrap().page_hedge.slots, 0);
        assert!(parse(&[("RACER_PAGE_HEDGE_SLOTS", "1")]).is_ok());
        for pairs in [
            vec![("RACER_PAGE_HEDGE_SLOTS", "33")],
            vec![("RACER_PAGE_HEDGE_DELAY_MS", "0")],
            vec![
                ("RACER_PAGE_HEDGE_SLOTS", "1"),
                ("RACER_PAGE_HEDGE_BYTES", "1"),
            ],
        ] {
            assert!(parse(&pairs).is_err());
        }
    }

    fn exact_boolean(name: &str, field: fn(&Config) -> bool) {
        assert!(!field(&Config::from_lookup(lookup).unwrap()));
        for (value, expected) in [("true", true), ("false", false)] {
            let config = Config::from_lookup(|key| {
                if key == name {
                    Ok(Some(value.into()))
                } else {
                    lookup(key)
                }
            })
            .unwrap();
            assert_eq!(field(&config), expected);
            assert_eq!(field(&parse(&[(name, value)]).unwrap()), expected);
        }
        for value in [
            "", "1", "0", "TRUE", "False", "auto", " true", "true ", "true\n",
        ] {
            assert!(matches!(
                parse(&[(name, value)]),
                Err(Error::InvalidConfiguration)
            ));
        }
        assert!(
            Config::from_lookup(|key| {
                if key == name {
                    Err(Error::InvalidConfiguration)
                } else {
                    lookup(key)
                }
            })
            .is_err()
        );
        assert!(
            Config::from_lookup(|key| {
                if key == name {
                    Err(Error::InvalidConfiguration)
                } else {
                    lookup(key)
                }
            })
            .is_err()
        );
    }
    #[test]
    fn peer_tcp_nodelay_requires_exact_boolean_and_defaults_off() {
        exact_boolean("RACER_PEER_TCP_NODELAY", |c| c.peer_tcp_nodelay);
    }

    #[test]
    fn opaque_relay_requires_an_exact_explicit_boolean() {
        exact_boolean("RACER_OPAQUE_RELAY", |c| c.opaque_relay);
    }

    #[test]
    fn routing_algorithm_selector_is_rejected() {
        assert!(parse(&[]).is_ok());
        for value in ["", "1", "2", "3", "4", "5", "6", "03", " 3", "3 ", "auto"] {
            assert!(parse(&[("RACER_ROUTING_ALGORITHM", value)]).is_err());
        }
    }

    #[test]
    fn smt_requires_an_exact_explicit_boolean() {
        exact_boolean("RACER_ALLOW_SMT", |c| c.allow_smt);
    }

    #[test]
    fn origin_connection_cap_is_independent_and_validated() {
        let config = parse(&[
            ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "16"),
            ("RACER_CONNECTIONS_PER_NEIGHBOR", "1"),
        ])
        .unwrap();
        assert_eq!(config.origin_connections_per_cache.get(), 16);
        assert_eq!(config.limits.connections_per_neighbor.get(), 1);
        for value in ["", "0", "1025", "129", "-1", "1 ", "many"] {
            assert!(
                parse(&[("RACER_ORIGIN_CONNECTIONS_PER_CACHE", value)]).is_err(),
                "{value}"
            );
        }
        assert!(
            parse(&[
                ("RACER_ORIGIN_CONNECTIONS_PER_CACHE", "1024"),
                ("RACER_CLIENT_CONNECTIONS", "1024")
            ])
            .is_ok()
        );
    }

    #[test]
    fn diagnostics_accepts_expanded_pod_ip_of_either_family() {
        for (address, expected) in [
            ("192.0.2.1:9090", "192.0.2.1:9090"),
            ("[192.0.2.1]:9090", "192.0.2.1:9090"),
            ("[2001:db8::1]:9090", "[2001:db8::1]:9090"),
        ] {
            let config = parse(&[("RACER_DIAGNOSTICS_LISTEN", address)]).unwrap();
            assert_eq!(config.diagnostics_listen, expected.parse().unwrap());
            assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        }
        for address in [
            "[$(RACER_POD_IP)]:9090",
            "[]:9090",
            "[localhost]:9090",
            "[192.0.2.1]:0",
            "[192.0.2.1]:65536",
            "[192.0.2.1]:+9090",
            "[224.0.0.1]:9090",
            "[255.255.255.255]:9090",
            "[::ffff:192.0.2.1]:9090",
            "[fe80::1%eth0]:9090",
            "[ff02::1]:9090",
            "[192.0.2.1]:7443",
        ] {
            assert!(
                parse(&[("RACER_DIAGNOSTICS_LISTEN", address)]).is_err(),
                "{address}"
            );
        }
        assert!(
            parse(&[
                ("RACER_PEER_LISTEN", "0.0.0.0:9090"),
                ("RACER_DIAGNOSTICS_LISTEN", "[192.0.2.1]:9091"),
            ])
            .is_ok()
        );
    }

    #[test]
    fn peer_accepts_expanded_pod_ip_of_either_family() {
        for (address, expected) in [
            ("192.0.2.1:7443", "192.0.2.1:7443"),
            ("[192.0.2.1]:7443", "192.0.2.1:7443"),
            ("[2001:db8::1]:7443", "[2001:db8::1]:7443"),
            ("[192.0.2.1]:65535", "192.0.2.1:65535"),
        ] {
            let config = parse(&[("RACER_PEER_LISTEN", address)]).unwrap();
            assert_eq!(config.peer_listen, expected.parse().unwrap());
            assert_eq!(config.node.0, UNRESOLVED_NODE_ID);
        }
        assert_eq!(
            parse(&[]).unwrap().peer_listen,
            "0.0.0.0:7443".parse().unwrap()
        );
        for address in [
            "[$(RACER_POD_IP)]:7443",
            "[]:7443",
            "[localhost]:7443",
            "[192.0.2.1]:0",
            "[192.0.2.1]:65536",
            "[192.0.2.1]:+7443",
            "[192.0.2.1]:7443extra",
            "[[192.0.2.1]]:7443",
            "[224.0.0.1]:7443",
            "[255.255.255.255]:7443",
            "[::ffff:192.0.2.1]:7443",
            "[fe80::1%eth0]:7443",
            "[ff02::1]:7443",
            "2001:db8::1:7443",
        ] {
            assert!(
                parse(&[("RACER_PEER_LISTEN", address)]).is_err(),
                "{address}"
            );
        }
        for ip in ["192.0.2.1", "2001:db8::1"] {
            let peer = format!("[{ip}]:9090");
            let diagnostics = format!("[{ip}]:9091");
            assert!(
                parse(&[
                    ("RACER_PEER_LISTEN", &peer),
                    ("RACER_DIAGNOSTICS_LISTEN", &diagnostics),
                ])
                .is_ok()
            );
            assert!(
                parse(&[
                    ("RACER_PEER_LISTEN", &peer),
                    ("RACER_DIAGNOSTICS_LISTEN", &peer),
                ])
                .is_err()
            );
        }
    }

    fn peer_listener_round_trip(ip: &str) {
        use std::io::Write;
        use std::net::TcpListener;
        use std::net::TcpStream;

        let address = format!("[{ip}]:7443");
        let mut config = parse(&[("RACER_PEER_LISTEN", &address)]).unwrap();
        assert_eq!(config.peer_listen.ip(), ip.parse::<IpAddr>().unwrap());
        // Production rejects port zero. Allocate an ephemeral test port after
        // configuration validation, using the same exact bind as PeerServer.
        config.peer_listen.set_port(0);
        let listener = TcpListener::bind(config.peer_listen).unwrap();
        let bound = listener.local_addr().unwrap();
        assert_eq!(bound.ip(), config.peer_listen.ip());
        let mut client = TcpStream::connect_timeout(&bound, Duration::from_secs(2)).unwrap();
        client
            .set_write_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        client.write_all(b"peer").unwrap();
        listener.set_nonblocking(true).unwrap();
        let (mut accepted, _) = listener.accept().unwrap();
        assert_eq!(accepted.local_addr().unwrap(), bound);
        accepted
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let mut body = [0; 4];
        accepted.read_exact(&mut body).unwrap();
        assert_eq!(&body, b"peer");
    }

    #[test]
    fn peer_listener_binds_exact_ipv4() {
        peer_listener_round_trip("127.0.0.1");
    }

    #[test]
    fn peer_listener_binds_exact_ipv6() {
        peer_listener_round_trip("::1");
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
        assert_eq!(parse(&[]).unwrap().shares.get(), 4);
        assert_eq!(parse(&[("RACER_SHARES", "9")]).unwrap().shares.get(), 9);
        for value in ["0", "-1", "+4", "4294967296", "4.0"] {
            assert!(parse(&[("RACER_SHARES", value)]).is_err());
        }
        let config = parse(&[
            ("RACER_METADATA_ENTRIES", "32"),
            ("RACER_DISK_PAGE_ENTRIES", "8192"),
        ])
        .unwrap();
        assert_eq!(config.disk_page_entries.get(), 8192);
        assert_eq!(config.limits.metadata_entries.get(), 32);
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
        for value in ["0", "1"] {
            assert!(parse(&[("RACER_MAX_THREADS", value)]).is_err());
        }
        for value in [2, 7, 257, usize::MAX] {
            assert_eq!(
                parse(&[("RACER_MAX_THREADS", &value.to_string())])
                    .unwrap()
                    .max_threads,
                value
            );
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
            "/etc/racer/trust/ca.crt",
            "/etc/racer/trust",
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
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "0"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "86400001"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "18446744073709551615"),
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

    #[test]
    fn peer_attempt_timeout_is_independent_and_validates_programmatic_config() {
        let mut config = parse(&[
            ("RACER_REQUEST_TIMEOUT_MS", "10"),
            ("RACER_READER_STALL_TIMEOUT_MS", "1"),
            ("RACER_PEER_ATTEMPT_TIMEOUT_MS", "6000"),
        ])
        .unwrap();
        assert_eq!(config.peer_attempt_timeout, Duration::from_secs(6));
        assert_eq!(config.validate(), Ok(()));
        config.peer_attempt_timeout = Duration::ZERO;
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
        config.peer_attempt_timeout = Duration::from_millis(86_400_001);
        assert_eq!(config.validate(), Err(Error::InvalidConfiguration));
    }
}
