//! RDMA NIC JSON and the Pod report annotation.
//!
//! The NIC record codec in `racer_wire::control` is private. These helpers reuse it by
//! wrapping NICs in a one-member publication, so both sides stay identical.

use racer_wire::control::{self as wire, Member, NodeId, Publication, RailMapping};
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
        "pools": [],
        "page_bytes": racer_wire::object::PageSize::DEFAULT.get(),
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
        page_size: racer_wire::object::PageSize::DEFAULT,
        pools: vec![],
    };
    let bytes = wire::encode_publication(&p)?;
    let mut v: Value = serde_json::from_slice(&bytes).map_err(|_| wire::Error::InvalidRequest)?;
    Ok(v["members"][0]["rdma_nics"].take())
}

/// Most fills one Pod may report.
pub const MAX_REPORT_FILLS: usize = crate::limits::MAX_REPORT_FILLS;

/// Pod report annotation.
#[derive(Debug, Deserialize, Serialize)]
struct ReportDto {
    shares: u32,
    rdma_nics: Value,
    /// Per-fill progress. Optional so an older primary reads a new report.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    fills: Vec<Value>,
    /// Bytes this node may still pin on this node.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pinnable_bytes: Option<u64>,
}

/// One fill's progress in the Pod report annotation.
#[derive(Clone, Debug, Deserialize, Serialize)]
struct FillProgressDto {
    fill: String,
    generation: u64,
    digest: String,
    membership: u64,
    owned_pages: u64,
    pinned_pages: u64,
    owned_bytes: u64,
    pinned_bytes: u64,
    refused_bytes: u64,
}

/// Values a Pod reports for its own Node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Report {
    pub shares: NonZeroU32,
    pub rdma_nics: Vec<RailMapping>,
    pub fills: Vec<FillProgress>,
    pub pinnable_bytes: Option<u64>,
}

/// Progress one node reports for one fill.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FillProgress {
    /// ClusterCacheFill UID.
    pub fill: String,
    pub generation: u64,
    /// Manifest digest, 64 lowercase hex.
    pub digest: String,
    pub membership: u64,
    pub owned_pages: u64,
    pub pinned_pages: u64,
    pub owned_bytes: u64,
    pub pinned_bytes: u64,
    pub refused_bytes: u64,
}

impl FillProgress {
    /// Whether the entry fits the report bounds.
    pub fn valid(&self) -> bool {
        racer_wire::control::valid_uuid(&self.fill)
            && valid_digest(&self.digest)
            && self.pinned_pages <= self.owned_pages
            && self.pinned_bytes <= self.owned_bytes
    }
}

