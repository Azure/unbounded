//! Environment configuration.

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

/// Cache key and issuer root rotation timing.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Rotation {
    pub interval: Duration,

    pub prepare_for: Duration,
}

impl Default for Rotation {
    fn default() -> Self {
        Self {
            interval: Duration::from_secs(24 * 3600),
            prepare_for: Duration::from_secs(3600),
        }
    }
}

/// Runtime configuration, normally read by [`Config::from_env`].
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
}

impl Config {
    /// Read the environment. Returns `None` outside Kubernetes, when
    /// `RACER_NAMESPACE` is unset, so standalone fixtures run without control.
    pub fn from_env() -> Result<Option<Self>, Error> {
        Self::from_lookup(|name| std::env::var(name).ok())
    }

    /// Read configuration from any lookup function.
    pub fn from_lookup(get: impl Fn(&str) -> Option<String>) -> Result<Option<Self>, Error> {
        let Some(namespace) = get("RACER_NAMESPACE").filter(|v| !v.is_empty()) else {
            return Ok(None);
        };
        let need = |name: &str| {
            get(name)
                .filter(|v| !v.is_empty())
                .ok_or(Error::InvalidConfiguration)
        };
        let pod_name = need("RACER_POD_NAME")?;
        let pod_uid = need("RACER_POD_UID")?;
        let node_name = need("RACER_NODE_NAME")?;
        let pod_ip: IpAddr = need("RACER_POD_IP")?
            .parse()
            .map_err(|_| Error::InvalidConfiguration)?;
        let seats = match get("RACER_CONTROL_SEATS").as_deref() {
            None | Some("") | Some("auto") => SeatPolicy::Auto,
            Some(v) => match v.parse::<usize>() {
                Ok(n) if (1..=crate::annotations::MAX_SEATS).contains(&n) && v == n.to_string() => {
                    SeatPolicy::Fixed(n)
                }
                _ => return Err(Error::InvalidConfiguration),
            },
        };
        let address = |name: &str, default: &str| -> Result<SocketAddr, Error> {
            let v = get(name)
                .filter(|v| !v.is_empty())
                .unwrap_or_else(|| default.into());
            let v = if v.starts_with(':') {
                format!("0.0.0.0{v}")
            } else {
                v
            };
            if let Ok(address) = v.parse() {
                return Ok(address);
            }
            let (host, port) = v
                .strip_prefix('[')
                .and_then(|v| v.split_once("]:"))
                .ok_or(Error::InvalidConfiguration)?;
            if port.is_empty() || !port.bytes().all(|b| b.is_ascii_digit()) {
                return Err(Error::InvalidConfiguration);
            }
            Ok(SocketAddr::new(
                host.parse().map_err(|_| Error::InvalidConfiguration)?,
                port.parse().map_err(|_| Error::InvalidConfiguration)?,
            ))
        };
        let host = need("KUBERNETES_SERVICE_HOST")?;
        let port = get("KUBERNETES_SERVICE_PORT")
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| "443".into());
        port.parse::<u16>()
            .map_err(|_| Error::InvalidConfiguration)?;
        let api_server = if host.contains(':') {
            format!("[{host}]:{port}")
        } else {
            format!("{host}:{port}")
        };
        let peer_port = get("RACER_PEER_PORT")
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| "8082".into())
            .parse::<u16>()
            .ok()
            .filter(|p| *p != 0)
            .ok_or(Error::InvalidConfiguration)?;
        let sa = "/var/run/secrets/kubernetes.io/serviceaccount";
        let config = Self {
            namespace,
            pod_name,
            pod_uid,
            node_name,
            pod_ip,
            seats,
            control_listen: address("RACER_CONTROL_LISTEN", ":8443")?,
            webhook_listen: address("RACER_WEBHOOK_LISTEN", ":9443")?,
            object_image: get("RACER_OBJECT_IMAGE").filter(|v| !v.is_empty()),
            api_server,
            api_ca: get("RACER_API_CA").map_or_else(|| format!("{sa}/ca.crt").into(), Into::into),
            api_token: get("RACER_API_TOKEN")
                .map_or_else(|| format!("{sa}/token").into(), Into::into),
            control_token: get("RACER_CONTROL_TOKEN")
                .map_or_else(|| "/var/run/racer-token/token".into(), Into::into),
            daemonset: get("RACER_DAEMONSET")
                .filter(|v| !v.is_empty())
                .unwrap_or_else(|| "racer-dataplane".into()),
            service_account: get("RACER_SERVICE_ACCOUNT")
                .filter(|v| !v.is_empty())
                .unwrap_or_else(|| "racer-dataplane".into()),
            peer_port,
            rotation: Rotation::default(),
        };
        config.validate()?;
        Ok(Some(config))
    }

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
            || !racer_control_wire::valid_uuid(&self.pod_uid)
            || (self.control_listen.port() != 0
                && self.control_listen.port() == self.webhook_listen.port())
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn env(extra: &[(&str, &str)]) -> HashMap<String, String> {
        let mut m: HashMap<String, String> = [
            ("RACER_NAMESPACE", "racer-system"),
            ("RACER_POD_NAME", "racer-dataplane-abc"),
            ("RACER_POD_UID", "11111111-1111-4111-8111-111111111111"),
            ("RACER_NODE_NAME", "node-a"),
            ("RACER_POD_IP", "10.0.0.5"),
            ("KUBERNETES_SERVICE_HOST", "10.96.0.1"),
        ]
        .into_iter()
        .map(|(k, v)| (k.into(), v.into()))
        .collect();
        for (k, v) in extra {
            m.insert((*k).into(), (*v).into());
        }
        m
    }

    fn parse(m: &HashMap<String, String>) -> Result<Option<Config>, Error> {
        Config::from_lookup(|k| m.get(k).cloned())
    }

    #[test]
    fn absent_namespace_is_standalone() {
        assert!(Config::from_lookup(|_| None).unwrap().is_none());
    }

    #[test]
    fn defaults() {
        let c = parse(&env(&[])).unwrap().unwrap();
        assert_eq!(c.seats, SeatPolicy::Auto);
        assert_eq!(c.control_listen.port(), 8443);
        assert_eq!(c.webhook_listen.port(), 9443);
        assert_eq!(c.api_server, "10.96.0.1:443");
        assert_eq!(c.peer_port, 8082);
        assert!(c.object_image.is_none());
    }

    #[test]
    fn seats_and_ipv6_api() {
        let c = parse(&env(&[
            ("RACER_CONTROL_SEATS", "5"),
            ("KUBERNETES_SERVICE_HOST", "fd00::1"),
        ]))
        .unwrap()
        .unwrap();
        assert_eq!(c.seats, SeatPolicy::Fixed(5));
        assert_eq!(c.api_server, "[fd00::1]:443");
        for bad in ["0", "17", "05", "x"] {
            assert_eq!(
                parse(&env(&[("RACER_CONTROL_SEATS", bad)])).unwrap_err(),
                Error::InvalidConfiguration
            );
        }
    }

    #[test]
    fn managed_listeners_accept_both_pod_ip_families() {
        for ip in ["10.244.2.3", "fd00:10:244::3"] {
            let control = format!("[{ip}]:8443");
            let webhook = format!("[{ip}]:9443");
            let c = parse(&env(&[
                ("RACER_POD_IP", ip),
                ("RACER_CONTROL_LISTEN", &control),
                ("RACER_WEBHOOK_LISTEN", &webhook),
            ]))
            .unwrap()
            .unwrap();
            let ip: IpAddr = ip.parse().unwrap();
            assert_eq!(c.control_listen, SocketAddr::new(ip, 8443));
            assert_eq!(c.webhook_listen, SocketAddr::new(ip, 9443));
        }
    }

    #[test]
    fn listener_address_validation() {
        for address in ["127.0.0.1:0", "[127.0.0.1]:0", "[::1]:0", ":0"] {
            let c = parse(&env(&[("RACER_CONTROL_LISTEN", address)]))
                .unwrap()
                .unwrap();
            assert_eq!(c.control_listen.port(), 0);
        }
        for bad in [
            "[10.244.2.3:8443",
            "10.244.2.3]:8443",
            "[10.244.2.3]:+8443",
            "[10.244.2.3]:8443extra",
            "[10.244.2.3]:65536",
            "[10.244.2.3]:",
            "[not-an-ip]:8443",
            "[]:8443",
            "fd00:10:244::3:8443",
        ] {
            for name in ["RACER_CONTROL_LISTEN", "RACER_WEBHOOK_LISTEN"] {
                assert_eq!(
                    parse(&env(&[(name, bad)])).unwrap_err(),
                    Error::InvalidConfiguration,
                    "{name}={bad}"
                );
            }
        }
    }

    #[test]
    fn missing_identity_fails() {
        let mut m = env(&[]);
        m.remove("RACER_POD_UID");
        assert_eq!(parse(&m).unwrap_err(), Error::InvalidConfiguration);
        assert_eq!(
            parse(&env(&[("RACER_POD_IP", "nope")])).unwrap_err(),
            Error::InvalidConfiguration
        );
    }
}
