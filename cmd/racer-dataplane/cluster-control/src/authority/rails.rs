//! RDMA NIC JSON and the Pod report annotation.
//!
//! The NIC record codec in control-wire is private. These helpers reuse it by
//! wrapping NICs in a one-member publication, so both sides stay identical.

use racer_control_wire::{self as wire, Member, NodeId, Publication, RailMapping};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::num::NonZeroU32;

const PROBE_UUID: &str = "00000000-0000-4000-8000-000000000001";

/// Parse a JSON NIC array.
pub fn parse_nics(value: &Value) -> Result<Vec<RailMapping>, wire::Error> {
    if !value.is_array() {
        return Err(wire::Error::InvalidRequest);
    }
    let doc = json!({
        "schema_version": wire::SCHEMA_VERSION,
        "cluster": PROBE_UUID,
        "sequence": "1",
        "membership_version": "1",
        "members": [{
            "node": PROBE_UUID, "shares": 1, "peer_endpoint": "192.0.2.1:1",
            "rdma_nics": value, "site": "",
        }],
        "caches": [],
    });
    let bytes = serde_json::to_vec(&doc).map_err(|_| wire::Error::InvalidRequest)?;
    let mut p = wire::decode_publication(&bytes)?;
    Ok(std::mem::take(&mut p.members[0].rails))
}

/// Parse a NIC annotation string.
pub fn parse_nics_str(s: &str) -> Result<Vec<RailMapping>, wire::Error> {
    let v: Value = serde_json::from_str(s).map_err(|_| wire::Error::InvalidRequest)?;
    parse_nics(&v)
}

/// Encode NICs in canonical order.
pub fn encode_nics(nics: &[RailMapping]) -> Result<Value, wire::Error> {
    let p = Publication {
        schema_version: wire::SCHEMA_VERSION,
        cluster: wire::ClusterId(PROBE_UUID.into()),
        sequence: wire::PublicationSequence(1),
        membership_version: wire::MembershipVersion(1),
        members: vec![Member {
            node: NodeId(PROBE_UUID.into()),
            shares: NonZeroU32::MIN,
            peer_endpoint: "192.0.2.1:1".into(),
            rails: nics.to_vec(),
            site: String::new(),
        }],
        caches: vec![],
    };
    let bytes = wire::encode_publication(&p)?;
    let mut v: Value = serde_json::from_slice(&bytes).map_err(|_| wire::Error::InvalidRequest)?;
    Ok(v["members"][0]["rdma_nics"].take())
}

/// Pod report annotation.
#[derive(Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ReportDto {
    shares: u32,
    rdma_nics: Value,
}

/// Values a Pod reports for its own Node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Report {
    pub shares: NonZeroU32,
    pub rdma_nics: Vec<RailMapping>,
}

/// Parse a report annotation.
pub fn parse_report(s: &str) -> Result<Report, wire::Error> {
    if s.len() > crate::limits::MAX_REPORT_BYTES {
        return Err(wire::Error::Overloaded);
    }
    let d: ReportDto = serde_json::from_str(s).map_err(|_| wire::Error::InvalidRequest)?;
    Ok(Report {
        shares: NonZeroU32::new(d.shares).ok_or(wire::Error::InvalidRequest)?,
        rdma_nics: parse_nics(&d.rdma_nics)?,
    })
}

