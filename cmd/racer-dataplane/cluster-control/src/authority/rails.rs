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
mod tests {
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
            parse_report(&" ".repeat(5000)),
            Err(wire::Error::Overloaded)
        );
    }
}