fn valid_digest(digest: &str) -> bool {
    digest.len() == 64
        && digest
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// Parse a report annotation.
pub fn parse_report(s: &str) -> Result<Report, wire::Error> {
    if s.len() > crate::limits::MAX_REPORT_BYTES {
        return Err(wire::Error::Overloaded);
    }
    let d: ReportDto = serde_json::from_str(s).map_err(|_| wire::Error::InvalidRequest)?;
    let mut fills = Vec::new();
    for value in d.fills.into_iter().take(MAX_REPORT_FILLS) {
        // A malformed entry must not drop the node or the other fills. Each
        // entry is decoded on its own, so an unknown future entry shape is
        // ignored rather than failing the whole report.
        let Ok(d) = serde_json::from_value::<FillProgressDto>(value) else {
            continue;
        };
        let progress = FillProgress {
            fill: d.fill,
            generation: d.generation,
            digest: d.digest,
            membership: d.membership,
            owned_pages: d.owned_pages,
            pinned_pages: d.pinned_pages,
            owned_bytes: d.owned_bytes,
            pinned_bytes: d.pinned_bytes,
            refused_bytes: d.refused_bytes,
        };
        // Bounds are checked only after a successful decode.
        if progress.valid() {
            fills.push(progress);
        }
    }
    Ok(Report {
        shares: NonZeroU32::new(d.shares).ok_or(wire::Error::InvalidRequest)?,
        rdma_nics: parse_nics(&d.rdma_nics)?,
        fills,
        pinnable_bytes: d.pinnable_bytes,
    })
}

/// Encode a report annotation.
pub fn encode_report(r: &Report) -> Result<String, wire::Error> {
    if r.fills.len() > MAX_REPORT_FILLS || r.fills.iter().any(|f| !f.valid()) {
        return Err(wire::Error::InvalidRequest);
    }
    let s = serde_json::to_string(&ReportDto {
        shares: r.shares.get(),
        rdma_nics: encode_nics(&r.rdma_nics)?,
        fills: r
            .fills
            .iter()
            .map(|f| {
                serde_json::to_value(FillProgressDto {
                    fill: f.fill.clone(),
                    generation: f.generation,
                    digest: f.digest.clone(),
                    membership: f.membership,
                    owned_pages: f.owned_pages,
                    pinned_pages: f.pinned_pages,
                    owned_bytes: f.owned_bytes,
                    pinned_bytes: f.pinned_bytes,
                    refused_bytes: f.refused_bytes,
                })
                .map_err(|_| wire::Error::InvalidRequest)
            })
            .collect::<Result<Vec<_>, _>>()?,
        pinnable_bytes: r.pinnable_bytes,
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
            fills: vec![],
            pinnable_bytes: None,
        };
        let s = encode_report(&r).unwrap();
        assert_eq!(parse_report(&s).unwrap(), r);
        assert!(parse_report(r#"{"shares":0,"rdma_nics":[]}"#).is_err());
        assert!(parse_report(r#"{"shares":1}"#).is_err());
        // Unknown fields are tolerated across a rolling upgrade.
        assert!(parse_report(r#"{"shares":1,"rdma_nics":[],"x":1}"#).is_ok());
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
            fills: vec![],
            pinnable_bytes: None,
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
            fills: vec![],
            pinnable_bytes: None,
        };
        assert_eq!(
            parse_report(&encode_report(&typical).unwrap()).unwrap(),
            typical
        );
    }

    #[test]
    fn fills_roundtrip_and_bounds() {
        let fill = "11111111-1111-4111-8111-111111111111";
        let digest = "ab".repeat(32);
        let progress = FillProgress {
            fill: fill.into(),
            generation: 3,
            digest: digest.clone(),
            membership: 17,
            owned_pages: 120,
            pinned_pages: 118,
            owned_bytes: 5_000_000,
            pinned_bytes: 4_900_000,
            refused_bytes: 0,
        };
        let r = Report {
            shares: NonZeroU32::new(8).unwrap(),
            rdma_nics: vec![],
            fills: vec![progress.clone()],
            pinnable_bytes: Some(1234),
        };
        let s = encode_report(&r).unwrap();
        assert_eq!(parse_report(&s).unwrap(), r);
        assert!(s.contains(r#""pinnable_bytes":1234"#));
        // Old reports stay byte-identical: no fills or pinnable fields.
        let old = Report {
            shares: NonZeroU32::new(8).unwrap(),
            rdma_nics: vec![],
            fills: vec![],
            pinnable_bytes: None,
        };
        assert_eq!(
            encode_report(&old).unwrap(),
            r#"{"shares":8,"rdma_nics":[]}"#
        );
        // An old primary can read a new report, and a new primary tolerates
        // fields it does not know.
        assert_eq!(parse_report(&s).unwrap().fills[0], progress);
        assert!(parse_report(r#"{"shares":1,"rdma_nics":[],"future":true}"#).is_ok());
        // Invalid entries are ignored individually, not fatal.
        let invalid = serde_json::json!({
            "shares": 1,
            "rdma_nics": [],
            "fills": [
                {"fill":"not-a-uuid","generation":1,"digest":digest,"membership":1,
                 "owned_pages":1,"pinned_pages":1,"owned_bytes":1,"pinned_bytes":1,"refused_bytes":0},
                {"fill":fill,"generation":1,"digest":"AB","membership":1,
                 "owned_pages":1,"pinned_pages":1,"owned_bytes":1,"pinned_bytes":1,"refused_bytes":0},
                {"fill":fill,"generation":1,"digest":digest,"membership":1,
                 "owned_pages":1,"pinned_pages":2,"owned_bytes":1,"pinned_bytes":1,"refused_bytes":0},
                {"fill":fill,"generation":1,"digest":digest,"membership":1,
                 "owned_pages":1,"pinned_pages":1,"owned_bytes":1,"pinned_bytes":2,"refused_bytes":0},
                {"fill":fill,"generation":1,"digest":digest,"membership":1,
                 "owned_pages":1,"pinned_pages":1,"owned_bytes":1,"pinned_bytes":1,"refused_bytes":0}
            ]
        });
        let parsed = parse_report(&invalid.to_string()).unwrap();
        assert_eq!(parsed.fills.len(), 1);
        assert_eq!(parsed.fills[0].fill, fill);
        // Encoding rejects invalid or over-count entries.
        let mut too_many = old.clone();
        too_many.fills = (0..MAX_REPORT_FILLS + 1)
            .map(|_| progress.clone())
            .collect();
        assert_eq!(encode_report(&too_many), Err(wire::Error::InvalidRequest));
        let mut bad = old;
        bad.fills = vec![FillProgress {
            owned_pages: 1,
            pinned_pages: 2,
            ..progress
        }];
        assert_eq!(encode_report(&bad), Err(wire::Error::InvalidRequest));
    }

    #[test]
    fn maximum_report_including_fills_fits() {
        let mut r = max_report();
        let digest = "ab".repeat(32);
        r.fills = (0..MAX_REPORT_FILLS)
            .map(|i| FillProgress {
                fill: format!("11111111-1111-4111-8111-{:012}", i),
                generation: u64::MAX,
                digest: digest.clone(),
                membership: u64::MAX,
                owned_pages: u64::MAX,
                pinned_pages: u64::MAX,
                owned_bytes: u64::MAX,
                pinned_bytes: u64::MAX,
                refused_bytes: u64::MAX,
            })
            .collect();
        r.pinnable_bytes = Some(u64::MAX);
        let s = encode_report(&r).unwrap();
        assert!(s.len() <= crate::limits::MAX_REPORT_BYTES, "{}", s.len());
        assert!(
            s.len()
                <= crate::limits::MAX_REPORT_INVENTORY_BYTES + crate::limits::MAX_REPORT_FILL_BYTES
        );
        assert_eq!(parse_report(&s).unwrap(), r);
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