/// Encode a report annotation.
pub fn encode_report(r: &Report) -> Result<String, wire::Error> {
    let s = serde_json::to_string(&ReportDto {
        shares: r.shares.get(),
        rdma_nics: encode_nics(&r.rdma_nics)?,
    })
    .map_err(|_| wire::Error::InvalidRequest)?;
    if s.len() > crate::limits::MAX_REPORT_BYTES {
        return Err(wire::Error::Overloaded);
    }
    Ok(s)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    #[test]
    fn nics_roundtrip_and_reject() {
        let v = json!([{"device":"mlx5_1","port":1,"rail":2},{"device":"mlx5_0","port":1,"rail":1,"numa_node":0}]);
        let nics = parse_nics(&v).unwrap();
        assert_eq!(nics.len(), 2);
        let back = encode_nics(&nics).unwrap();
        assert_eq!(back[0]["device"], "mlx5_0");
        assert_eq!(parse_nics(&back).unwrap(), nics);
        assert!(parse_nics(&json!({})).is_err());
        assert!(parse_nics(&json!([{"device":"x","port":1,"rail":1,"extra":1}])).is_err());
        assert!(parse_nics_str("[").is_err());
        assert!(parse_nics_str("[]").unwrap().is_empty());
    }

    #[test]
    fn report_roundtrip_and_limits() {
        let r = Report {
            shares: NonZeroU32::new(8).unwrap(),
            rdma_nics: parse_nics(&json!([{"device":"d","port":1,"rail":0}])).unwrap(),
        };
        let s = encode_report(&r).unwrap();
        assert_eq!(parse_report(&s).unwrap(), r);
        assert!(parse_report(r#"{"shares":0,"rdma_nics":[]}"#).is_err());
        assert!(parse_report(r#"{"shares":1}"#).is_err());
        assert!(parse_report(r#"{"shares":1,"rdma_nics":[],"x":1}"#).is_err());
        assert_eq!(
            parse_report(&" ".repeat(crate::limits::MAX_REPORT_BYTES + 1)),
            Err(wire::Error::Overloaded)
        );
    }

    /// Widest valid NIC: the longest discoverable device name of characters
    /// JSON escapes, and every field at its widest. Ports keep them distinct.
    fn widest_nic(i: usize) -> RailMapping {
        RailMapping {
            device: "\u{1}".repeat(crate::limits::MAX_REPORT_DEVICE_BYTES),
            port: 192 + i as u8,
            rail: wire::RailId(u16::MAX),
            gid: Some([0xab; 16]),
            numa_node: Some(u32::MAX as usize),
        }
    }

    pub(crate) fn max_report() -> Report {
        Report {
            shares: NonZeroU32::MAX,
            rdma_nics: (0..64).map(widest_nic).collect(),
        }
    }

    #[test]
    fn maximum_inventory_fits_report() {
        let r = max_report();
        let s = encode_report(&r).unwrap();
        // The derived bound is exact for the worst case and fits the limit.
        assert_eq!(s.len(), crate::limits::MAX_REPORT_INVENTORY_BYTES);
        assert!(s.len() <= crate::limits::MAX_REPORT_BYTES);
        assert_eq!(parse_report(&s).unwrap(), r);
        // A typical 64-port inventory, the case the old 4 KiB limit rejected.
        let typical = Report {
            shares: NonZeroU32::new(8).unwrap(),
            rdma_nics: (0..64)
                .map(|i| RailMapping {
                    device: format!("mlx5_{i}"),
                    port: 1,
                    rail: wire::RailId(i),
                    gid: Some([0xfe; 16]),
                    numa_node: Some(1),
                })
                .collect(),
        };
        assert_eq!(
            parse_report(&encode_report(&typical).unwrap()).unwrap(),
            typical
        );
    }

    #[test]
    fn oversize_overcount_and_invalid_reports_rejected() {
        // Over count: the wire allows at most 64 NICs.
        let mut r = max_report();
        r.rdma_nics.truncate(63);
        r.rdma_nics.push(RailMapping {
            device: "x".into(),
            port: 1,
            rail: wire::RailId(0),
            gid: None,
            numa_node: None,
        });
        assert!(encode_report(&r).is_ok());
        r.rdma_nics.push(RailMapping {
            device: "y".into(),
            port: 1,
            rail: wire::RailId(0),
            gid: None,
            numa_node: None,
        });
        assert_eq!(encode_report(&r), Err(wire::Error::Overloaded));
        let mut v: Value = serde_json::from_str(&encode_report(&max_report()).unwrap()).unwrap();
        v["rdma_nics"]
            .as_array_mut()
            .unwrap()
            .push(json!({"device":"z","port":1,"rail":0}));
        assert_eq!(parse_report(&v.to_string()), Err(wire::Error::Overloaded));
        // Oversize: a device name past the bound may push a report over the limit.
        let mut r = max_report();
        r.rdma_nics[0]
            .device
            .push_str(&"\u{1}".repeat(crate::limits::MAX_REPORT_BYTES));
        assert_eq!(encode_report(&r), Err(wire::Error::Overloaded));
        let padded = format!(
            "{}{}",
            encode_report(&max_report()).unwrap(),
            " ".repeat(
                crate::limits::MAX_REPORT_BYTES + 1 - crate::limits::MAX_REPORT_INVENTORY_BYTES,
            )
        );
        assert_eq!(parse_report(&padded), Err(wire::Error::Overloaded));
        // The limit counts UTF-8 bytes, like the admission policy: two-byte
        // characters reach it at half the code points.
        let max = crate::limits::MAX_REPORT_BYTES;
        let at = "\u{e9}".repeat(max / 2);
        assert_eq!(at.len(), max);
        assert_eq!(parse_report(&at), Err(wire::Error::InvalidRequest));
        assert_eq!(
            parse_report(&format!("{at}r")),
            Err(wire::Error::Overloaded)
        );
        // Invalid NICs remain rejected at any size.
        let mut r = max_report();
        r.rdma_nics[1].port = r.rdma_nics[0].port;
        assert!(encode_report(&r).is_err());
        for bad in [
            r#"{"shares":1,"rdma_nics":[{"device":"","port":1,"rail":0}]}"#,
            r#"{"shares":1,"rdma_nics":[{"device":"d","port":0,"rail":0}]}"#,
            r#"{"shares":1,"rdma_nics":[{"device":"d\n","port":1,"rail":0}]}"#,
            r#"{"shares":1,"rdma_nics":[{"device":"d","port":1,"rail":0,"gid":"ab"}]}"#,
            r#"{"shares":4294967296,"rdma_nics":[]}"#,
        ] {
            assert_eq!(parse_report(bad), Err(wire::Error::InvalidRequest), "{bad}");
        }
    }
}
