//! Runtime configuration. The dataplane builds it from its environment table
//! (`racer::config`), so this crate never reads the process environment.

use crate::Error;
use std::{
    net::{IpAddr, SocketAddr},
    path::PathBuf,
    time::Duration,
};

/// How many seats to run.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SeatPolicy {
    /// `clamp(ceil(members / 2048), 3, 16)`, limited by eligible Pods.
    Auto,

    /// A fixed count from 1 to 16.
    Fixed(usize),
}

/// Cache key and issuer root rotation timing, and page key retention.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Rotation {
    pub interval: Duration,

    pub prepare_for: Duration,

    /// Readable page keys per cache, in `1..=`[`racer_wire::control::MAX_RETAINED_PAGE_KEYS`].
    pub retained_page_keys: usize,
}

impl Default for Rotation {
    fn default() -> Self {
        Self {
            interval: Duration::from_secs(24 * 3600),
            prepare_for: Duration::from_secs(3600),
            retained_page_keys: crate::rules::keyring::DEFAULT_RETAINED_PAGE_KEYS,
        }
    }
}

/// Runtime configuration, built by the dataplane's configuration table.
#[derive(Clone, Debug)]
pub struct Config {
    pub namespace: String,

    pub pod_name: String,

    pub pod_uid: String,

    pub node_name: String,

    pub pod_ip: IpAddr,

    pub seats: SeatPolicy,

    pub control_listen: SocketAddr,

    pub webhook_listen: SocketAddr,

    /// Pod IP of the other address family, from the Pod's IPs, served on
    /// the webhook port too. A dual-stack Pod's `status.podIP` family need
    /// not match the primary family of the `racer-webhook` Service, which
    /// is the only family the API server reaches through its ClusterIP.
    /// Set only when the webhook listens on `pod_ip`.
    pub webhook_alternate: Option<IpAddr>,

    /// Image injected as the object sidecar. Webhook serving is off when unset.
    pub object_image: Option<String>,

    /// Kubernetes API `host:port`.
    pub api_server: String,

    pub api_ca: PathBuf,

    /// Audience-default service account token, used only for the Kubernetes API.
    pub api_token: PathBuf,

    /// `racer-control` audience token, sent only to Racer seats.
    pub control_token: PathBuf,

    pub daemonset: String,

    pub service_account: String,

    /// Dataplane peer port published in member endpoints.
    pub peer_port: u16,

    pub rotation: Rotation,

    /// Cluster-wide page size published to every node.
    pub page_size: racer_wire::object::PageSize,
}

impl Config {
    /// Check names and identities.
    pub fn validate(&self) -> Result<(), Error> {
        let label = |v: &str| {
            !v.is_empty()
                && v.len() <= 253
                && v.bytes()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-' || b == b'.')
        };
        if !label(&self.namespace)
            || !label(&self.pod_name)
            || !label(&self.daemonset)
            || !label(&self.service_account)
            || self.node_name.is_empty()
            || !racer_wire::control::valid_uuid(&self.pod_uid)
            || (self.control_listen.port() != 0
                && self.control_listen.port() == self.webhook_listen.port())
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
